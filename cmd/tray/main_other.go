//go:build !windows

// Command openvms-tray is the Windows notification-area panel. On Linux use
// systemctl and the web UI's "Sistema" page instead.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "openvms-tray only runs on Windows; on Linux use systemctl and the web UI (Sistema).")
	os.Exit(1)
}
