//go:build hardened

package journal

// captureMessageText is false in hardened builds: journal entries retain
// their structural metadata (counts, signals, params) but strip the actual
// prompt text. Nothing the LLM saw ends up in the log record.
const captureMessageText = false
