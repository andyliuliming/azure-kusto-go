// Package filesystem provides a client with the ability to import data into Kusto via a variety of fileystems
// such as local storage or blobstore.
package queued

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/andyliuliming/azure-kusto-go/kusto/data/errors"
	"github.com/andyliuliming/azure-kusto-go/kusto/ingest/internal/gzip"
	"github.com/andyliuliming/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/andyliuliming/azure-kusto-go/kusto/ingest/internal/resources"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-storage-queue-go/azqueue"
)

const (
	_1MiB = 1024 * 1024

	// The numbers below are magic numbers. They were derived from doing Azure to Azure tests of azcopy for various file sizes
	// to prove that changes weren't going to make azcopy slower. It was found that multiplying azcopy's concurrency by 10x (to 50)
	// made a 5x improvement in speed. We don't have any numbers from the service side to give us numbers we should use, so this
	// is our best guess from observation. DO NOT CHANGE UNLESS YOU KNOW BETTER.

	BlockSize   = 8 * _1MiB
	Concurrency = 50
)

// Queued provides methods for taking data from various sources and ingesting it into Kusto using queued ingestion.
type Queued interface {
	io.Closer
	Local(ctx context.Context, from string, props properties.All) error
	Reader(ctx context.Context, reader io.Reader, props properties.All) (string, error)
	Blob(ctx context.Context, from string, fileSize int64, props properties.All) error
}

// uploadStream provides a type that mimics azblob.UploadStreamToBlockBlob to allow fakes for testing.
type uploadStream func(context.Context, string, string, io.Reader, *azblob.Client, *azblob.UploadStreamOptions) (azblob.UploadStreamResponse, error)

// uploadBlob provides a type that mimics azblob.UploadFileToBlockBlob to allow fakes for test
type uploadBlob func(context.Context, string, string, *os.File, *azblob.Client, *azblob.UploadFileOptions) (azblob.UploadFileResponse, error)

// Ingestion provides methods for taking data from a filesystem of some type and ingesting it into Kusto.
// This object is scoped for a single database and table.
type Ingestion struct {
	db    string
	table string
	mgr   *resources.Manager

	uploadStream uploadStream
	uploadBlob   uploadBlob

	bufferSize int
	maxBuffers int
}

// Option is an optional argument to New().
type Option func(s *Ingestion)

// WithStaticBuffer sets a static buffer with a buffer size and max amount of buffers for uploading blobs to kusto.
func WithStaticBuffer(bufferSize int, maxBuffers int) Option {
	return func(s *Ingestion) {
		s.bufferSize = bufferSize
		s.maxBuffers = maxBuffers
	}
}

// New is the constructor for Ingestion.
func New(db, table string, mgr *resources.Manager, options ...Option) (*Ingestion, error) {
	i := &Ingestion{
		db:    db,
		table: table,
		mgr:   mgr,
		uploadStream: func(ctx context.Context, container string, blobName string, reader io.Reader, client *azblob.Client, options *azblob.UploadStreamOptions) (azblob.UploadStreamResponse, error) {
			return client.UploadStream(ctx, container, blobName, reader, options)
		},
		uploadBlob: func(ctx context.Context, containerName string, blobName string, file *os.File, client *azblob.Client, options *azblob.UploadFileOptions) (azblob.UploadFileResponse, error) {
			return client.UploadFile(ctx, containerName, blobName, file, options)
		},
	}

	for _, opt := range options {
		opt(i)
	}

	return i, nil
}

// Local ingests a local file into Kusto.
func (i *Ingestion) Local(ctx context.Context, from string, props properties.All) error {
	blobClient, container, err := i.upstreamContainer()
	if err != nil {
		return err
	}

	mgrResources, err := i.mgr.Resources()
	if err != nil {
		return err
	}

	// We want to check the queue size here so so we don't upload a file and then find we don't have a Kusto queue to stick
	// it in. If we don't have a container, that is handled by containerQueue().
	if len(mgrResources.Queues) == 0 {
		return errors.ES(errors.OpFileIngest, errors.KBlobstore, "no Kusto queue resources are defined, there is no queue to upload to").SetNoRetry()
	}

	blobURL, size, err := i.localToBlob(ctx, from, blobClient, container, &props)
	if err != nil {
		return err
	}

	if err := i.Blob(ctx, blobURL, size, props); err != nil {
		return err
	}

	return nil
}

