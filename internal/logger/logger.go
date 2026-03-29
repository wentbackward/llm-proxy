// Package logger provides levelled logging with runtime level changes via SIGHUP.
//
// Levels:
//
//	0 = errors only (default)
//	1 = requests — method, path, model, status, duration
//	2 = headers  — level 1 + request headers
//	3 = body     — level 2 + first 80 chars of request body
//
// Set LOG_LEVEL=<n> in the environment. Send SIGHUP to reload from the
// environment without restarting: docker kill --signal=HUP llm-proxy
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
)

var current atomic.Int32

func init() {
	Reload()
}

// Reload reads LOG_LEVEL from the environment and applies it.
// Called automatically at startup and on SIGHUP.
func Reload() {
	l := LevelError
	if s := os.Getenv("LOG_LEVEL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 3 {
			l = n
		}
	}
	current.Store(int32(l))
	log.Printf("[logger] log level %d", l)
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
