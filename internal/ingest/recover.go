package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/ingest/tsseg"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

// finalizeTemp truncates a temp file to whole TS packets, renames it into
// place and finalizes its index row. Used for aborted sessions and crash
// recovery. A file with no complete packet is marked corrupt.
func finalizeTemp(idx Index, id int64, root, rel, tmp string, end time.Time) error {
	ctx := context.Background()
	fi, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	size := fi.Size() - fi.Size()%tsseg.PacketSize
	if size == 0 {
		os.Remove(tmp)
		return idx.MarkCorrupt(ctx, id, 0)
	}
	if size != fi.Size() {
		if err := os.Truncate(tmp, size); err != nil {
			return err
		}
	}
	sum, err := fileSHA256(tmp)
	if err != nil {
		return err
	}
	if f, err := os.OpenFile(tmp, os.O_RDWR, 0); err == nil {
		f.Sync()
		f.Close()
	}
	final := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	syncDir(filepath.Dir(final))
	return idx.Finalize(ctx, id, end, uint64(size), sum, "")
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RecoveryStore is what Reconcile needs from the index.
type RecoveryStore interface {
	Index
	Writing(ctx context.Context, volumeID string) ([]segments.Segment, error)
	PendingDeletes(ctx context.Context, volumeID string) ([]segments.Segment, error)
	FinishDelete(ctx context.Context, id int64) error
}

// ReconcileResult reports what startup reconciliation did on one volume.
type ReconcileResult struct {
	Recovered int // temp files turned into final segments
	Corrupt   int // rows with no usable data
	Deletes   int // interrupted deletions completed
}

// Reconcile repairs the index of one reachable volume after a crash or power
// loss: segments left in 'writing' are recovered from their temp file (or
// from the final file if the crash hit between rename and index update), and
// interrupted deletions are completed. It only touches this recorder's files.
func Reconcile(ctx context.Context, st RecoveryStore, volumeID, root string, log *slog.Logger) (ReconcileResult, error) {
	var r ReconcileResult
	writing, err := st.Writing(ctx, volumeID)
	if err != nil {
		return r, err
	}
	for _, seg := range writing {
		tmp := TempPath(root, seg.ID)
		final := filepath.Join(root, filepath.FromSlash(seg.RelativePath))
		switch {
		case exists(tmp):
			fi, _ := os.Stat(tmp)
			if err := finalizeTemp(st, seg.ID, root, seg.RelativePath, tmp, fi.ModTime()); err != nil {
				log.Error("recover temp segment", "segment", seg.ID, "err", err)
				continue
			}
			if fi.Size() >= tsseg.PacketSize {
				r.Recovered++
			} else {
				r.Corrupt++
			}
		case exists(final):
			fi, _ := os.Stat(final)
			sum, err := fileSHA256(final)
			if err != nil {
				return r, err
			}
			if err := st.Finalize(ctx, seg.ID, fi.ModTime(), uint64(fi.Size()), sum, ""); err != nil {
				return r, err
			}
			r.Recovered++
		default:
			if err := st.MarkCorrupt(ctx, seg.ID, 0); err != nil {
				return r, err
			}
			r.Corrupt++
		}
	}
	dels, err := st.PendingDeletes(ctx, volumeID)
	if err != nil {
		return r, err
	}
	for _, seg := range dels {
		p := filepath.Join(root, filepath.FromSlash(seg.RelativePath))
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Error("finish interrupted delete", "segment", seg.ID, "err", err)
			continue
		}
		if err := st.FinishDelete(ctx, seg.ID); err != nil {
			return r, err
		}
		r.Deletes++
	}
	return r, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