// Reader uploads a file via an io.Reader.
// If the function succeeds, it returns the path of the created blob.
func (i *Ingestion) Reader(ctx context.Context, reader io.Reader, props properties.All) (string, error) {
	blobClient, container, err := i.upstreamContainer()
	if err != nil {
		return "", err
	}

	mgrResources, err := i.mgr.Resources()
	if err != nil {
		return "", err
	}

	// We want to check the queue size here so so we don't upload a file and then find we don't have a Kusto queue to stick
	// it in. If we don't have a container, that is handled by containerQueue().
	if len(mgrResources.Queues) == 0 {
		return "", errors.ES(errors.OpFileIngest, errors.KBlobstore, "no Kusto queue resources are defined, there is no queue to upload to").SetNoRetry()
	}

	shouldCompress := true
	if props.Source.OriginalSource != "" {
		shouldCompress = CompressionDiscovery(props.Source.OriginalSource) == properties.CTNone
	}
	if props.Source.DontCompress {
		shouldCompress = false
	}

	extension := "gz"
	if !shouldCompress {
		if props.Source.OriginalSource != "" {
			extension = filepath.Ext(props.Source.OriginalSource)
		} else {
			extension = props.Ingestion.Additional.Format.String() // Best effort
		}
	}

	blobName := fmt.Sprintf("%s_%s_%s_%s.%s", i.db, i.table, nower(), filepath.Base(uuid.New().String()), extension)

	// Here's how to upload a blob.
	// blobClient, _ := to.NewBlockBlobClient(blobName)

	size := int64(0)

	if shouldCompress {
		reader = gzip.Compress(reader)
	}

	_, err = i.uploadStream(
		ctx,
		container,
		blobName,
		reader,
		blobClient,
		&azblob.UploadStreamOptions{},
	)

	if err != nil {
		return blobName, errors.ES(errors.OpFileIngest, errors.KBlobstore, "problem uploading to Blob Storage: %s", err)
	}

	if gz, ok := reader.(*gzip.Streamer); ok {
		size = gz.InputSize()
	}

	if err := i.Blob(ctx, blobClient.URL(), size, props); err != nil {
		return blobName, err
	}

	return blobName, nil
}

// Blob ingests a file from Azure Blob Storage into Kusto.
func (i *Ingestion) Blob(ctx context.Context, from string, fileSize int64, props properties.All) error {
	// To learn more about ingestion properties, go to:
	// https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/#ingestion-properties
	// To learn more about ingestion methods go to:
	// https://docs.microsoft.com/en-us/azure/data-explorer/ingest-data-overview#ingestion-methods

	to, err := i.upstreamQueue()
	if err != nil {
		return err
	}

	props.Ingestion.BlobPath = from
	if fileSize != 0 {
		props.Ingestion.RawDataSize = fileSize
	}

	props.Ingestion.RetainBlobOnSuccess = !props.Source.DeleteLocalSource

	err = CompleteFormatFromFileName(&props, from)
	if err != nil {
		return err
	}

	j, err := props.Ingestion.MarshalJSONString()
	if err != nil {
		return errors.ES(errors.OpFileIngest, errors.KInternal, "could not marshal the ingestion blob info: %s", err).SetNoRetry()
	}

	if _, err := to.Enqueue(ctx, j, 0, 0); err != nil {
		return errors.E(errors.OpFileIngest, errors.KBlobstore, err)
	}

	err = props.ApplyDeleteLocalSourceOption()
	if err != nil {
		return err
	}

	return nil
}

func CompleteFormatFromFileName(props *properties.All, from string) error {
	// If they did not tell us how the file was encoded, try to discover it from the file extension.
	if props.Ingestion.Additional.Format != properties.DFUnknown {
		return nil
	}

	et := properties.DataFormatDiscovery(from)
	if et == properties.DFUnknown {
		// If we can't figure out the file type, default to CSV.
		et = properties.CSV
	}
	props.Ingestion.Additional.Format = et

	return nil
}

// upstreamContainer randomly selects a container queue in which to upload our file to blobstore.
func (i *Ingestion) upstreamContainer() (*azblob.Client, string, error) {
	mgrResources, err := i.mgr.Resources()
	if err != nil {
		return nil, "", errors.E(errors.OpFileIngest, errors.KBlobstore, err)
	}

	if len(mgrResources.Containers) == 0 {
		return nil, "", errors.ES(
			errors.OpFileIngest,
			errors.KBlobstore,
			"no Blob Storage container resources are defined, there is no container to upload to",
		).SetNoRetry()
	}

	storageURI := mgrResources.Containers[rand.Intn(len(mgrResources.Containers))]
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net?%s", storageURI.Account(), storageURI.SAS().Encode())

	service, err := azblob.NewClientWithNoCredential(serviceURL, nil)
	if err != nil {
		return nil, "", errors.E(errors.OpFileIngest, errors.KBlobstore, err)
	}

	return service, storageURI.ObjectName(), nil
}

