package agentpool

import (
	"context"
	"sync"
	"sync/atomic"
)

// StubRegistrar is a Registrar that simulates registration without calling GitHub.
// Used in tests and in deployments that point at a fake GitHub server.
// Each Register call returns a unique agent ID with the configured OAuth endpoint.
type StubRegistrar struct {
	nextID     atomic.Int64
	mu         sync.Mutex
	registered map[int64]bool
	authURL    string
	brokerURL  string
}

// NewStubRegistrar returns a StubRegistrar with a synthetic agent ID counter.
func NewStubRegistrar() *StubRegistrar {
	return NewStubRegistrarWithURLs("https://stub.example.com/token", "https://stub.example.com/broker")
}

// NewStubRegistrarWithURLs returns a StubRegistrar that returns the given
// authURL and brokerURL as the OAuth and broker endpoints for every agent.
// Used in e2e tests to point agents at a local fake server.
func NewStubRegistrarWithURLs(authURL, brokerURL string) *StubRegistrar {
	r := &StubRegistrar{
		registered: make(map[int64]bool),
		authURL:    authURL,
		brokerURL:  brokerURL,
	}
	r.nextID.Store(1000)
	return r
}

func (r *StubRegistrar) Register(_ context.Context, _ string, _ RegisterParams) (*AgentCredentials, error) {
	id := r.nextID.Add(1)
	r.mu.Lock()
	r.registered[id] = true
	r.mu.Unlock()
	return &AgentCredentials{
		AgentID:          id,
		ClientID:         "stub-client-id",
		AuthorizationURL: r.authURL,
		BrokerURL:        r.brokerURL,
	}, nil
}

func (r *StubRegistrar) Deregister(_ context.Context, _ string, agentID int64) error {
	r.mu.Lock()
	delete(r.registered, agentID)
	r.mu.Unlock()
	return nil
}
