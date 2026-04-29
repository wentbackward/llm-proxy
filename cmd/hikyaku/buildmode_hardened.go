//go:build hardened

package main

import (
	"log"
	"strings"

	"github.com/wentbackward/hikyaku/internal/proxy"
)

// BuildMode identifies this binary to operators at startup.
const BuildMode = "hardened"

// logStartupBanner prints a visible banner. In dev builds it shows the ASCII
// logo; in release builds it prints a single line.
func logStartupBanner() {
	if strings.Contains(Version, "-") {
		log.Printf(` ____  _____   __  ___ _   _ ___ _    ___
|   \| __\ \/ /  | _ ) | | |_ _| |  |   \
| |)| _|  \ /   | _ \ |_| || || |__| |) |
|___/|___| \/   |___/\___/|___|____|___/

  hikyaku %s — HARDENED`, Version)
	} else {
		log.Printf("[hikyaku] %s — HARDENED", Version)
	}
}

// installCaptureSignal is a no-op in hardened builds.
func installCaptureSignal(_ *proxy.Server) {}
