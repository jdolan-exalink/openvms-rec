package retention

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrNoLongerEligible is returned by Store.BeginDelete when a segment became
// protected or held after planning; the executor skips it.
var ErrNoLongerEligible = errors.New("retention: segment no longer eligible")

// Store is the index operations the executor needs.
type Store interface {
	// BeginDelete atomically re-checks eligibility at now, marks the segment
	// as deleting and returns its path relative to the volume root.
	BeginDelete(ctx context.Context, segmentID int64, now time.Time) (relPath string, err error)
	// FinishDelete records that the file is gone.
	FinishDelete(ctx context.Context, segmentID int64) error
}

// Result summarizes an execution.
type Result struct {
	Deleted    int
	FreedBytes uint64
	Skipped    int
	Errors     []error
}

// Execute applies a plan on the volume mounted at root. Index first, file
// second, index again: a crash between steps leaves a "deleting" row that
// startup reconciliation finishes, never an index entry pointing to nothing
// that looks valid. Files already missing count as deleted.
func Execute(ctx context.Context, plan Plan, root string, st Store, now time.Time) Result {
	var r Result
	for _, d := range plan.Deletions {
		if ctx.Err() != nil {
			r.Errors = append(r.Errors, ctx.Err())
			return r
		}
		rel, err := st.BeginDelete(ctx, d.SegmentID, now)
		if errors.Is(err, ErrNoLongerEligible) {
			r.Skipped++
			continue
		}
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.Errors = append(r.Errors, err)
			continue
		}
		if err := st.FinishDelete(ctx, d.SegmentID); err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		r.Deleted++
		r.FreedBytes += d.SizeBytes
	}
	return r
}
