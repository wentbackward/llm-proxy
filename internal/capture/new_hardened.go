//go:build hardened

package capture

// New is a no-op in hardened builds: the SIGUSR1 message-capture feature is
// compiled out. Callers already check `c == nil`, so skipping the feature
// happens transparently — no config option, no runtime flag, no file writes.
func New(_ Config) (*Capture, error) {
	return nil, nil
}
