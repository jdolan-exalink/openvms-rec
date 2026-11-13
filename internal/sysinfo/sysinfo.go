package sysinfo

import (
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// Proc is one process's usage. CPUPercent is of one core (100 = one full
// core), averaged since the previous sample.
type Proc struct {
	PID        int     `json:"pid"`
	CPUPercent float64 `json:"cpu_percent"`
	RSSBytes   uint64  `json:"rss_bytes"`
}

// Host is machine-wide usage.
type Host struct {
	CPUPercent    float64 `json:"cpu_percent"` // all cores, 0-100
	CPUs          int     `json:"cpus"`
	MemTotalBytes uint64  `json:"mem_total_bytes"`
	MemUsedBytes  uint64  `json:"mem_used_bytes"`
}

// Sampler computes CPU deltas between calls. Safe for concurrent use.
type Sampler struct {
	mu    sync.Mutex
	prev  map[int32]sample
	start time.Time
}

type sample struct {
	at   time.Time
	busy float64 // seconds
}

// NewSampler starts measuring now.
func NewSampler() *Sampler { return &Sampler{prev: map[int32]sample{}, start: time.Now()} }

// Uptime since the sampler was created (the recorder's start).
func (s *Sampler) Uptime() time.Duration { return time.Since(s.start) }

// Process samples pid. Unknown or exited processes return ok=false.
func (s *Sampler) Process(pid int) (Proc, bool) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return Proc{}, false
	}
	out := Proc{PID: pid}
	if m, err := p.MemoryInfo(); err == nil {
		out.RSSBytes = m.RSS
	}
	t, err := p.Times()
	if err != nil {
		return out, true
	}
	now := time.Now()
	busy := t.User + t.System
	s.mu.Lock()
	prev, seen := s.prev[int32(pid)]
	s.prev[int32(pid)] = sample{now, busy}
	s.mu.Unlock()
	if seen {
		if dt := now.Sub(prev.at).Seconds(); dt > 0 {
			out.CPUPercent = max(0, (busy-prev.busy)/dt*100)
		}
	} else if ct, err := p.CreateTime(); err == nil {
		if dt := now.Sub(time.UnixMilli(ct)).Seconds(); dt > 0 {
			out.CPUPercent = busy / dt * 100
		}
	}
	return out, true
}

// Self samples the recorder process.
func (s *Sampler) Self() Proc {
	p, _ := s.Process(os.Getpid())
	return p
}

// Host samples machine-wide CPU (since the previous call) and memory.
func (s *Sampler) Host() Host {
	h := Host{CPUs: runtime.NumCPU()}
	if v, err := cpu.Percent(0, false); err == nil && len(v) == 1 {
		h.CPUPercent = v[0]
	}
	if m, err := mem.VirtualMemory(); err == nil {
		h.MemTotalBytes, h.MemUsedBytes = m.Total, m.Used
	}
	return h
}

// Forget drops samples of processes not in keep.
func (s *Sampler) Forget(keep map[int]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pid := range s.prev {
		if int(pid) != os.Getpid() && !keep[int(pid)] {
			delete(s.prev, pid)
		}
	}
}
