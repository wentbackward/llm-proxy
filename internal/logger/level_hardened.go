//go:build hardened

package logger

// MaxAllowedLevel is the highest log level the logger will honor.
// Hardened builds cap at level 2 (headers) — level 3 would log 80 bytes of
// request body and level 4 would log full message text. Both are stripped.
const MaxAllowedLevel = LevelHeaders

// Body is a no-op in hardened builds. The 80-byte request-body preview that
// level 3 would emit is compiled out entirely.
func Body(_ string, _ ...interface{}) {}

// Content is a no-op in hardened builds. The full-message content that
// level 4 would emit is compiled out entirely.
func Content(_ string, _ ...interface{}) {}
