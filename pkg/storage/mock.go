package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// MockClient is an in-memory storage client for testing
type MockClient struct {
	mu      sync.RWMutex
	objects map[string]*mockObject

	// Error injection for testing error handling
	UploadError   error
	DownloadError error
	DeleteError   error
	ListError     error
	ExistsError   error
}

type mockObject struct {
	data         []byte
	tags         *ObjectTags
	lastModified time.Time
}

// NewMockClient creates a new in-memory mock storage client
func NewMockClient() *MockClient {
	return &MockClient{
		objects: make(map[string]*mockObject),
	}
}

func (m *MockClient) Upload(ctx context.Context, key string, body io.Reader) error {
	return m.UploadWithTags(ctx, key, body, nil)
}

func (m *MockClient) UploadWithTags(ctx context.Context, key string, body io.Reader, tags *ObjectTags) error {
	if m.UploadError != nil {
		return m.UploadError
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("failed to read body: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects[key] = &mockObject{
		data:         data,
		tags:         tags,
		lastModified: time.Now(),
	}

	return nil
}

func (m *MockClient) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	if m.DownloadError != nil {
		return nil, m.DownloadError
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", key)
	}

	return io.NopCloser(bytes.NewReader(obj.data)), nil
}

func (m *MockClient) Exists(ctx context.Context, key string) (bool, error) {
	if m.ExistsError != nil {
		return false, m.ExistsError
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	_, ok := m.objects[key]
	return ok, nil
}

func (m *MockClient) Delete(ctx context.Context, key string) error {
	if m.DeleteError != nil {
		return m.DeleteError
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.objects, key)
	return nil
}

func (m *MockClient) List(ctx context.Context, prefix string) ([]StorageObject, error) {
	if m.ListError != nil {
		return nil, m.ListError
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var objects []StorageObject
	for key, obj := range m.objects {
		if len(prefix) == 0 || len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			objects = append(objects, StorageObject{
				Key:          key,
				Size:         int64(len(obj.data)),
				LastModified: obj.lastModified,
			})
		}
	}

	return objects, nil
}

// Helper methods for testing

// AddObject adds an object directly to the mock storage
func (m *MockClient) AddObject(key string, data []byte, lastModified time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects[key] = &mockObject{
		data:         data,
		lastModified: lastModified,
	}
}

// GetObject returns the raw data for a key (for test assertions)
func (m *MockClient) GetObject(key string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, false
	}
	return obj.data, true
}

// GetTags returns the tags for a key (for test assertions)
func (m *MockClient) GetTags(key string) (*ObjectTags, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, false
	}
	return obj.tags, true
}

// Count returns the number of objects in the mock storage
func (m *MockClient) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.objects)
}

// Clear removes all objects from the mock storage
func (m *MockClient) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects = make(map[string]*mockObject)
}

// Keys returns all keys in the mock storage
func (m *MockClient) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	return keys
}

// Verify MockClient implements StorageClient
var _ StorageClient = (*MockClient)(nil)

