//go:build !hardened

package journal

// captureMessageText controls whether Analyze fills Entry.SystemText and
// Entry.LastUserText with (truncated) prompt bytes. In the default build
// they're populated; the journal log record therefore contains up to 2 KB
// of the system prompt and up to 8 KB of the last user message.
const captureMessageText = true
