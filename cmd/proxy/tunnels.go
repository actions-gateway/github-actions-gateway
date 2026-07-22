package main

import (
	"net"
	"sync"
)

// tunnel is one in-flight hijacked CONNECT relay. It owns the connections making
// up the relay so a shutdown that overruns its drain deadline can force them
// closed, unblocking the io.Copy goroutines in handleConnect.
type tunnel struct {
	// closed is closed by tunnelTracker.release once the relay has finished.
	closed chan struct{}

	mu    sync.Mutex
	conns []net.Conn
}

// track records a connection belonging to this tunnel. The upstream connection
// is recorded before the client connection is hijacked, so a registered tunnel
// is always force-closable even if shutdown lands mid-handshake.
func (t *tunnel) track(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.conns = append(t.conns, c)
}

// closeNow closes every connection in the relay. Both directions are closed
// rather than one: each relay goroutine blocks in io.Copy on a read, and only
// closing the connection it is reading from unblocks it.
func (t *tunnel) closeNow() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.conns {
		_ = c.Close()
	}
}

// tunnelTracker tracks in-flight hijacked CONNECT tunnels so shutdown can wait
// for them.
//
// net/http stops tracking a connection the moment it is hijacked, and
// http.Server.Shutdown documents that it neither closes nor waits for hijacked
// connections. Every CONNECT tunnel is hijacked, so without this tracker
// Shutdown returns while tunnels are still relaying and the process exits
// mid-stream, cutting live CI egress on every rollout (Q384).
type tunnelTracker struct {
	mu      sync.Mutex
	tunnels map[*tunnel]struct{}
}

// add registers a new tunnel.
//
// Callers must register BEFORE hijacking the client connection. While the
// handler is still pre-hijack, net/http counts its connection as active and
// Shutdown waits for it; registering first therefore closes the window in which
// Shutdown could return between a hijack and its registration and observe a
// tracker that looks empty.
func (t *tunnelTracker) add() *tunnel {
	tn := &tunnel{closed: make(chan struct{})}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tunnels == nil {
		t.tunnels = make(map[*tunnel]struct{})
	}
	t.tunnels[tn] = struct{}{}
	return tn
}

// release deregisters a finished tunnel and unblocks anyone waiting on it.
func (t *tunnelTracker) release(tn *tunnel) {
	t.mu.Lock()
	delete(t.tunnels, tn)
	t.mu.Unlock()
	close(tn.closed)
}

// drained returns a channel closed once every tunnel registered at the moment of
// the call has finished. Per repo convention the channel is returned rather than
// waited on internally, so the caller decides how to bound the wait.
//
// Call it only after http.Server.Shutdown has returned: at that point the
// listener is closed and every surviving handler has already hijacked — and so
// already registered — which is what makes the snapshot the complete set.
func (t *tunnelTracker) drained() <-chan struct{} {
	pending := t.snapshot()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, tn := range pending {
			<-tn.closed
		}
	}()
	return done
}

// closeAll force-closes every tunnel still open, returning how many were cut.
func (t *tunnelTracker) closeAll() int {
	pending := t.snapshot()
	for _, tn := range pending {
		tn.closeNow()
	}
	return len(pending)
}

// snapshot returns the currently registered tunnels.
func (t *tunnelTracker) snapshot() []*tunnel {
	t.mu.Lock()
	defer t.mu.Unlock()
	pending := make([]*tunnel, 0, len(t.tunnels))
	for tn := range t.tunnels {
		pending = append(pending, tn)
	}
	return pending
}
