// Package onvif is a bounded ONVIF client. Copied from jdolan-exalink/openvms
// (internal/onvif, 2026-11-13, commit 6723888) with the owner's permission;
// probe.go and lan.go are Recorder additions.
package onvif

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- WS-Security UsernameToken PasswordDigest requires SHA-1 for interoperability.
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const soapNamespace = "http://www.w3.org/2003/05/soap-envelope"

var (
	ErrResponseTooLarge = errors.New("ONVIF response exceeds configured limit")
	ErrInvalidEndpoint  = errors.New("invalid ONVIF endpoint")
)

// Endpoint is a validated, credential-free HTTP(S) service URL.
type Endpoint struct{ value string }

// String returns the validated credential-free endpoint URL.
func (e Endpoint) String() string { return e.value }

func ParseEndpoint(raw string) (Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\\\r\n\t") {
		return Endpoint{}, ErrInvalidEndpoint
	}
	return Endpoint{value: u.String()}, nil
}

type Operation uint8

const (
	ReadOnly Operation = iota
	Mutating
)

type Status string

const (
	StatusSupported   Status = "supported"
	StatusUnsupported Status = "unsupported"
	StatusDegraded    Status = "degraded"
	StatusFailed      Status = "failed"
)

// CallError exposes stable, redacted failure details while retaining the cause for errors.Is.
type CallError struct {
	Status Status
	Code   string
	cause  error
}

func (e *CallError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("ONVIF call %s: %s", e.Status, e.Code)
	}
	return fmt.Sprintf("ONVIF call %s", e.Status)
}
func (e *CallError) Unwrap() error { return e.cause }

type Config struct {
	Timeout      time.Duration
	MaxBodyBytes int64
	MaxAttempts  int
	RetryDelay   time.Duration
	Credentials  *Credentials
	TimeOffset   time.Duration
	nonceSource  io.Reader
	now          func() time.Time
}

// Credentials holds an ONVIF UsernameToken identity without exposing the password
// through formatting or JSON serialization. Keep the original password in memory only.
type Credentials struct{ username, password string }

func NewCredentials(username, password string) *Credentials {
	return &Credentials{username: username, password: password}
}

func (*Credentials) String() string   { return "ONVIF credentials" }
func (*Credentials) GoString() string { return "onvif.Credentials{REDACTED}" }

type Client struct {
	transport   http.RoundTripper
	config      Config
	nonceSource io.Reader
	now         func() time.Time
}

func NewClient(transport http.RoundTripper, config Config) *Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 1 << 20
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 1
	}
	if config.MaxAttempts > 3 {
		config.MaxAttempts = 3
	}
	if config.RetryDelay < 0 {
		config.RetryDelay = 0
	}
	nonceSource := config.nonceSource
	if nonceSource == nil {
		nonceSource = rand.Reader
	}
	now := config.now
	if now == nil {
		now = time.Now
	}
	return &Client{transport: transport, config: config, nonceSource: nonceSource, now: now}
}

func (c *Client) SetTimeOffset(offset time.Duration) {
	c.config.TimeOffset = offset
}

func (c *Client) TimeOffset() time.Duration {
	return c.config.TimeOffset
}

const (
	// #nosec G101 -- This is the required WS-Security PasswordDigest profile URI, not a credential.
	passwordDigestType = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest"
	base64BinaryType   = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary"
)

type securityHeader struct {
	XMLName        xml.Name      `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Security"`
	MustUnderstand string        `xml:"http://www.w3.org/2003/05/soap-envelope mustUnderstand,attr"`
	Token          usernameToken `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd UsernameToken"`
}
type usernameToken struct {
	Username string          `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Username"`
	Password passwordElement `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Password"`
	Nonce    nonceElement    `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Nonce"`
	Created  createdElement  `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd Created"`
}
type passwordElement struct {
	Type  string `xml:"Type,attr"`
	Value string `xml:",chardata"`
}
type nonceElement struct {
	EncodingType string `xml:"EncodingType,attr"`
	Value        string `xml:",chardata"`
}
type createdElement struct {
	Value string `xml:",chardata"`
}

func (c *Client) requestBody(operationXML string) ([]byte, error) {
	body := []byte(`<s:Envelope xmlns:s="` + soapNamespace + `"><s:Body>` + operationXML + `</s:Body></s:Envelope>`)
	if c.config.Credentials == nil {
		return body, nil
	}
	creds := c.config.Credentials
	if creds.username == "" {
		return nil, errors.New("invalid ONVIF credentials")
	}
	var nonce [20]byte
	if _, err := io.ReadFull(c.nonceSource, nonce[:]); err != nil {
		return nil, errors.New("ONVIF authentication nonce unavailable")
	}
	created := c.now().Add(c.config.TimeOffset).UTC().Format("2006-01-02T15:04:05Z")
	// #nosec G401 -- ONVIF UsernameToken PasswordDigest is specified as SHA-1; this is not used for signatures.
	hash := sha1.New()
	_, _ = hash.Write(nonce[:])
	_, _ = io.WriteString(hash, created)
	_, _ = io.WriteString(hash, creds.password)
	security := securityHeader{MustUnderstand: "true", Token: usernameToken{
		Username: creds.username,
		Password: passwordElement{Type: passwordDigestType, Value: base64.StdEncoding.EncodeToString(hash.Sum(nil))},
		Nonce:    nonceElement{EncodingType: base64BinaryType, Value: base64.StdEncoding.EncodeToString(nonce[:])},
		Created:  createdElement{Value: created},
	}}
	securityXML, err := xml.Marshal(security)
	if err != nil {
		return nil, errors.New("ONVIF authentication header failed")
	}
	// XML marshaling handles username special characters and assigns valid namespace prefixes.
	header := `<s:Header>` + string(securityXML) + `</s:Header>`
	return bytes.Replace(body, []byte(`<s:Body>`), []byte(header+`<s:Body>`), 1), nil
}

