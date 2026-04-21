// Package capture records full request/response bodies to disk for debugging.
//
// Disabled by default. A SIGUSR1 to the proxy process arms a bounded capture
// window: the next N proxied requests are each written as one JSON file to
// the configured output folder, then the window closes automatically.
//
// The feature is never on unless explicitly enabled in config AND an output
// folder is set — there is no default folder to guard against bodies landing
// somewhere unexpected.
package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// MaxResponseBytes caps how much of each response body is captured, to keep
// per-file size predictable. Bodies past this are truncated.
const MaxResponseBytes = 5 * 1024 * 1024 // 5 MB

// DefaultMaxMessages is used when enabled but max_messages is unset or <= 0.
const DefaultMaxMessages = 5

// Config controls capture behavior. Zero value = disabled.
type Config struct {
	Enabled      bool
	OutputFolder string
	MaxMessages  int
}

// Capture manages a bounded, SIGUSR1-armed capture window.
// A nil *Capture is safe at every call site.
type Capture struct {
	outputFolder string
	maxMessages  int
	remaining    atomic.Int32
}

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
	max := cfg.MaxMessages
	if max <= 0 {
		max = DefaultMaxMessages
	}
	return &Capture{outputFolder: cfg.OutputFolder, maxMessages: max}, nil
}

// Configured reports whether the capture feature is active.
func (c *Capture) Configured() bool { return c != nil }

// OutputFolder returns the configured directory, or "" if disabled.
func (c *Capture) OutputFolder() string {
	if c == nil {
		return ""
	}
	return c.outputFolder
}

// MaxMessages returns the configured window size, or 0 if disabled.
func (c *Capture) MaxMessages() int {
	if c == nil {
		return 0
	}
	return c.maxMessages
}

// Arm opens a capture window for the next MaxMessages requests. Calling Arm
// again re-arms to the full window. Returns the window size.
func (c *Capture) Arm() int {
	if c == nil {
		return 0
	}
	c.remaining.Store(int32(c.maxMessages))
	return c.maxMessages
}

// Reserve atomically claims one capture slot. Returns nil if the window is
// closed (no remaining slots).
func (c *Capture) Reserve() *Slot {
	if c == nil {
		return nil
	}
	for {
		n := c.remaining.Load()
		if n <= 0 {
			return nil
		}
		if c.remaining.CompareAndSwap(n, n-1) {
			seq := c.maxMessages - int(n) + 1
			return &Slot{folder: c.outputFolder, seq: seq, total: c.maxMessages}
		}
	}
}

// Slot represents one reserved capture slot. Call Write exactly once.
type Slot struct {
	folder string
	seq    int
	total  int
}

// Payload is the JSON structure written to disk.
type Payload struct {
	RequestID string           `json:"request_id"`
	Timestamp string           `json:"timestamp"`
	Request   RequestSnapshot  `json:"request"`
	Response  ResponseSnapshot `json:"response"`
	Timing    TimingSnapshot   `json:"timing"`
}

type RequestSnapshot struct {
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	VirtualModel string            `json:"virtual_model"`
	RealModel    string            `json:"real_model"`
	Backend      string            `json:"backend"`
	Protocol     string            `json:"protocol"`
	Streaming    bool              `json:"streaming"`
	Headers      map[string]string `json:"headers,omitempty"`
	Incoming     json.RawMessage   `json:"incoming,omitempty"` // body as received from client
	Resolved     json.RawMessage   `json:"resolved,omitempty"` // body as sent to backend
}

type ResponseSnapshot struct {
	StatusCode int             `json:"status_code,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"` // non-streaming: parsed JSON or quoted string
	SSE        string          `json:"sse,omitempty"`  // streaming: raw SSE bytes
	Truncated  bool            `json:"truncated,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type TimingSnapshot struct {
	StartedAt  string  `json:"started_at"`
	DurationMs float64 `json:"duration_ms"`
}

// FileTimestamp returns a filesystem-safe timestamp (no colons) for file names.
func FileTimestamp(t time.Time) string {
	return t.UTC().Format("20060102T150405.000Z")
}

// Write serializes payload to {file_timestamp}-{request_id}.json in the
// capture folder. Write is atomic (write-then-rename) so readers never see a
// partial file. A Slot must be written exactly once.
func (s *Slot) Write(p Payload) error {
	if s == nil {
		return nil
	}
	ts, err := time.Parse(time.RFC3339Nano, p.Timestamp)
	if err != nil {
		ts = time.Now()
	}
	fname := fmt.Sprintf("%s-%s.json", FileTimestamp(ts), p.RequestID)
	final := filepath.Join(s.folder, fname)
	tmp := final + ".tmp"

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal capture: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write capture: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename capture: %w", err)
	}
	log.Printf("[capture] wrote %s (%d/%d)", fname, s.seq, s.total)
	return nil
}

// CappedBuffer accumulates bytes up to Max, marking Truncated once the cap
// is reached. Writes past the cap are silently dropped. Zero value is valid
// if Max is set before first Write.
type CappedBuffer struct {
	Max       int
	buf       bytes.Buffer
	Truncated bool
}

// Write implements io.Writer. Never returns an error — overflow is recorded
// in Truncated rather than returned, since this is a best-effort debug sink.
func (b *CappedBuffer) Write(p []byte) (int, error) {
	if b.Truncated {
		return len(p), nil
	}
	remaining := b.Max - b.buf.Len()
	if remaining <= 0 {
		b.Truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.Truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Bytes returns the accumulated bytes.
func (b *CappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// String returns the accumulated bytes as a string.
func (b *CappedBuffer) String() string { return b.buf.String() }
