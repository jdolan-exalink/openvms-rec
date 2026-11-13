// Package provisionnvr talks to Provision-ISR network video recorders that run the TVT NVMS-9000
// platform, through the XML web API their own web client uses (POST http://host/<command>).
//
// The protocol is not published. Everything here follows what the device itself serves and answers:
// the login handshake and the read-only query commands listed in reads.go. Nothing is assumed about
// commands that were not observed. Phase 1 is read-only: Query refuses any command that is not a
// query*, and the client never edits a setting of the recorder.
package provisionnvr

import (
	"bytes"
	"context"
	"crypto/md5"    //nolint:gosec // the NVMS login protocol hashes the password with MD5
	"crypto/sha512" // the second step of the same handshake
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout = 10 * time.Second
	// maxBodyBytes caps a response. The largest one seen on a 16-channel recorder is about 5 KiB.
	maxBodyBytes = 1 << 20

	// Error codes the device returns in <errorCode> (observed on a real recorder).
	codeSessionTimeout = 536871011
	codeBadPassword    = 536870947
	codeBadDigest      = 536870948
	codeUserLocked     = 536870951
	codeNoSession      = 536870934
)

// Credentials is the account used to log in. Its formatting never reveals the password.
type Credentials struct{ username, password string }

// NewCredentials returns the credentials for a recorder account.
func NewCredentials(username, password string) Credentials {
	return Credentials{username: username, password: password}
}

func (Credentials) String() string   { return "provisionnvr.Credentials{REDACTED}" }
func (Credentials) GoString() string { return "provisionnvr.Credentials{REDACTED}" }

// Config tunes a Client. The zero value is usable.
type Config struct {
	// Transport carries the requests; nil uses http.DefaultTransport, which verifies TLS certificates.
	Transport http.RoundTripper
	Timeout   time.Duration
}

// Client is a session with one recorder. It is safe for concurrent use.
type Client struct {
	base  string
	creds Credentials
	http  *http.Client

	mu         sync.Mutex
	token      string
	authFailed bool
}

// NewClient returns a client for the recorder at baseURL ("http://host:port" or "https://host:port").
func NewClient(baseURL string, creds Credentials, cfg Config) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: base URL must be http(s)://host[:port] without credentials", ErrInvalidEndpoint)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:  strings.TrimRight(u.Scheme+"://"+u.Host, "/"),
		creds: creds,
		http:  &http.Client{Transport: cfg.Transport, Timeout: cfg.Timeout, Jar: jar},
	}, nil
}

// response is the envelope every command answers with. The body of a command is read from the same
// bytes by the typed reads.
type response struct {
	XMLName   xml.Name `xml:"response"`
	Status    string   `xml:"status"`
	ErrorCode string   `xml:"errorCode"`
}