type soapEnvelope struct {
	XMLName xml.Name `xml:"http://www.w3.org/2003/05/soap-envelope Envelope"`
	Body    soapBody `xml:"http://www.w3.org/2003/05/soap-envelope Body"`
}
type soapBody struct {
	Inner string `xml:",innerxml"`
}
type soapFault struct {
	Code struct {
		Value   string `xml:"Value"`
		Subcode struct {
			Value string `xml:"Value"`
		} `xml:"Subcode"`
	} `xml:"Code"`
	Reason struct {
		Text string `xml:"Text"`
	} `xml:"Reason"`
}

// Call performs one SOAP operation. Only explicitly read-only calls may retry transport or 5xx failures.
func (c *Client) Call(ctx context.Context, endpoint Endpoint, action, operationXML string, operation Operation) ([]byte, error) {
	if endpoint.value == "" {
		return nil, ErrInvalidEndpoint
	}
	if action == "" || strings.ContainsAny(action, "\r\n\"") {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_action"}
	}
	if _, err := ParseEndpoint(endpoint.value); err != nil {
		return nil, err
	}
	if _, err := xml.Marshal(soapBody{Inner: operationXML}); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_request_xml"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	attempts := 1
	if operation == ReadOnly {
		attempts = c.config.MaxAttempts
	}
	var last error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, bodyErr := c.requestBody(operationXML)
		if bodyErr != nil {
			return nil, &CallError{Status: StatusFailed, Code: "authentication_failed"}
		}
		var requestEnvelope soapEnvelope
		if err := xml.Unmarshal(body, &requestEnvelope); err != nil {
			return nil, &CallError{Status: StatusFailed, Code: "invalid_request_xml"}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.value, bytes.NewReader(body))
		if err != nil {
			return nil, &CallError{Status: StatusFailed, Code: "request_failed", cause: err}
		}
		req.Header.Set("Content-Type", `application/soap+xml; charset=utf-8; action="`+action+`"`)
		resp, err := c.transport.RoundTrip(req)
		if err != nil {
			last = err
			if i+1 < attempts && wait(ctx, c.config.RetryDelay) {
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &CallError{Status: StatusDegraded, Code: "transport_error", cause: err}
		}
		data, readErr := readBounded(resp.Body, c.config.MaxBodyBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &CallError{Status: StatusFailed, Code: "authentication_failed"}
		}
		var envelope soapEnvelope
		soapErr := xml.Unmarshal(data, &envelope)
		if soapErr != nil || strings.TrimSpace(envelope.Body.Inner) == "" {
			if resp.StatusCode >= 500 && i+1 < attempts && wait(ctx, c.config.RetryDelay) {
				continue
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				status := StatusFailed
				if resp.StatusCode >= 500 {
					status = StatusDegraded
				}
				return nil, &CallError{Status: status, Code: "http_error"}
			}
			return nil, &CallError{Status: StatusFailed, Code: "invalid_soap_response"}
		}
		var fault soapFault
		if err := xml.Unmarshal([]byte(envelope.Body.Inner), &fault); err == nil && (fault.Code.Value != "" || fault.Code.Subcode.Value != "") {
			code := fault.Code.Subcode.Value
			if code == "" {
				code = fault.Code.Value
			}
			status := StatusFailed
			if strings.Contains(strings.ToLower(code), "unsupported") || strings.Contains(strings.ToLower(code), "actionnotsupported") {
				status = StatusUnsupported
			} else if strings.Contains(strings.ToLower(fault.Code.Value), "receiver") {
				status = StatusDegraded
				if operation == ReadOnly && resp.StatusCode >= 500 && i+1 < attempts && wait(ctx, c.config.RetryDelay) {
					continue
				}
			}
			return nil, &CallError{Status: status, Code: "soap_fault"}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode >= 500 && operation == ReadOnly && i+1 < attempts && wait(ctx, c.config.RetryDelay) {
				continue
			}
			status := StatusFailed
			if resp.StatusCode >= 500 {
				status = StatusDegraded
			}
			return nil, &CallError{Status: status, Code: "http_error"}
		}
		// Return the complete SOAP document. Namespace declarations on Envelope
		// or Body are in scope for operation elements and must remain available
		// to encoding/xml decoders.
		return bytes.Clone(data), nil
	}
	return nil, &CallError{Status: StatusDegraded, Code: "transport_error", cause: last}
}

func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "response_read_failed", cause: err}
	}
	if int64(len(b)) > maxBytes {
		return nil, ErrResponseTooLarge
	}
	return b, nil
}

func wait(ctx context.Context, delay time.Duration) bool {
	if delay == 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
