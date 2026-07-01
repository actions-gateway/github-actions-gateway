package agentpool

import (
	"context"
	"sync"
	"sync/atomic"
)

// StubRegistrar is a Registrar that simulates registration without calling GitHub.
// Used in tests and in deployments that point at a fake GitHub server.
// Each Register call returns a unique agent ID with the configured OAuth endpoint.
// Like real GitHub it tracks live records by name and returns *NameConflictError
// when a name is registered twice without an intervening Deregister.
type StubRegistrar struct {
	nextID           atomic.Int64
	mu               sync.Mutex
	registered       map[int64]string // agentID → name
	names            map[string]int64 // name → agentID
	authURL          string
	brokerURL        string
	encodedJITConfig string
	registerErr      error // returned by every Register while set (test hook)
	registerCalls    int
	// busy maps an agentID to the number of remaining Deregister calls that must
	// return *RunnerBusyError before the record is finally deleted, mimicking
	// GitHub's transient 422 "runner is currently running a job and cannot be
	// deleted" after a single-use JIT job completes (Q259). A negative count
	// stays busy forever (models a runner that never releases). Set by
	// SimulateRunnerBusy.
	busy            map[int64]int
	deregisterCalls int
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
		registered: make(map[int64]string),
		names:      make(map[string]int64),
		busy:       make(map[int64]int),
		authURL:    authURL,
		brokerURL:  brokerURL,
	}
	r.nextID.Store(1000)
	return r
}

// SetEncodedJITConfig configures the JIT config blob returned by every Register call.
// Empty (the default) leaves the field unset, which is appropriate for tests that
// do not exercise the wrapper's runner-config materialization path.
func (r *StubRegistrar) SetEncodedJITConfig(blob string) {
	r.mu.Lock()
	r.encodedJITConfig = blob
	r.mu.Unlock()
}

// SetRegisterError makes every subsequent Register call return err.
// Pass nil to restore normal behavior. Test hook.
func (r *StubRegistrar) SetRegisterError(err error) {
	r.mu.Lock()
	r.registerErr = err
	r.mu.Unlock()
}

// RegisterCalls returns the number of Register invocations, including failed
// ones. Test observability.
func (r *StubRegistrar) RegisterCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerCalls
}

// AgentIDForName returns the live record's agent ID for name, or 0 when no
// record exists. Test observability.
func (r *StubRegistrar) AgentIDForName(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.names[name]
}

// SimulateServerDelete removes the live record for agentID without a
// Deregister call, mimicking GitHub deleting a single-use JIT runner after it
// completes a job. Test hook.
func (r *StubRegistrar) SimulateServerDelete(agentID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name, ok := r.registered[agentID]; ok {
		delete(r.registered, agentID)
		delete(r.names, name)
	}
}

// SimulateRunnerBusy makes the next attempts Deregister calls for agentID return
// *RunnerBusyError (GitHub's transient 422 "runner is currently running a job
// and cannot be deleted" after a single-use JIT job completes, Q259) without
// deleting the record; the following call deletes it normally. The record is
// left in place so re-registration under its name conflicts (409) until it
// releases, exactly as GitHub behaves during the auto-removal window. A negative
// attempts count stays busy forever (models a runner that never releases). Test
// hook.
func (r *StubRegistrar) SimulateRunnerBusy(agentID int64, attempts int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy[agentID] = attempts
}

// DeregisterCalls returns the number of Deregister invocations, including those
// that returned *RunnerBusyError. Test observability — proves the recycle retry
// loop is bounded and does not spin.
func (r *StubRegistrar) DeregisterCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deregisterCalls
}

func (r *StubRegistrar) Register(_ context.Context, _ string, params RegisterParams) (*AgentCredentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerCalls++
	if r.registerErr != nil {
		return nil, r.registerErr
	}
	if _, exists := r.names[params.Name]; exists {
		return nil, &NameConflictError{Name: params.Name}
	}
	id := r.nextID.Add(1)
	r.registered[id] = params.Name
	r.names[params.Name] = id
	return &AgentCredentials{
		AgentID:          id,
		ClientID:         "stub-client-id",
		AuthorizationURL: r.authURL,
		BrokerURL:        r.brokerURL,
		EncodedJITConfig: r.encodedJITConfig,
	}, nil
}

func (r *StubRegistrar) Deregister(_ context.Context, _ string, agentID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deregisterCalls++
	// Still-running race: GitHub refuses the delete with 422 while the just-
	// consumed single-use JIT runner lingers (Q259). Leave the record in place so
	// the record's name stays taken (a re-register 409s) until it releases.
	if rem, ok := r.busy[agentID]; ok && rem != 0 {
		if rem > 0 {
			r.busy[agentID] = rem - 1
		}
		return &RunnerBusyError{AgentID: agentID}
	}
	delete(r.busy, agentID)
	if name, ok := r.registered[agentID]; ok {
		delete(r.registered, agentID)
		delete(r.names, name)
	}
	return nil
}

// ResolveAgentID returns the live record's ID for name, or 0 when none exists.
func (r *StubRegistrar) ResolveAgentID(_ context.Context, _ string, name string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.names[name], nil
}
