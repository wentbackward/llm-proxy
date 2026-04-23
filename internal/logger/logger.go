// Package logger provides leveled logging with runtime level changes via SIGHUP.
//
// Levels:
//
//	0 = errors only (default)
//	1 = requests — method, path, model, status, duration
//	2 = headers  — level 1 + incoming headers + transformation summary
//	3 = body     — level 2 + first 80 chars of request body
//	4 = content  — level 3 + full message text (request and response)
//
// Set via server.log_level in config.yaml, or LOG_LEVEL=<n> in the
// environment (env wins when both are set). Send SIGHUP to reload:
// docker kill --signal=HUP llm-proxy
package logger

import (
	"log"
	"os"
	"strconv"
	"sync/atomic"
)

const (
	LevelError   = 0
	LevelRequest = 1
	LevelHeaders = 2
	LevelBody    = 3
	LevelContent = 4
)

var current atomic.Int32

func init() {
	Reload()
}

// Reload reads LOG_LEVEL from the environment and applies it.
// Called automatically at package init.
func Reload() { Apply(nil) }

// Apply sets the log level using yamlLevel as the configured baseline,
// with the LOG_LEVEL environment variable taking precedence when set.
// yamlLevel may be nil (no YAML setting) or out-of-range (ignored).
// Use this from main after config load and on SIGHUP.
func Apply(yamlLevel *int) {
	l := LevelError
	src := "default"
	if yamlLevel != nil && *yamlLevel >= LevelError && *yamlLevel <= LevelContent {
		l = *yamlLevel
		src = "config"
	}
	if s := os.Getenv("LOG_LEVEL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= LevelError && n <= LevelContent {
			l = n
			src = "env"
		}
	}
	current.Store(int32(l))
	log.Printf("[logger] log level %d (%s)", l, src)
}

// Get returns the current log level.
func Get() int { return int(current.Load()) }

// Request logs at level 1.
func Request(format string, args ...interface{}) {
	if Get() >= LevelRequest {
		log.Printf("[req] "+format, args...)
	}
}

// Headers logs at level 2.
func Headers(format string, args ...interface{}) {
	if Get() >= LevelHeaders {
		log.Printf("[hdr] "+format, args...)
	}
}

// Body logs at level 3.
func Body(format string, args ...interface{}) {
	if Get() >= LevelBody {
		log.Printf("[body] "+format, args...)
	}
}

// Content logs at level 4.
func Content(format string, args ...interface{}) {
	if Get() >= LevelContent {
		log.Printf(format, args...)
	}
}
