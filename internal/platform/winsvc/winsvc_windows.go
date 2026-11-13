//go:build windows

// Package winsvc runs the recorder as a native Windows service (Service
// Control Manager) with automatic start, crash recovery and Event Log.
package winsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// Name is the service name; the virtual account is NT SERVICE\<Name>.
const Name = "OpenVMSRecorder"

// IsService reports whether the process was started by the SCM.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// Run executes run under the SCM until a stop request cancels its context.
func Run(run func(ctx context.Context) error) error {
	return svc.Run(Name, &handler{run: run})
}

type handler struct {
	run func(ctx context.Context) error
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			cancel()
			if err != nil {
				logEvent(eventlog.Error, "recorder stopped with error: "+err.Error())
				return true, 1 // non-zero exit triggers SCM recovery actions
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 30000}
				cancel()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// Install registers the service: automatic (delayed) start, restart on
// failure, runs as the virtual account NT SERVICE\OpenVMSRecorder (least
// privilege; grant it Modify on each recording root and the data dir).
func Install(configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(Name); err == nil {
		s.Close()
		return fmt.Errorf("service %s already installed", Name)
	}
	s, err := m.CreateService(Name, exe, mgr.Config{
		DisplayName:      "OpenVMS Recorder",
		Description:      "OpenVMS Recorder NVR: records RTSP cameras to local volumes.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ServiceStartName: `NT SERVICE\` + Name,
	}, "serve", "-config", configPath)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400); err != nil {
		return err
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	if err := eventlog.InstallAsEventCreate(Name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		// Already registered is fine.
		_ = err
	}
	return nil
}

// Uninstall removes the service registration. Recordings, database and
// configuration are left untouched.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return fmt.Errorf("service %s not installed", Name)
	}
	defer s.Close()
	_ = eventlog.Remove(Name)
	return s.Delete()
}

func logEvent(kind uint16, msg string) {
	l, err := eventlog.Open(Name)
	if err != nil {
		return
	}
	defer l.Close()
	switch kind {
	case eventlog.Error:
		l.Error(1, msg)
	case eventlog.Warning:
		l.Warning(1, msg)
	default:
		l.Info(1, msg)
	}
}

// Info writes an informational Event Log entry.
func Info(msg string) { logEvent(eventlog.Info, msg) }

// KillChildrenOnExit puts the process in a job object that kills FFmpeg
// children if the recorder dies, so no orphan keeps a camera session open.
func KillChildrenOnExit() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return err
	}
	return windows.AssignProcessToJobObject(job, windows.CurrentProcess())
	// The handle is intentionally never closed: it closes when the process exits.
}

// ErrAccessDenied means the caller needs elevation for the operation.
var ErrAccessDenied = windows.ERROR_ACCESS_DENIED

// State returns the service state ("running", "stopped", "start_pending",
// "stop_pending", ...) using only query rights, so it works without
// elevation. installed is false when the service is not registered.
func State() (state string, installed bool, err error) {
	s, closeFn, err := open(windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer closeFn()
	st, err := s.Query()
	if err != nil {
		return "", true, err
	}
	return stateName(st.State), true, nil
}

func stateName(s svc.State) string {
	switch s {
	case svc.Running:
		return "running"
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start_pending"
	case svc.StopPending:
		return "stop_pending"
	case svc.Paused:
		return "paused"
	default:
		return "unknown"
	}
}

// Control starts, stops or restarts the service and waits up to 45 s.
// Returns ErrAccessDenied when the caller lacks the rights.
func Control(action string) error {
	s, closeFn, err := open(windows.SERVICE_START | windows.SERVICE_STOP | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer closeFn()
	stop := func() error {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			return err
		}
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
		return wait(s, svc.Stopped)
	}
	switch action {
	case "start":
		if err := s.Start(); err != nil {
			return err
		}
		return wait(s, svc.Running)
	case "stop":
		return stop()
	case "restart":
		if err := stop(); err != nil {
			return err
		}
		if err := s.Start(); err != nil {
			return err
		}
		return wait(s, svc.Running)
	}
	return fmt.Errorf("unknown action %q", action)
}

func open(access uint32) (*mgr.Service, func(), error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, err
	}
	name, _ := windows.UTF16PtrFromString(Name)
	h, err := windows.OpenService(scm, name, access)
	if err != nil {
		windows.CloseServiceHandle(scm)
		return nil, nil, err
	}
	s := &mgr.Service{Name: Name, Handle: h}
	return s, func() { s.Close(); windows.CloseServiceHandle(scm) }, nil
}

func wait(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("service did not reach the expected state in 45 s")
}
