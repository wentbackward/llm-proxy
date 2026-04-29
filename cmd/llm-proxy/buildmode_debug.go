//go:build !hardened

package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/wentbackward/llm-proxy/internal/proxy"
)

// BuildMode identifies this binary to operators at startup and in docs.
const BuildMode = "debug"

// logStartupBanner prints a visible banner listing the debug-only features
// that are compiled into this binary. Intended to be grep-able in container
// logs so operators can't miss that this build includes prompt-bearing
// capabilities. Build with `-tags hardened` to strip them.
func logStartupBanner() {
	banner := fmt.Sprintf(`
===============================================================================
  llm-proxy %s — DEBUG BUILD — includes features that can expose prompt contents:
    * SIGUSR1 writes full request/response bodies to disk (when enabled)
    * LOG_LEVEL=3 logs 80 bytes of request bodies
    * LOG_LEVEL=4 logs full request and response message text
    * The request journal records up to 2KB of system + 8KB of last user text

  For production use, build with:  go build -tags hardened ./cmd/llm-proxy
  See docs/security.md for details.
===============================================================================`, Version)
	log.Print(banner)
}

// installCaptureSignal wires SIGUSR1 to the capture feature.
func installCaptureSignal(srv *proxy.Server) {
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		for range usr1 {
			c := srv.Capture()
			if c == nil {
				log.Println("[llm-proxy] SIGUSR1 received — message capture disabled; enable sig_message_capture in config")
				continue
			}
			n := c.Arm()
			log.Printf("[llm-proxy] SIGUSR1 received — capturing next %d requests to %s", n, c.OutputFolder())
		}
	}()
}
