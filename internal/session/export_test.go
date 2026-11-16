package session

// SetAfterPending sets the hook an Instructions stream calls between its
// reading of the queue and its claim of what it read, and returns what puts
// the hook back.
func SetAfterPending(f func()) (restore func()) {
	old := afterPending.Swap(&f)
	return func() { afterPending.Store(old) }
}
