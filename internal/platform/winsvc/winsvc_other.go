//go:build !windows

// Package winsvc is the Windows service integration; on other platforms the
// functions are inert so callers need no build tags.
package winsvc

import (
	"context"
	"errors"
)

const Name = "OpenVMSRecorder"

var errNotWindows = errors.New("windows service management is only available on Windows")

func IsService() bool                               { return false }
func Run(run func(ctx context.Context) error) error { return run(context.Background()) }
func Install(string) error                          { return errNotWindows }
func Uninstall() error                              { return errNotWindows }
func Info(string)                                   {}
func KillChildrenOnExit() error                     { return nil }

// ErrAccessDenied is never returned outside Windows.
var ErrAccessDenied = errors.New("access denied")

// State is not available outside Windows.
func State() (string, bool, error) { return "", false, nil }

// Control is not available outside Windows.
func Control(string) error { return errors.New("windows services are only available on Windows") }
