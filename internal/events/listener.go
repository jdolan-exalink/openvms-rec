package events

import (
	"context"
	"sync"
)

// Listener is a background event source (ONVIF PullPoint, brand-specific
// listener such as provisionISR, or local NVR motion analytics).
type Listener interface {
	Name() string
	Start(ctx context.Context)
	RestartCamera(cameraID string)
}

// Name returns the listener identifier for ONVIFPuller.
func (p *ONVIFPuller) Name() string { return "onvif_pullpoint" }

// Manager runs all registered event listeners concurrently.
type Manager struct {
	listeners []Listener
}

// NewManager creates a Manager for the given listeners.
func NewManager(listeners ...Listener) *Manager {
	var active []Listener
	for _, l := range listeners {
		if l != nil {
			active = append(active, l)
		}
	}
	return &Manager{listeners: active}
}

// Start launches every listener and blocks until ctx is cancelled and all exit.
func (m *Manager) Start(ctx context.Context) {
	if m == nil || len(m.listeners) == 0 {
		<-ctx.Done()
		return
	}
	var wg sync.WaitGroup
	for _, l := range m.listeners {
		wg.Add(1)
		go func(lst Listener) {
			defer wg.Done()
			lst.Start(ctx)
		}(l)
	}
	wg.Wait()
}

// RestartCamera notifies every listener to reconcile or restart workers for id.
func (m *Manager) RestartCamera(id string) {
	if m == nil {
		return
	}
	for _, l := range m.listeners {
		l.RestartCamera(id)
	}
}
