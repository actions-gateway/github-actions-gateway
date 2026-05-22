package agentpool

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
)

// StubRegistrar is a Registrar that simulates registration without calling GitHub.
// Used in M2 while Investigation A (runner registration API) is pending.
// Each Register call returns a unique agent ID with a placeholder OAuth endpoint.
type StubRegistrar struct {
	nextID     atomic.Int64
	mu         sync.Mutex
	registered map[int64]bool
}

// NewStubRegistrar returns a StubRegistrar with a synthetic agent ID counter.
func NewStubRegistrar() *StubRegistrar {
	r := &StubRegistrar{registered: make(map[int64]bool)}
	r.nextID.Store(1000)
	return r
}

func (r *StubRegistrar) Register(_ context.Context, _ string, _ RegisterParams) (*AgentCredentials, error) {
	id := r.nextID.Add(1)
	r.mu.Lock()
	r.registered[id] = true
	r.mu.Unlock()

	// STUB_AUTH_URL / STUB_BROKER_URL let tests redirect credentials to a fake server.
	authURL := os.Getenv("STUB_AUTH_URL")
	if authURL == "" {
		authURL = "https://stub.example.com/token"
	}
	brokerURL := os.Getenv("STUB_BROKER_URL")
	if brokerURL == "" {
		brokerURL = "https://stub.example.com/broker"
	}
	return &AgentCredentials{
		AgentID:          id,
		ClientID:         "stub-client-id",
		AuthorizationURL: authURL,
		BrokerURL:        brokerURL,
	}, nil
}

func (r *StubRegistrar) Deregister(_ context.Context, _ string, agentID int64) error {
	r.mu.Lock()
	delete(r.registered, agentID)
	r.mu.Unlock()
	return nil
}
