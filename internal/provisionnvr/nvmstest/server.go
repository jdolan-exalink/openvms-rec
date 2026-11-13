// Package nvmstest is a fake NVMS-9000 recorder for tests. It speaks the login handshake and answers the
// read-only queries from fixtures captured on a real recorder (identifiers replaced), so the client and
// the code built on it run without a device.
package nvmstest

import (
	"crypto/md5"    //nolint:gosec // the NVMS login protocol hashes the password with MD5
	"crypto/sha512" // second step of the same handshake
	"embed"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

//go:embed testdata/*.xml
var fixtures embed.FS

const (
	nonce = "{DEADBEEF-0000-4000-8000-000000000001}"
	token = "test-session-token"
)

// Server is the fake recorder. Set the exported fields before the first request.
type Server struct {
	*httptest.Server

	username, password string

	// LockAfter locks the account after this many wrong logins; zero never locks.
	LockAfter int
	// Override replaces the answer of a command with the given body.
	Override map[string]string
	// Offline answers every call with HTTP 500, like a recorder whose web service is down.
	Offline bool
	// OnRequest, when set, sees the command and the raw request body of every call. For tests that check what
	// the client asked.
	OnRequest func(cmd, body string)

	mu           sync.Mutex
	calls        []string
	failedLogins int
	logins       int
	expireNext   bool
}

// New starts a fake recorder that accepts the given account. It is closed when the test ends.
func New(t testing.TB, username, password string) *Server {
	t.Helper()
	s := &Server{username: username, password: password, Override: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// Fixture returns the captured answer for a command.
func Fixture(cmd string) (string, error) {
	b, err := fixtures.ReadFile("testdata/" + cmd + ".xml")
	return string(b), err
}

// Calls lists the commands received, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// FailedLogins is the number of doLogin calls the recorder rejected.
func (s *Server) FailedLogins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failedLogins
}

// Logins is the number of doLogin calls the recorder accepted.
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// SetPassword changes the password the recorder accepts, like an operator changing it on the device.
func (s *Server) SetPassword(password string) {
	s.mu.Lock()
	s.password = password
	s.mu.Unlock()
}

// ExpireSession makes the next authenticated call answer "session timeout", once.
func (s *Server) ExpireSession() {
	s.mu.Lock()
	s.expireNext = true
	s.mu.Unlock()
}

func fail(w io.Writer, code int) {
	_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<response>\n\t<status>fail</status>\n\t<errorCode>%d</errorCode>\n</response>\n", code)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	cmd := strings.TrimPrefix(r.URL.Path, "/")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.calls = append(s.calls, cmd)
	s.mu.Unlock()
	if s.OnRequest != nil {
		s.OnRequest(cmd, string(body))
	}
	if s.Offline {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	var req struct {
		Token    string `xml:"token"`
		UserName string `xml:"content>userName"`
		Password string `xml:"content>password"`
	}
	_ = xml.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/xml")
	switch cmd {
	case "reqLogin":
		_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<response version=\"1.0\" cmdId=\"\" cmdUrl=\"reqLogin\">\n\t<status>success</status>\n\t<content>\n\t\t<sessionId>sess</sessionId>\n\t\t<nonce>%s</nonce>\n\t\t<token>%s</token>\n\t</content>\n</response>\n", nonce, token)
	case "doLogin":
		s.doLogin(w, req.UserName, req.Password)
	case "doLogout":
		_, _ = io.WriteString(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<response><status>success</status></response>\n")
	case "queryActivationStatus":
		s.serve(w, cmd)
	default:
		if !strings.HasPrefix(cmd, "query") {
			fail(w, 536870943)
			return
		}
		s.mu.Lock()
		expire := s.expireNext && req.Token == token
		if expire {
			s.expireNext = false
		}
		s.mu.Unlock()
		switch {
		case expire:
			fail(w, 536871011)
		case req.Token != token:
			fail(w, 536870934)
		default:
			s.serve(w, cmd)
		}
	}
}

func (s *Server) doLogin(w io.Writer, user, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LockAfter > 0 && s.failedLogins >= s.LockAfter {
		fail(w, 536870951)
		return
	}
	sum := md5.Sum([]byte(s.password)) //nolint:gosec // dictated by the NVMS protocol
	want := sha512.Sum512([]byte(strings.ToUpper(hex.EncodeToString(sum[:])) + "#" + nonce))
	switch {
	case user != s.username:
		s.failedLogins++
		fail(w, 536870947)
	case digest != hex.EncodeToString(want[:]):
		s.failedLogins++
		fail(w, 536870948)
	default:
		s.logins++
		_, _ = io.WriteString(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<response version=\"1.0\" cmdId=\"\" cmdUrl=\"doLogin\">\n\t<status>success</status>\n\t<content>\n\t\t<userType>default_admin</userType>\n\t</content>\n</response>\n")
	}
}

func (s *Server) serve(w io.Writer, cmd string) {
	if o, ok := s.Override[cmd]; ok {
		_, _ = io.WriteString(w, o)
		return
	}
	b, err := Fixture(cmd)
	if err != nil {
		fail(w, 536870943)
		return
	}
	_, _ = io.WriteString(w, b)
}