func (i *Ingestion) upstreamQueue() (azqueue.MessagesURL, error) {
	mgrResources, err := i.mgr.Resources()
	if err != nil {
		return azqueue.MessagesURL{}, err
	}

	if len(mgrResources.Queues) == 0 {
		return azqueue.MessagesURL{}, errors.ES(
			errors.OpFileIngest,
			errors.KBlobstore,
			"no Kusto queue resources are defined, there is no queue to upload to",
		).SetNoRetry()
	}

	queue := mgrResources.Queues[rand.Intn(len(mgrResources.Queues))]
	service, _ := url.Parse(fmt.Sprintf("https://%s.queue.core.windows.net?%s", queue.Account(), queue.SAS().Encode()))

	creds := azqueue.NewAnonymousCredential()
	p := azqueue.NewPipeline(creds, azqueue.PipelineOptions{})

	return azqueue.NewServiceURL(*service, p).NewQueueURL(queue.ObjectName()).NewMessagesURL(), nil
}

var nower = time.Now

// localToBlob copies from a local to to an Azure Blobstore blob. It returns the URL of the Blob, the local file info and an
// error if there was one.
func (i *Ingestion) localToBlob(ctx context.Context, from string, blobClient *azblob.Client, container string, props *properties.All) (string, int64, error) {
	compression := CompressionDiscovery(from)
	blobName := fmt.Sprintf("%s_%s_%s_%s_%s", i.db, i.table, nower(), filepath.Base(uuid.New().String()), filepath.Base(from))
	if compression == properties.CTNone {
		blobName = blobName + ".gz"
	}

	// Here's how to upload a blob.
	// blobClient, _ := container.NewBlockBlobClient(blobName)

	file, err := os.Open(from)
	if err != nil {
		return "", 0, errors.ES(
			errors.OpFileIngest,
			errors.KLocalFileSystem,
			"problem retrieving source file %q: %s", from, err,
		).SetNoRetry()
	}

	stat, err := file.Stat()
	if err != nil {
		return "", 0, errors.ES(
			errors.OpFileIngest,
			errors.KLocalFileSystem,
			"could not Stat the file(%s): %s", from, err,
		).SetNoRetry()
	}

	if compression == properties.CTNone && !props.Source.DontCompress {
		gstream := gzip.New()
		gstream.Reset(file)

		_, err = i.uploadStream(
			ctx,
			container,
			blobName,
			gstream,
			blobClient,
			&azblob.UploadStreamOptions{},
		)

		if err != nil {
			return "", 0, errors.ES(errors.OpFileIngest, errors.KBlobstore, "problem uploading to Blob Storage: %s", err)
		}
		return blobClient.URL(), gstream.InputSize(), nil
	}

	// The high-level API UploadFileToBlockBlob function uploads blocks in parallel for optimal performance, and can handle large files as well.
	// This function calls StageBlock/CommitBlockList for files larger 256 MBs, and calls Upload for any file smaller
	_, err = i.uploadBlob(
		ctx,
		container,
		blobName,
		file,
		blobClient,
		&azblob.UploadFileOptions{
			BlockSize:   BlockSize,
			Concurrency: Concurrency,
		},
	)

	if err != nil {
		return "", 0, errors.ES(errors.OpFileIngest, errors.KBlobstore, "problem uploading to Blob Storage: %s", err)
	}

	return blobClient.URL(), stat.Size(), nil
}

// CompressionDiscovery looks at the file extension. If it is one we support, we return that
// CompressionType that represents that value. Otherwise we return CTNone to indicate that the
// file should not be compressed.
func CompressionDiscovery(fName string) properties.CompressionType {
	var ext string
	if strings.HasPrefix(strings.ToLower(fName), "http") {
		ext = strings.ToLower(filepath.Ext(path.Base(fName)))
	} else {
		ext = strings.ToLower(filepath.Ext(fName))
	}

	switch ext {
	case ".gz":
		return properties.GZIP
	case ".zip":
		return properties.ZIP
	}
	return properties.CTNone
}

// This allows mocking the stat func later on
var statFunc = os.Stat

// IsLocalPath detects whether a path points to a file system accessiable file
// If this file requires another protocol http protocol it will return false
// If the file requires another protocol(ftp, https, etc) it will return an error
func IsLocalPath(s string) (bool, error) {
	u, err := url.Parse(s)
	if err == nil {
		switch u.Scheme {
		// With this we know it SHOULD be a blobstore path.  It might not be, but I think that is a fine assumption to make.
		case "http", "https":
			return false, nil
		}
	}

	// By this point, we know its not blobstore, so it needs to be something that gets resolved to a file.
	// So we are going to Stat() the file and see if it exists and is not a directory.
	// In your tests, this would fail "file://" which we don't support.  Also, because of this method, your tests
	// are going to be broken.   Again, fileystems, blah....
	stat, err := statFunc(s)
	if err != nil {
		return false, fmt.Errorf("It is not a valid local file path (could not stat file) and not a valid blob path")
	}

	if stat.IsDir() {
		return false, fmt.Errorf("path is a local directory and not a valid file")
	}

	return true, nil
}

func (i *Ingestion) Close() error {
	i.mgr.Close()
	return nil
}
