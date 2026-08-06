package scalesetlistener

// GuardedJobIDs returns the jobIDs the three replay guards currently hold, each sorted.
// It exists so a test can assert on WHICH entries a delete retired rather than on a
// count: a total that fell says nothing about the specific job whose guard had to
// survive (Q597).
func (l *Listener) GuardedJobIDs() (provisioned, completed, abandoned []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return sortedKeys(l.provisioned), sortedKeys(l.completed), sortedKeys(l.abandoned)
}

// PendingMessageCount returns how many cursor-acked messages are still awaiting their
// delete, so a test can pin the queue state its guard assertions are read against.
func (l *Listener) PendingMessageCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending)
}
