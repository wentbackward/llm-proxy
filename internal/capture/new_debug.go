//go:build !hardened

package capture

import (
	"fmt"
	"os"
)

// New returns a Capture if cfg.Enabled is true AND OutputFolder is set.
// Returns nil if the feature is not configured — that nil is safe to pass
// around; Reserve returns nil and Arm is a no-op.
func New(cfg Config) (*Capture, error) {
	if !cfg.Enabled || cfg.OutputFolder == "" {
		return nil, nil
	}
	if err := os.MkdirAll(cfg.OutputFolder, 0o700); err != nil {
		return nil, fmt.Errorf("capture output_folder: %w", err)
	}
	m := cfg.MaxMessages
	if m <= 0 {
		m = DefaultMaxMessages
	}
	if m > MaxAllowedMessages {
		return nil, fmt.Errorf("capture max_messages=%d exceeds maximum %d", m, MaxAllowedMessages)
	}
	return &Capture{outputFolder: cfg.OutputFolder, maxMessages: int32(m)}, nil
}
