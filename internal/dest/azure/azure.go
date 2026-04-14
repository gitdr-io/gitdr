// Package azure implements the create-only Destination for Azure Blob Storage. WORM is
// a container immutability policy (version-level immutability); writes are create-only
// via If-None-Match. Auth uses DefaultAzureCredential. There is no delete path.
package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"gitdr.io/gitdr/internal/dest"
)

// Options configures the Azure Blob backend. Credentials come from DefaultAzureCredential
// (real Azure) or a connection string (emulator / shared key).
type Options struct {
	Account          string // storage account (real Azure)
	Container        string
	Endpoint         string // override service URL; default https://<account>.blob.core.windows.net/
	ConnectionString string // for Azurite or shared-key auth
}

// Backend is a create-only Azure Blob Destination.
type Backend struct {
	client    *azblob.Client
	container string
	logger    *slog.Logger
}

var _ dest.Destination = (*Backend)(nil)

// New builds an Azure Blob backend.
func New(_ context.Context, opts Options, logger *slog.Logger) (*Backend, error) {
	if strings.TrimSpace(opts.Container) == "" {
		return nil, errors.New("azure: container is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var client *azblob.Client
	var err error
	if opts.ConnectionString != "" {
		client, err = azblob.NewClientFromConnectionString(opts.ConnectionString, nil)
	} else {
		cred, cerr := azidentity.NewDefaultAzureCredential(nil)
		if cerr != nil {
			return nil, fmt.Errorf("azure: default credential: %w", cerr)
		}
		url := opts.Endpoint
		if url == "" {
			if opts.Account == "" {
				return nil, errors.New("azure: account or endpoint is required")
			}
			url = fmt.Sprintf("https://%s.blob.core.windows.net/", opts.Account)
		}
		client, err = azblob.NewClient(url, cred, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("azure: new client: %w", err)
	}
	return &Backend{client: client, container: opts.Container, logger: logger}, nil
}

// VerifyWorm checks the container has version-level immutability enabled. (Full
// policy-lock state needs the management plane; this is the data-plane signal.)
func (b *Backend) VerifyWorm(ctx context.Context) (dest.WormStatus, error) {
	cc := b.client.ServiceClient().NewContainerClient(b.container)
	props, err := cc.GetProperties(ctx, nil)
	if err != nil {
		return dest.WormStatus{}, fmt.Errorf("azure: container properties: %w", err)
	}
	enabled := props.IsImmutableStorageWithVersioningEnabled != nil && *props.IsImmutableStorageWithVersioningEnabled
	st := dest.WormStatus{Enabled: enabled, Locked: enabled, Mode: "IMMUTABILITY", Details: "version-level immutability"}
	if !enabled {
		st.Details = "no version-level immutability on container"
	}
	return st, nil
}

// PutImmutable uploads key create-only (If-None-Match: *); immutability is enforced by
// the container's immutability policy. Never overwrites, never deletes.
func (b *Backend) PutImmutable(ctx context.Context, key string, r io.Reader, _ int64, _ dest.Retention) (dest.PutResult, error) {
	etagAny := azcore.ETagAny
	_, err := b.client.UploadStream(ctx, b.container, key, r, &azblob.UploadStreamOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &etagAny},
		},
	})
	if err != nil {
		return dest.PutResult{}, fmt.Errorf("azure: put %q: %w", key, err)
	}
	return dest.PutResult{Key: key}, nil
}

// List returns blobs under prefix (read-only).
func (b *Backend) List(ctx context.Context, prefix string) ([]dest.Object, error) {
	var out []dest.Object
	pager := b.client.NewListBlobsFlatPager(b.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: list %q: %w", prefix, err)
		}
		for _, item := range page.Segment.BlobItems {
			o := dest.Object{Key: *item.Name}
			if item.Properties != nil && item.Properties.ContentLength != nil {
				o.Size = *item.Properties.ContentLength
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// Get opens key for reading (read-only). Caller closes the reader.
func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := b.client.DownloadStream(ctx, b.container, key, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: get %q: %w", key, err)
	}
	return resp.Body, nil
}