// rawCall posts one command and returns the body of a successful answer.
func (c *Client) rawCall(ctx context.Context, cmd, token, content string) ([]byte, error) {
	body := `<?xml version="1.0" encoding="utf-8" ?><request version="1.0" systemType="NVMS-9000" clientType="WEB"><token>` +
		token + `</token>` + content + `</request>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+cmd, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Class: ClassOffline, Command: cmd, Err: redactTransport(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, &Error{Class: ClassOffline, Command: cmd, Err: redactTransport(err)}
	}
	if len(raw) > maxBodyBytes {
		return nil, &Error{Class: ClassUnsupported, Command: cmd, Err: errors.New("response is larger than the allowed size")}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Class: ClassUnsupported, Command: cmd, Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	var env response
	// encoding/xml neither fetches external entities nor expands custom ones, so a DOCTYPE in the
	// answer has no effect (see TestResponseWithEntityDeclarationIsInert).
	if err := xml.NewDecoder(bytes.NewReader(raw)).Decode(&env); err != nil {
		return nil, &Error{Class: ClassUnsupported, Command: cmd, Err: errors.New("the answer is not an NVMS response")}
	}
	if env.Status == "" {
		return nil, &Error{Class: ClassUnsupported, Command: cmd, Err: errors.New("the answer is not an NVMS response")}
	}
	if env.Status != "success" {
		code, _ := strconv.Atoi(strings.TrimSpace(env.ErrorCode))
		return nil, &Error{Class: classOfCode(code), Command: cmd, Code: code, Err: fmt.Errorf("the recorder answered %q (code %d)", env.Status, code)}
	}
	return raw, nil
}

func classOfCode(code int) Class {
	switch code {
	case codeBadPassword, codeBadDigest:
		return ClassAuthFailed
	case codeUserLocked:
		return ClassLocked
	case codeSessionTimeout, codeNoSession:
		return ClassSession
	default:
		return ClassDegraded
	}
}

// Login opens a session. A rejected username or password is remembered: the client then refuses to
// log in again, because every wrong attempt counts toward the recorder's account lockout. Build a
// new Client (with the corrected credentials) to try again.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	if c.authFailed {
		return &Error{Class: ClassAuthFailed, Command: "doLogin", Err: errors.New("the credentials were rejected earlier; not retrying to avoid locking the account")}
	}
	raw, err := c.rawCall(ctx, "reqLogin", "null", "")
	if err != nil {
		return err
	}
	var hs struct {
		Nonce string `xml:"content>nonce"`
		Token string `xml:"content>token"`
	}
	if err := xml.Unmarshal(raw, &hs); err != nil || hs.Nonce == "" || hs.Token == "" {
		return &Error{Class: ClassUnsupported, Command: "reqLogin", Err: errors.New("the handshake answer has no nonce or token")}
	}
	// password = sha512_hex( MD5_hex_UPPERCASE(password) + "#" + nonce ), nonce as returned (braces included).
	sum := md5.Sum([]byte(c.creds.password)) //nolint:gosec // dictated by the NVMS protocol
	digest := sha512.Sum512([]byte(strings.ToUpper(hex.EncodeToString(sum[:])) + "#" + hs.Nonce))
	content := "<content><userName><![CDATA[" + cdataSafe(c.creds.username) + "]]></userName><password><![CDATA[" +
		hex.EncodeToString(digest[:]) + "]]></password></content>"
	if _, err := c.rawCall(ctx, "doLogin", hs.Token, content); err != nil {
		var e *Error
		if errors.As(err, &e) && (e.Class == ClassAuthFailed || e.Class == ClassLocked) {
			c.authFailed = true
		}
		return err
	}
	c.token = hs.Token
	return nil
}

// cdataSafe keeps a value from closing its CDATA section.
func cdataSafe(s string) string { return strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>") }

// Logout ends the session. It is best effort: a failure is returned but changes nothing else.
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	token := c.token
	c.token = ""
	c.mu.Unlock()
	if token == "" {
		return nil
	}
	_, err := c.rawCall(ctx, "doLogout", token, "")
	return err
}

// Query runs one read-only command (its name starts with "query") and returns the answer. It logs in
// when there is no session and logs in again once when the session timed out.
func (c *Client) Query(ctx context.Context, cmd, content string) ([]byte, error) {
	if !strings.HasPrefix(cmd, "query") || strings.ContainsAny(cmd, "/?#& \t\r\n") {
		return nil, fmt.Errorf("%w: %q", ErrNotReadOnly, cmd)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if c.token == "" {
			if err := c.loginLocked(ctx); err != nil {
				return nil, err
			}
		}
		raw, err := c.rawCall(ctx, cmd, c.token, content)
		if err == nil {
			return raw, nil
		}
		var e *Error
		if attempt == 0 && errors.As(err, &e) && e.Class == ClassSession {
			c.token = ""
			continue
		}
		return nil, err
	}
}

// QueryUnauthenticated runs a query that needs no session (queryActivationStatus).
func (c *Client) QueryUnauthenticated(ctx context.Context, cmd string) ([]byte, error) {
	if cmd != "queryActivationStatus" {
		return nil, fmt.Errorf("%w: %q needs a session", ErrNotReadOnly, cmd)
	}
	return c.rawCall(ctx, cmd, "null", "")
}
