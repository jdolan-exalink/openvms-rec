package provisionnvr

import (
	"errors"
	"fmt"
	"net/url"
)

var (
	// ErrInvalidEndpoint reports a base URL that is not http(s)://host[:port].
	ErrInvalidEndpoint = errors.New("invalid recorder endpoint")
	// ErrNotReadOnly reports a command the Phase 1 client refuses to send.
	ErrNotReadOnly = errors.New("only read-only query commands are allowed")
)

// Class says what kind of failure a call had. It maps onto the server states OpenVMS shows:
// offline, auth_failed, unsupported and degraded.
type Class string

const (
	// ClassOffline: the recorder could not be reached or the connection broke.
	ClassOffline Class = "offline"
	// ClassAuthFailed: the username or password was rejected.
	ClassAuthFailed Class = "auth_failed"
	// ClassLocked: the account is locked after failed attempts; wait for the recorder to unlock it.
	ClassLocked Class = "locked"
	// ClassSession: the session expired or is unknown; logging in again fixes it.
	ClassSession Class = "session"
	// ClassUnsupported: the answer is not an NVMS-9000 response, so the device is not supported.
	ClassUnsupported Class = "unsupported"
	// ClassDegraded: the recorder answered but refused this command.
	ClassDegraded Class = "degraded"
)

// State is the server state a class corresponds to.
func (c Class) State() string {
	switch c {
	case ClassOffline:
		return "offline"
	case ClassAuthFailed, ClassLocked:
		return "auth_failed"
	case ClassUnsupported:
		return "unsupported"
	default:
		return "degraded"
	}
}

// Error is a failed call. Its text never contains the password, the token or the nonce.
type Error struct {
	Class   Class
	Command string
	// Code is the recorder's <errorCode>, zero when the failure was not an NVMS answer.
	Code int
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("provision NVR %s: %s: %v", e.Command, e.Class, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

// ClassOf returns the class of an error returned by this package, and false for any other error.
func ClassOf(err error) (Class, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Class, true
	}
	return "", false
}

// redactTransport drops the request URL that net/http puts in its errors, keeping the cause.
func redactTransport(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
