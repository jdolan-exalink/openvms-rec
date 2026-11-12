// Command openvms-recorder is the OpenVMS Recorder service and CLI.
//
//	openvms-recorder serve          [-config FILE]
//	openvms-recorder storage check  [-config FILE] [-json]
//	openvms-recorder storage init   [-config FILE] VOLUME_ID
//	openvms-recorder migrate        [-config FILE]
//	openvms-recorder apikey create  [-config FILE] NAME
//	openvms-recorder version
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/app"
	"github.com/jdolan-exalink/openvms-rec/internal/auth"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/health"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New(`usage:
  openvms-recorder serve         [-config FILE]
  openvms-recorder storage check [-config FILE] [-json]
  openvms-recorder storage init  [-config FILE] VOLUME_ID
  openvms-recorder migrate       [-config FILE]
  openvms-recorder apikey create [-config FILE] NAME
  openvms-recorder version`)
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "version", "-version", "--version":
		fmt.Printf("openvms-recorder %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "serve":
		return cmdServe(args[1:])
	case "migrate":
		return cmdMigrate(args[1:])
	case "apikey":
		if len(args) >= 2 && args[1] == "create" {
			return cmdAPIKeyCreate(args[2:])
		}
	case "storage":
		if len(args) < 2 {
			return usage()
		}
		switch args[1] {
		case "check":
			return cmdStorageCheck(args[2:])
		case "init":
			return cmdStorageInit(args[2:])
		}
	}
	return usage()
}

func loadConfig(fs *flag.FlagSet, args []string) (config.Config, error) {
	path := fs.String("config", os.Getenv("OPENVMS_RECORDER_CONFIG"), "configuration file (YAML)")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	return config.Load(*path)
}

func openDB(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	if err := os.MkdirAll(cfg.Data.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	return db.Open(ctx, filepath.Join(cfg.Data.Dir, "recorder.db"))
}

func cmdMigrate(args []string) error {
	cfg, err := loadConfig(flag.NewFlagSet("migrate", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	h, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer h.Close()
	v, err := db.Version(ctx, h)
	if err != nil {
		return err
	}
	fmt.Println("schema version:", v)
	return nil
}

func cmdServe(args []string) error {
	cfg, err := loadConfig(flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := exec.LookPath(cfg.Recording.FFmpegPath); err != nil && len(cfg.Cameras) > 0 {
		return fmt.Errorf("ffmpeg not found (%s): install it or set recording.ffmpeg_path", cfg.Recording.FFmpegPath)
	}
	a, err := app.New(ctx, cfg, volumes.OSProber{}, log, version)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}

func cmdStorageCheck(args []string) error {
	fs := flag.NewFlagSet("storage check", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	// Read-only: no database needed, recorder bytes reported as 0 when the
	// index is not available.
	sts := volumes.Inspector{Prober: volumes.OSProber{}}.InspectAll(storage.SpecsFromConfig(cfg, runtime.GOOS))
	cands := storage.Candidates(sts, cfg.Storage)

	if *asJSON {
		type row struct {
			volumes.Status
			Writable bool
		}
		out := make([]row, len(sts))
		for i := range sts {
			out[i] = row{sts[i], cands[i].Writable}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tWRITE\tFS\tCAPACITY\tFREE\tLIMIT\tBINDING\tSTABLE ID\tPATH")
	for i, st := range sts {
		fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			st.Spec.ID, st.State, cands[i].Writable, st.Info.FSType,
			gb(st.Usage.CapacityBytes), gb(st.Usage.FreeBytes), gb(st.Limits.Effective),
			st.Limits.Binding, st.Info.StableID, st.Spec.Root)
	}
	tw.Flush()
	for i, st := range sts {
		if st.Reason != "" {
			fmt.Printf("  %s: %s\n", st.Spec.ID, st.Reason)
		}
		for _, w := range st.Warnings {
			fmt.Printf("  %s: warning %s\n", st.Spec.ID, w)
		}
		if st.State == volumes.StateHealthy && !cands[i].Writable {
			fmt.Printf("  %s: healthy but not writable (system/network volume not allowed by config)\n", st.Spec.ID)
		}
	}
	fmt.Println("\nNote: several volumes spread load and capacity; they are NOT redundancy. Use RAID/Storage Spaces/ZFS for fault tolerance.")
	return nil
}

func cmdStorageInit(args []string) error {
	fs := flag.NewFlagSet("storage init", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("storage init needs exactly one VOLUME_ID")
	}
	id := fs.Arg(0)
	for _, sp := range storage.SpecsFromConfig(cfg, runtime.GOOS) {
		if sp.ID != id {
			continue
		}
		m, err := volumes.Initialize(volumes.OSProber{}, sp, time.Now())
		if err != nil {
			return err
		}
		p := health.Probe(sp.Root)
		if !p.OK {
			return fmt.Errorf("volume %s initialized but write probe failed: %w", id, p.Err)
		}
		fmt.Printf("volume %s ready at %s (stable id %s, probe %s)\n", id, sp.Root, m.StableID, p.Latency.Round(time.Microsecond))
		return nil
	}
	return fmt.Errorf("volume %q not in configuration", id)
}

func gb(b uint64) string { return fmt.Sprintf("%.1fGB", float64(b)/volumes.GB) }

func cmdAPIKeyCreate(args []string) error {
	fs := flag.NewFlagSet("apikey create", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("apikey create needs exactly one NAME")
	}
	if cfg.Server.APIKeysFile == "" {
		return errors.New("set server.api_keys_file in the configuration first")
	}
	tok, k, err := auth.Generate(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := auth.AppendKey(cfg.Server.APIKeysFile, k); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "API key %q created. It is shown only once; store it securely:\n", fs.Arg(0))
	fmt.Println(tok)
	return nil
}
