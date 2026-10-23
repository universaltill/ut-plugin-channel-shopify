//go:build !wasip1

// Package conformance is the sales-channel conformance suite
// (ut-docs reference/contracts/sales-channel-v1.md section 6) and the host
// emulator it runs an adapter on.
//
// The emulator is NOT the cloud channel host. It plays the host's side of
// the contract that an adapter can observe: the single network path
// (channel_http) with its egress rules, credential injection and
// redaction, the retry on 429, the per-call caps, plus the SDK's FakeHost
// for cursors, submit, the outbox and log. The sandbox limits themselves
// (memory, deadline, no environment, the IP rules) are enforced and tested
// by ut-cloud internal/channelhost (TestEgress_*, TestHostile_*); the suite
// checks what the ADAPTER does under those rules: it never tries to leave
// {account}, never touches the credential, and stays inside the caps.
package conformance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Contract caps (sales-channel-v1.md section 3.1).
const (
	MaxHTTPCallsPerAsk = 2000
	MaxRecordsPerAsk   = 20000
	MaxRequestBody     = 4 << 20
	MaxResponseBody    = 8 << 20
	MaxAttempts        = 6
)

// Seen is one channel_http request as the adapter made it, recorded before
// the emulator's own checks.
type Seen struct {
	Method   string
	URL      string
	Header   map[string]string
	Body     []byte
	CostHint int  // channel_http cost_hint, the pacing hint (0 = none)
	Denied   bool // the emulator answered -2
}

// TB is the part of testing.TB the emulator needs, so the suite can also be
// pointed at a deliberately broken adapter in its own self-tests.
type TB interface {
	Helper()
	Fatal(args ...any)
	Cleanup(func())
}

// Emulator is the host side of one connection.
type Emulator struct {
	T        TB
	Host     *channel.FakeHost
	Handlers channel.Handlers
	Server   http.Handler
	Account  string
	// CredentialHeader and Credential: the host injects the credential on
	// every call and scrubs it from every answer. The adapter never has it.
	CredentialHeader string
	Credential       string

	mu        sync.Mutex
	seen      []Seen
	httpCalls int // channel_http calls in the current ask
}

// NewEmulator installs a fresh FakeHost for t. server is the channel's fake
// server; handlers the adapter's asks.
func NewEmulator(t TB, handlers channel.Handlers, server http.Handler, account, credHeader, credential string) *Emulator {
	t.Helper()
	e := &Emulator{T: t, Host: channel.NewFakeHost(), Handlers: handlers, Server: server, Account: account, CredentialHeader: credHeader, Credential: credential}
	e.Host.HTTP = e.http
	channel.UseFakeHost(t, e.Host)
	return e
}

// Seen returns every request the adapter made.
func (e *Emulator) Seen() []Seen {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Seen(nil), e.seen...)
}

// HTTPCalls is the number of channel_http calls in the latest ask.
func (e *Emulator) HTTPCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.httpCalls
}

// Result of one ask.
type Result struct {
	Answer  []byte
	Err     error
	Elapsed time.Duration
}

// errKilled is what Ask reports when the sandbox would have stopped the
// guest: it kept calling channel_http long after the host's cap answered -5.
type errKilled struct{ reason string }

func (e errKilled) Error() string { return "killed by the host: " + e.reason }

// Ask delivers one ask (an Ask* constant) to the adapter, as the host does:
// the event on stdin, the answer from stdout. The per-ask caps restart.
func (e *Emulator) Ask(typ string, payload any) Result {
	e.T.Helper()
	e.mu.Lock()
	e.httpCalls = 0
	e.mu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		e.T.Fatal(err)
	}
	ev, _ := json.Marshal(channel.Event{ID: "evt-1", Type: typ, Payload: raw})
	start := time.Now()
	var out []byte
	func() {
		defer func() {
			if k := recover(); k != nil {
				if kill, ok := k.(errKilled); ok {
					err = kill
					return
				}
				panic(k)
			}
		}()
		out, err = channel.Dispatch(e.Handlers, ev)
	}()
	return Result{Answer: out, Err: err, Elapsed: time.Since(start)}
}

