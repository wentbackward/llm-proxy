//go:build !hardened

package logger

import "log"

// MaxAllowedLevel is the highest log level the logger will honor.
// In the default (debug) build, all four levels are available.
const MaxAllowedLevel = LevelContent

// Body logs at level 3 (shows first 80 chars of request bodies).
func Body(format string, args ...interface{}) {
	if Get() >= LevelBody {
		log.Printf("[body] "+format, args...)
	}
}

// Content logs at level 4 (shows full message text of requests and responses).
func Content(format string, args ...interface{}) {
	if Get() >= LevelContent {
		log.Printf(format, args...)
	}
}
