// Package health probes whether a volume can really take writes. A volume
// that fails a probe is marked degraded: Recorder stops writing there and
// never formats, initializes or repairs it (PRD M04).
package health

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ProbeSize is small enough to be cheap and large enough to hit the device.
const ProbeSize = 64 * 1024

// Result of a write probe.
type Result struct {
	OK      bool
	Latency time.Duration // create + write + fsync + remove
	Err     error
}

// Probe writes, fsyncs, reads back and removes a temporary file under root.
func Probe(root string) Result {
	start := time.Now()
	buf := make([]byte, ProbeSize)
	if _, err := rand.Read(buf); err != nil {
		return Result{Err: err}
	}
	name := filepath.Join(root, ".probe-"+hex.EncodeToString(buf[:8]))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{Err: fmt.Errorf("create probe: %w", err)}
	}
	_, werr := f.Write(buf)
	serr := f.Sync()
	cerr := f.Close()
	defer os.Remove(name)
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			return Result{Err: fmt.Errorf("write probe: %w", e)}
		}
	}
	got, err := os.ReadFile(name)
	if err != nil {
		return Result{Err: fmt.Errorf("read probe: %w", err)}
	}
	if string(got) != string(buf) {
		return Result{Err: fmt.Errorf("read probe: content mismatch")}
	}
	if err := os.Remove(name); err != nil {
		return Result{Err: fmt.Errorf("remove probe: %w", err)}
	}
	return Result{OK: true, Latency: time.Since(start)}
}
