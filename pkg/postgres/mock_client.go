package postgres

import (
	"context"
	"sync"
)

// MockClient implements ClientInterface for testing
type MockClient struct {
	mu         sync.RWMutex
	databases  map[string]bool
	extensions map[string][]string // database -> extensions
	users      map[string]string   // username -> password

	Version    string
	ShouldFail bool
	FailError  error
}

func NewMockClient() *MockClient {
	return &MockClient{
		databases:  make(map[string]bool),
		extensions: make(map[string][]string),
		users:      make(map[string]string),
		Version:    "PostgreSQL 16.0 (mock)",
	}
}

func (m *MockClient) Close() {}

func (m *MockClient) Ping(ctx context.Context) error {
	if m.ShouldFail {
		return m.FailError
	}
	return nil
}

func (m *MockClient) GetVersion(ctx context.Context) (string, error) {
	if m.ShouldFail {
		return "", m.FailError
	}
	return m.Version, nil
}

func (m *MockClient) DatabaseExists(ctx context.Context, name string) (bool, error) {
	if m.ShouldFail {
		return false, m.FailError
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.databases[name], nil
}

func (m *MockClient) CreateDatabase(ctx context.Context, name string) error {
	if m.ShouldFail {
		return m.FailError
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.databases[name] = true
	return nil
}

func (m *MockClient) EnsureDatabase(ctx context.Context, name string) error {
	exists, err := m.DatabaseExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return m.CreateDatabase(ctx, name)
}

func (m *MockClient) DropDatabase(ctx context.Context, name string) error {
	if m.ShouldFail {
		return m.FailError
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.databases, name)
	delete(m.extensions, name)
	return nil
}

func (m *MockClient) RevokePublicConnect(ctx context.Context, name string) error {
	if m.ShouldFail {
		return m.FailError
	}
	return nil
}

func (m *MockClient) CreateExtension(ctx context.Context, dbName, extensionName string) error {
	if m.ShouldFail {
		return m.FailError
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.extensions[dbName] = append(m.extensions[dbName], extensionName)
	return nil
}

func (m *MockClient) EnsureExtensions(ctx context.Context, dbName string, extensions []string) error {
	for _, ext := range extensions {
		if err := m.CreateExtension(ctx, dbName, ext); err != nil {
			return err
		}
	}
	return nil
}

func (m *MockClient) UserExists(ctx context.Context, username string) (bool, error) {
	if m.ShouldFail {
		return false, m.FailError
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.users[username]
	return exists, nil
}

func (m *MockClient) CreateUser(ctx context.Context, username, password string) error {
	if m.ShouldFail {
		return m.FailError
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[username] = password
	return nil
}

func (m *MockClient) SetPassword(ctx context.Context, username, password string) error {
	if m.ShouldFail {
		return m.FailError
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[username]; exists {
		m.users[username] = password
	}
	return nil
}

func (m *MockClient) SetConnectionLimit(ctx context.Context, username string, limit int) error {
	if m.ShouldFail {
		return m.FailError
	}
	return nil
}

func (m *MockClient) DropUser(ctx context.Context, username string) error {
	if m.ShouldFail {
		return m.FailError
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.users, username)
	return nil
}

func (m *MockClient) RevokeAllDatabaseAccess(ctx context.Context, username string) error {
	if m.ShouldFail {
		return m.FailError
	}
	return nil
}

func (m *MockClient) GrantDatabaseAccess(ctx context.Context, username, database string) error {
	if m.ShouldFail {
		return m.FailError
	}
	return nil
}

func (m *MockClient) ApplyPrivileges(ctx context.Context, username, database, preset string, additionalGrants []TableGrant) error {
	if m.ShouldFail {
		return m.FailError
	}
	return nil
}

func (m *MockClient) VerifyDatabaseIsolation(ctx context.Context, username, allowedDatabase string) ([]string, error) {
	if m.ShouldFail {
		return nil, m.FailError
	}
	return []string{allowedDatabase}, nil
}

func (m *MockClient) RevokePrivilegesInDatabase(ctx context.Context, username, database string) error {
	if m.ShouldFail {
		return m.FailError
	}
	return nil
}

// Helper methods for tests

func (m *MockClient) AddDatabase(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.databases[name] = true
}

func (m *MockClient) AddUser(username, password string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[username] = password
}

func (m *MockClient) GetDatabases() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, 0, len(m.databases))
	for db := range m.databases {
		result = append(result, db)
	}
	return result
}

func (m *MockClient) GetUsers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, 0, len(m.users))
	for user := range m.users {
		result = append(result, user)
	}
	return result
}

// MockClientCache implements ClientCacheInterface for testing
type MockClientCache struct {
	mu          sync.RWMutex
	clients     map[string]*MockClient
	DefaultMock *MockClient
}

func NewMockClientCache() *MockClientCache {
	return &MockClientCache{
		clients:     make(map[string]*MockClient),
		DefaultMock: NewMockClient(),
	}
}

func (m *MockClientCache) Get(ctx context.Context, clusterName string, config Config) (ClientInterface, error) {
	m.mu.RLock()
	client, ok := m.clients[clusterName]
	m.mu.RUnlock()

	if ok {
		return client, nil
	}

	return m.DefaultMock, nil
}

func (m *MockClientCache) Remove(clusterName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.clients, clusterName)
}

func (m *MockClientCache) Close() {}

func (m *MockClientCache) SetClient(clusterName string, client *MockClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[clusterName] = client
}

// Ensure MockClientCache implements ClientCacheInterface
var _ ClientCacheInterface = (*MockClientCache)(nil)

// Ensure MockClient implements ClientInterface
var _ ClientInterface = (*MockClient)(nil)
