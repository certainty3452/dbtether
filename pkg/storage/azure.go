package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// AzureClient provides Azure Blob Storage operations
type AzureClient struct {
	client    *azblob.Client
	container string
	logger    *slog.Logger
}

// AzureConfig contains configuration for Azure Blob Storage client
type AzureConfig struct {
	Container      string
	StorageAccount string
	// Credentials via:
	// - Azure Managed Identity (on AKS with Workload Identity)
	// - AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_CLIENT_SECRET env vars
	// - Azure CLI credentials (local dev)
}

// NewAzureClient creates a new Azure Blob Storage client
func NewAzureClient(ctx context.Context, cfg *AzureConfig, logger *slog.Logger) (*AzureClient, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Uses DefaultAzureCredential which tries:
	// 1. Environment credentials (AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_CLIENT_SECRET)
	// 2. Managed Identity (on Azure VMs, AKS, etc.)
	// 3. Azure CLI credentials
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get Azure credentials: %w", err)
	}

	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.StorageAccount)
	client, err := azblob.NewClient(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure Blob client: %w", err)
	}

	return &AzureClient{
		client:    client,
		container: cfg.Container,
		logger:    logger,
	}, nil
}

// Upload uploads data to Azure Blob Storage
func (c *AzureClient) Upload(ctx context.Context, key string, data io.Reader) error {
	// Read all data (Azure SDK requires seekable reader or bytes)
	dataBytes, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("failed to read data: %w", err)
	}

	_, err = c.client.UploadBuffer(ctx, c.container, key, dataBytes, nil)
	if err != nil {
		return fmt.Errorf("failed to upload to Azure Blob: %w", err)
	}
	return nil
}

// UploadWithTags uploads data with metadata
func (c *AzureClient) UploadWithTags(ctx context.Context, key string, data io.Reader, tags *ObjectTags) error {
	dataBytes, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("failed to read data: %w", err)
	}

	opts := &azblob.UploadBufferOptions{}
	if tags != nil {
		opts.Metadata = map[string]*string{
			"database":   strPtr(tags.Database),
			"cluster":    strPtr(tags.Cluster),
			"backupname": strPtr(tags.BackupName), // Azure metadata keys can't have hyphens
			"namespace":  strPtr(tags.Namespace),
			"timestamp":  strPtr(tags.Timestamp),
			"createdby":  strPtr(tags.CreatedBy),
		}
	}

	_, err = c.client.UploadBuffer(ctx, c.container, key, dataBytes, opts)
	if err != nil {
		return fmt.Errorf("failed to upload to Azure Blob: %w", err)
	}
	return nil
}

func strPtr(s string) *string {
	return &s
}

// Download downloads data from Azure Blob Storage
func (c *AzureClient) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := c.client.DownloadStream(ctx, c.container, key, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to download from Azure Blob: %w", err)
	}
	return resp.Body, nil
}

// Delete deletes a blob from Azure Blob Storage
func (c *AzureClient) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteBlob(ctx, c.container, key, nil)
	if err != nil {
		return fmt.Errorf("failed to delete from Azure Blob: %w", err)
	}
	return nil
}

// AzureObject represents an object in Azure Blob Storage
type AzureObject struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// List lists all blobs with the given prefix
func (c *AzureClient) List(ctx context.Context, prefix string) ([]AzureObject, error) {
	var objects []AzureObject

	pager := c.client.NewListBlobsFlatPager(c.container, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix,
	})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list Azure blobs: %w", err)
		}

		for _, blob := range page.Segment.BlobItems {
			lastMod := time.Time{}
			if blob.Properties.LastModified != nil {
				lastMod = *blob.Properties.LastModified
			}
			size := int64(0)
			if blob.Properties.ContentLength != nil {
				size = *blob.Properties.ContentLength
			}

			objects = append(objects, AzureObject{
				Key:          *blob.Name,
				Size:         size,
				LastModified: lastMod,
			})
		}
	}

	return objects, nil
}
