//go:build hardened

package main

import (
	"log"

	"github.com/wentbackward/llm-proxy/internal/proxy"
)

// BuildMode identifies this binary to operators at startup.
const BuildMode = "hardened"

// logStartupBanner prints a one-line confirmation that debug features have
// been compiled out. Kept short because there's nothing to warn about —
// this is the "nothing to see here" build.
func logStartupBanner() {
	log.Printf("[llm-proxy] %s — hardened build (SIGUSR1 capture, log levels 3-4, and journal prompt text are stripped)", Version)
}

// installCaptureSignal is a no-op in hardened builds.
func installCaptureSignal(_ *proxy.Server) {}
