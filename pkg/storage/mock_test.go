package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockClient_NewMockClient(t *testing.T) {
	client := NewMockClient()
	assert.NotNil(t, client)
	assert.NotNil(t, client.objects)
	assert.Equal(t, 0, client.Count())
}

func TestMockClient_Upload(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	data := []byte("test data")
	err := client.Upload(ctx, "test/file.txt", bytes.NewReader(data))
	require.NoError(t, err)

	assert.Equal(t, 1, client.Count())

	retrieved, ok := client.GetObject("test/file.txt")
	assert.True(t, ok)
	assert.Equal(t, data, retrieved)
}

func TestMockClient_UploadWithTags(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	data := []byte("tagged data")
	tags := &ObjectTags{
		Database: "mydb",
		Cluster:  "mycluster",
	}

	err := client.UploadWithTags(ctx, "tagged/file.txt", bytes.NewReader(data), tags)
	require.NoError(t, err)

	retrievedTags, ok := client.GetTags("tagged/file.txt")
	assert.True(t, ok)
	assert.Equal(t, "mydb", retrievedTags.Database)
	assert.Equal(t, "mycluster", retrievedTags.Cluster)
}

func TestMockClient_Upload_Error(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()
	client.UploadError = errors.New("upload failed")

	err := client.Upload(ctx, "test/file.txt", bytes.NewReader([]byte("data")))
	require.Error(t, err)
	assert.Equal(t, "upload failed", err.Error())
}

func TestMockClient_Download(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	data := []byte("download test")
	client.AddObject("download/file.txt", data, time.Now())

	reader, err := client.Download(ctx, "download/file.txt")
	require.NoError(t, err)
	defer reader.Close()

	downloaded, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, data, downloaded)
}

func TestMockClient_Download_NotFound(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	_, err := client.Download(ctx, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "object not found")
}

func TestMockClient_Download_Error(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()
	client.DownloadError = errors.New("download failed")

	client.AddObject("test.txt", []byte("data"), time.Now())
	_, err := client.Download(ctx, "test.txt")
	require.Error(t, err)
	assert.Equal(t, "download failed", err.Error())
}

func TestMockClient_Exists(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	client.AddObject("exists.txt", []byte("data"), time.Now())

	exists, err := client.Exists(ctx, "exists.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = client.Exists(ctx, "nonexistent.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestMockClient_Exists_Error(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()
	client.ExistsError = errors.New("exists failed")

	_, err := client.Exists(ctx, "test.txt")
	require.Error(t, err)
	assert.Equal(t, "exists failed", err.Error())
}

func TestMockClient_Delete(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	client.AddObject("delete-me.txt", []byte("data"), time.Now())
	assert.Equal(t, 1, client.Count())

	err := client.Delete(ctx, "delete-me.txt")
	require.NoError(t, err)
	assert.Equal(t, 0, client.Count())
}

func TestMockClient_Delete_Error(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()
	client.DeleteError = errors.New("delete failed")

	err := client.Delete(ctx, "test.txt")
	require.Error(t, err)
	assert.Equal(t, "delete failed", err.Error())
}

func TestMockClient_List(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	now := time.Now()
	client.AddObject("prefix/file1.txt", []byte("1"), now)
	client.AddObject("prefix/file2.txt", []byte("22"), now)
	client.AddObject("other/file3.txt", []byte("333"), now)

	// List with prefix
	objects, err := client.List(ctx, "prefix/")
	require.NoError(t, err)
	assert.Len(t, objects, 2)

	// List all
	objects, err = client.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, objects, 3)
}

func TestMockClient_List_Error(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()
	client.ListError = errors.New("list failed")

	_, err := client.List(ctx, "")
	require.Error(t, err)
	assert.Equal(t, "list failed", err.Error())
}

func TestMockClient_AddObject(t *testing.T) {
	client := NewMockClient()

	customTime := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	client.AddObject("custom.txt", []byte("custom"), customTime)

	objects, _ := client.List(context.Background(), "")
	assert.Len(t, objects, 1)
	assert.Equal(t, customTime, objects[0].LastModified)
}

func TestMockClient_GetObject_NotFound(t *testing.T) {
	client := NewMockClient()

	_, ok := client.GetObject("nonexistent")
	assert.False(t, ok)
}

func TestMockClient_GetTags_NotFound(t *testing.T) {
	client := NewMockClient()

	_, ok := client.GetTags("nonexistent")
	assert.False(t, ok)
}

func TestMockClient_Clear(t *testing.T) {
	client := NewMockClient()

	client.AddObject("file1.txt", []byte("1"), time.Now())
	client.AddObject("file2.txt", []byte("2"), time.Now())
	assert.Equal(t, 2, client.Count())

	client.Clear()
	assert.Equal(t, 0, client.Count())
}

func TestMockClient_Keys(t *testing.T) {
	client := NewMockClient()

	client.AddObject("a.txt", []byte("a"), time.Now())
	client.AddObject("b.txt", []byte("b"), time.Now())

	keys := client.Keys()
	assert.Len(t, keys, 2)
	assert.Contains(t, keys, "a.txt")
	assert.Contains(t, keys, "b.txt")
}

func TestMockClient_Implements_StorageClient(t *testing.T) {
	// Compile-time check
	var _ StorageClient = (*MockClient)(nil)
}

func TestMockClient_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	// Concurrent writes
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			key := "concurrent/" + string(rune('a'+idx)) + ".txt"
			_ = client.Upload(ctx, key, bytes.NewReader([]byte("data")))
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	assert.Equal(t, 10, client.Count())
}

