//go:build !hardened

package main

import (
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/wentbackward/hikyaku/internal/proxy"
)

// BuildMode identifies this binary to operators at startup and in docs.
const BuildMode = "inspect"

// logStartupBanner prints a visible banner. In dev builds it shows the ASCII
// logo; in release builds it prints a single line. Both modes always indicate
// whether this is an inspect or hardened build.
func logStartupBanner() {
	if strings.Contains(Version, "-") {
		log.Printf(` HIKYAKU
____  _____   __  ___ _   _ ___ _    ___
|   \| __\ \/ /  | _ ) | | |_ _| |  |   \
| |)| _|  \ /   | _ \ |_| || || |__| |) |
|___/|___| \/   |___/\___/|___|____|___/

  hikyaku %s — INSPECT`, Version)
	} else {
		log.Printf("[hikyaku] %s — INSPECT", Version)
	}
}

// installCaptureSignal wires SIGUSR1 to the capture feature.
func installCaptureSignal(srv *proxy.Server) {
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		for range usr1 {
			c := srv.Capture()
			if c == nil {
				log.Println("[hikyaku] SIGUSR1 received — message capture disabled; enable sig_message_capture in config")
				continue
			}
			n := c.Arm()
			log.Printf("[hikyaku] SIGUSR1 received — capturing next %d requests to %s", n, c.OutputFolder())
		}
	}()
}
