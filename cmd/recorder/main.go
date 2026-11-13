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

	"github.com/jdolan-exalink/openvms-rec/internal/api"
	"github.com/jdolan-exalink/openvms-rec/internal/app"
	"github.com/jdolan-exalink/openvms-rec/internal/auth"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/platform/winsvc"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/health"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
	"github.com/jdolan-exalink/openvms-rec/internal/sysinfo"
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
  openvms-recorder backup        [-config FILE] DEST.db
  openvms-recorder service install -config ABS_PATH   (Windows)
  openvms-recorder service uninstall                  (Windows)
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
	case "backup":
		return cmdBackup(args[1:])
	case "service":
		return cmdService(args[1:])
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
	ring := sysinfo.NewRing(50)
	log := slog.New(ring.Handler(slog.NewJSONHandler(os.Stderr, nil)))
	if err := winsvc.KillChildrenOnExit(); err != nil {
		log.Warn("could not bind FFmpeg lifetime to the recorder", "err", err)
	}
	if _, err := exec.LookPath(cfg.Recording.FFmpegPath); err != nil && len(cfg.Cameras) > 0 {
		return fmt.Errorf("ffmpeg not found (%s): install it or set recording.ffmpeg_path", cfg.Recording.FFmpegPath)
	}
	run := func(ctx context.Context) error {
		a, err := app.New(ctx, cfg, volumes.OSProber{}, log, version)
		if err != nil {
			return err
		}
		a.Logs = ring
		defer a.Close()
		return a.Run(ctx)
	}
	if winsvc.IsService() {
		// Under the SCM stderr goes nowhere: log to a file in the data dir.
		os.MkdirAll(cfg.Data.Dir, 0o750)
		if f, err := os.OpenFile(filepath.Join(cfg.Data.Dir, "recorder.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); err == nil {
			log = slog.New(ring.Handler(slog.NewJSONHandler(f, nil)))
			defer f.Close()
		}
		winsvc.Info("OpenVMS Recorder " + version + " starting")
		return winsvc.Run(run)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}

func cmdService(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ExitOnError)
		path := fs.String("config", "", "absolute path of the configuration file")
		fs.Parse(args[1:])
		if *path == "" {
			return errors.New("service install needs -config with an absolute path")
		}
		abs, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		if _, err := config.Load(abs); err != nil {
			return err
		}
		if err := winsvc.Install(abs); err != nil {
			return err
		}
		fmt.Printf("service %s installed (automatic start, restart on failure, account NT SERVICE\\%s).\n", winsvc.Name, winsvc.Name)
		fmt.Println("Grant that account Modify on the data dir and every recording root, e.g.:")
		fmt.Printf("  icacls \"D:\\OpenVMS\\Recordings\" /grant \"NT SERVICE\\%s:(OI)(CI)M\"\n", winsvc.Name)
		fmt.Printf("then: sc start %s\n", winsvc.Name)
		return nil
	case "uninstall":
		if err := winsvc.Uninstall(); err != nil {
			return err
		}
		fmt.Println("service removed; recordings, database and configuration were not touched")
		return nil
	}
	return usage()
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
		// Same shape as GET /api/v1/storage/volumes.
		out := make([]api.VolumeDTO, len(sts))
		for i := range sts {
			out[i] = api.ToVolumeDTO(sts[i], cands[i].Writable)
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
		// Report the state the recorder will see, e.g. degraded when another
		// configured volume sits on the same disk.
		var st volumes.Status
		for _, x := range (volumes.Inspector{Prober: volumes.OSProber{}}).InspectAll(storage.SpecsFromConfig(cfg, runtime.GOOS)) {
			if x.Spec.ID == id {
				st = x
			}
		}
		fmt.Printf("volume %s initialized at %s (stable id %s, probe %s), state %s\n", id, sp.Root, m.StableID, p.Latency.Round(time.Microsecond), st.State)
		if st.Reason != "" {
			fmt.Printf("  %s\n", st.Reason)
		}
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

// cmdBackup makes a consistent online copy of the index database. Restore:
// stop the service, replace recorder.db with the copy (remove -wal/-shm),
// start the service; startup reconciliation re-checks segments on disk.
func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("backup needs exactly one DEST path")
	}
	ctx := context.Background()
	h, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer h.Close()
	if err := db.Backup(ctx, h, fs.Arg(0)); err != nil {
		return err
	}
	fmt.Println("backup written to", fs.Arg(0))
	return nil
}
