// Package storage wires configuration to the multi-volume storage engine:
// volumes (identity, limits), placement, retention and health.
package storage

import (
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/placement"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// SpecsFromConfig converts configured volumes for the given GOOS.
func SpecsFromConfig(cfg config.Config, goos string) []volumes.Spec {
	s := cfg.Storage
	out := make([]volumes.Spec, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		out = append(out, volumes.Spec{
			ID:       v.ID,
			Root:     v.ResolvedPath(goos),
			Enabled:  v.IsEnabled(),
			Priority: v.Priority,
			Weight:   v.EffectiveWeight(),
			Cameras:  append([]string(nil), v.Cameras...),
			Policy: volumes.Policy{
				MaxPercent:           v.EffectiveMaxPercent(s),
				QuotaBytes:           uint64(v.QuotaGB * volumes.GB),
				MinFreeBytes:         uint64(v.EffectiveMinFreeGB(s) * volumes.GB),
				DefaultMaxPercent:    s.DefaultMaxPercent,
				CleanupStartPercent:  s.CleanupStartPercent,
				CleanupTargetPercent: s.CleanupTargetPercent,
			},
		})
	}
	return out
}

// Candidates turns inspection results into placement candidates. System and
// network volumes are only writable when the configuration explicitly allows
// them, to avoid filling the OS disk or depending on a network share.
func Candidates(sts []volumes.Status, s config.Storage) []placement.Candidate {
	out := make([]placement.Candidate, 0, len(sts))
	for _, st := range sts {
		w := st.Writable()
		if st.Info.IsSystem && !s.AllowSystemVolume {
			w = false
		}
		if st.Info.IsNetwork && !s.AllowNetworkVolumes {
			w = false
		}
		out = append(out, placement.Candidate{
			ID:            st.Spec.ID,
			Writable:      w,
			Priority:      st.Spec.Priority,
			Weight:        st.Spec.Weight,
			HeadroomBytes: st.Limits.Headroom,
			Cameras:       st.Spec.Cameras,
		})
	}
	return out
}