// expand fills the connection's account into the exact prefix
// "https://{account}" (ut-cloud channelhost expandAccount): a plugin is not
// told its account and writes the manifest's own placeholder.
func (e *Emulator) expand(raw string) string {
	rest, ok := strings.CutPrefix(raw, "https://{account}")
	if !ok || (rest != "" && rest[0] != '/' && rest[0] != '?' && rest[0] != '#') {
		return raw
	}
	return "https://" + e.Account + rest
}

// graphqlThrottled reports a GraphQL THROTTLED error (errors[].extensions.code)
// in a 200 answer: the host retries it like a 429 (contract section 3.1).
func graphqlThrottled(resp *httptest.ResponseRecorder) bool {
	if resp.Code != http.StatusOK {
		return false
	}
	var env struct {
		Errors []struct {
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal(resp.Body.Bytes(), &env) != nil {
		return false
	}
	for _, e := range env.Errors {
		if e.Extensions.Code == "THROTTLED" {
			return true
		}
	}
	return false
}

var forbiddenRequestHeaders = map[string]bool{
	"accept-encoding": true, "range": true, "if-range": true, "via": true, "origin": true, "cookie": true,
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true, "forwarded": true,
	"x-forwarded-for": true, "x-forwarded-host": true, "x-forwarded-proto": true, "x-real-ip": true,
}

// http is channel_http: https to the connection's account host on 443 only,
// GET HEAD POST PUT PATCH only (no DELETE), the credential added by the host,
// 429 and GraphQL THROTTLED retried up to MaxAttempts, bodies capped, the credential scrubbed from
// the answer.
func (e *Emulator) http(req channel.HTTPRequest) (channel.HTTPResponse, error) {
	e.mu.Lock()
	e.httpCalls++
	over := e.httpCalls > MaxHTTPCallsPerAsk
	rec := Seen{Method: req.Method, URL: e.expand(req.URL), Header: req.Headers, Body: req.Body, CostHint: req.CostHint}
	e.mu.Unlock()
	deny := func() (channel.HTTPResponse, error) {
		rec.Denied = true
		e.mu.Lock()
		e.seen = append(e.seen, rec)
		e.mu.Unlock()
		return channel.HTTPResponse{}, channel.ErrDenied
	}
	if over {
		if e.httpCalls > MaxHTTPCallsPerAsk+10 {
			panic(errKilled{reason: "channel_http calls past the cap of 2000"})
		}
		return channel.HTTPResponse{}, channel.ErrQuota
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return deny()
	}
	u, err := url.Parse(e.expand(req.URL))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() != e.Account || (u.Port() != "" && u.Port() != "443") {
		return deny()
	}
	if len(req.Body) > MaxRequestBody {
		return channel.HTTPResponse{}, channel.ErrInvalid
	}
	e.mu.Lock()
	e.seen = append(e.seen, rec)
	e.mu.Unlock()

	var resp *httptest.ResponseRecorder
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		r := httptest.NewRequest(method, u.RequestURI(), bytes.NewReader(req.Body))
		r.Host = u.Host
		for k, v := range req.Headers {
			if strings.EqualFold(k, e.CredentialHeader) || forbiddenRequestHeaders[strings.ToLower(k)] {
				continue
			}
			r.Header.Set(k, v)
		}
		r.Header.Set(e.CredentialHeader, e.Credential)
		resp = httptest.NewRecorder()
		e.Server.ServeHTTP(resp, r)
		if resp.Code != http.StatusTooManyRequests && !graphqlThrottled(resp) {
			break
		}
	}
	if resp.Code == http.StatusTooManyRequests || graphqlThrottled(resp) {
		return channel.HTTPResponse{}, channel.ErrQuota // dead-lettered after six attempts
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) > MaxResponseBody {
		return channel.HTTPResponse{}, channel.ErrQuota
	}
	body = bytes.ReplaceAll(body, []byte(e.Credential), nil)
	hdr := map[string]string{}
	for k := range resp.Header() {
		hdr[k] = strings.ReplaceAll(resp.Header().Get(k), e.Credential, "")
	}
	return channel.HTTPResponse{Status: resp.Code, Headers: hdr, Body: body}, nil
}
