// Package ecpclient is a Go client for the Engram Context Protocol (ECP), over
// HTTP (an ECP server's /ecp endpoint with a bearer token) or stdio (a server
// process such as "engram serve").
//
//	c, err := ecpclient.DialHTTP(ctx, "http://127.0.0.1:7079/ecp", token, ecpclient.Info{Name: "my-tool"})
//	res, err := c.Observe(ctx, ecpclient.ObserveParams{Kind: "fact", Content: "Tabs over spaces"})
//	hits, err := c.Recall(ctx, ecpclient.RecallParams{Query: "indentation"})
//
// Server notifications arrive on Notifications() after Listen (HTTP) or as soon
// as they are sent (stdio).
package ecpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the ECP version this client speaks.
const ProtocolVersion = "2026-11-01"

// Error is an ECP error response.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("ecp error %d: %s", e.Code, e.Message) }

// Error codes from the spec.
const (
	CodeParseError         = -32700
	CodeInvalidRequest     = -32600
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
	CodeNotInitialized     = -32001
	CodeUnsupportedVersion = -32002
	CodeUnauthorized       = -32003
	CodeScopeDenied        = -32004
	CodeMemoryNotFound     = -32005
	CodeInvalidProvenance  = -32006
	CodeRateLimited        = -32007
	CodeContentRejected    = -32008
)

// IsCode reports whether err is an ECP error with the given code.
func IsCode(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Notification is a server-pushed message.
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Info identifies the client and what notifications it wants.
type Info struct {
	Name, Version, Agent string
	Relevant, Conflict   bool // notifications to receive
	Consolidated         bool
}

// transport sends one JSON-RPC payload and returns the response payload (nil for notifications).
type transport interface {
	roundTrip(ctx context.Context, payload []byte, expectReply bool) ([]byte, error)
	close() error
}

// Client is an ECP connection.
type Client struct {
	t      transport
	nextID atomic.Int64
	notes  chan Notification
	Init   InitializeResult
	http   *httpTransport // set for HTTP connections
}

// Notifications returns server notifications. The channel is buffered; when it
// is full, new notifications are dropped rather than blocking the connection.
func (c *Client) Notifications() <-chan Notification { return c.notes }

func (c *Client) deliver(n Notification) {
	select {
	case c.notes <- n:
	default:
	}
}

// Close ends the connection.
func (c *Client) Close() error { return c.t.close() }

// Call sends a request and decodes its result into out (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	raw, err := c.CallRaw(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// CallRaw sends a request and returns the raw result.
func (c *Client) CallRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	resp, err := c.t.roundTrip(ctx, b, true)
	if err != nil {
		return nil, err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

// Notify sends a notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	_, err := c.t.roundTrip(ctx, b, false)
	return err
}

// Send sends raw bytes and returns the raw reply (for conformance testing).
func (c *Client) Send(ctx context.Context, raw []byte, expectReply bool) ([]byte, error) {
	return c.t.roundTrip(ctx, raw, expectReply)
}

// Initialize performs the ECP handshake. Dial functions call it for you; use it
// directly only with a connection from NewHTTP or StartCommand.
func (c *Client) Initialize(ctx context.Context, info Info) error {
	params := map[string]any{
		"protocolVersion":   ProtocolVersion,
		"supportedVersions": []string{ProtocolVersion},
		"clientInfo":        map[string]string{"name": info.Name, "version": info.Version, "agent": info.Agent},
		"capabilities": map[string]any{"notifications": map[string]bool{
			"relevant": info.Relevant, "conflict": info.Conflict, "consolidated": info.Consolidated}},
	}
	if err := c.Call(ctx, "initialize", params, &c.Init); err != nil {
		return err
	}
	return c.Notify(ctx, "notifications/initialized", nil)
}

// ---------------------------------------------------------------- HTTP

type httpTransport struct {
	url, token string
	hc         *http.Client
	mu         sync.Mutex
	session    string
}

func (t *httpTransport) roundTrip(ctx context.Context, payload []byte, expectReply bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.token)
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Ecp-Session-Id", t.session)
	}
	t.mu.Unlock()
	res, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if sid := res.Header.Get("Ecp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	switch {
	case res.StatusCode == http.StatusAccepted:
		return nil, nil
	case res.StatusCode == http.StatusUnauthorized:
		var r struct{ Error *Error }
		if json.Unmarshal(body, &r) == nil && r.Error != nil {
			return nil, r.Error
		}
		return nil, &Error{Code: CodeUnauthorized, Message: "unauthorized"}
	case res.StatusCode >= 300:
		return nil, fmt.Errorf("http %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	if !expectReply {
		return nil, nil
	}
	return body, nil
}

func (t *httpTransport) close() error {
	t.mu.Lock()
	sid := t.session
	t.mu.Unlock()
	if sid == "" {
		return nil
	}
	req, _ := http.NewRequest("DELETE", t.url, nil)
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Ecp-Session-Id", sid)
	res, err := t.hc.Do(req)
	if err == nil {
		res.Body.Close()
	}
	return err
}

// DialHTTP connects to an ECP server's HTTP endpoint (such as
// http://127.0.0.1:7079/ecp) with a bearer token and initializes.
func DialHTTP(ctx context.Context, url, token string, info Info) (*Client, error) {
	c := NewHTTP(url, token)
	if err := c.Initialize(ctx, info); err != nil {
		return nil, err
	}
	return c, nil
}

// NewHTTP returns an HTTP connection that has not initialized yet.
func NewHTTP(url, token string) *Client {
	t := &httpTransport{url: url, token: token, hc: http.DefaultClient}
	return &Client{t: t, notes: make(chan Notification, 64), http: t}
}

// Listen opens the HTTP notification stream and delivers notifications until ctx
// ends. It returns once the stream is open. Stdio connections need no Listen.
func (c *Client) Listen(ctx context.Context) error {
	t := c.http
	if t == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", t.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+t.token)
	t.mu.Lock()
	req.Header.Set("Ecp-Session-Id", t.session)
	t.mu.Unlock()
	res, err := t.hc.Do(req)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return fmt.Errorf("event stream: http %d", res.StatusCode)
	}
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var n Notification
				if json.Unmarshal([]byte(data), &n) == nil && n.Method != "" {
					c.deliver(n)
				}
			}
		}
	}()
	return nil
}

// ---------------------------------------------------------------- stdio

type stdioTransport struct {
	w       io.Writer
	closer  func() error
	wmu     sync.Mutex
	mu      sync.Mutex
	waiting map[string]chan []byte
	done    chan struct{}
	err     error
}

func (t *stdioTransport) readLoop(r io.Reader, c *Client) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if len(m.ID) == 0 && m.Method != "" {
			c.deliver(Notification{Method: m.Method, Params: m.Params})
			continue
		}
		t.mu.Lock()
		ch := t.waiting[string(m.ID)]
		delete(t.waiting, string(m.ID))
		t.mu.Unlock()
		if ch != nil {
			ch <- line
		}
	}
	t.err = sc.Err()
	if t.err == nil {
		t.err = io.EOF
	}
	close(t.done)
}

func (t *stdioTransport) roundTrip(ctx context.Context, payload []byte, expectReply bool) ([]byte, error) {
	var ch chan []byte
	if expectReply {
		var m struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(payload, &m)
		ch = make(chan []byte, 1)
		t.mu.Lock()
		t.waiting[string(m.ID)] = ch
		t.mu.Unlock()
	}
	t.wmu.Lock()
	_, err := t.w.Write(append(payload, '\n'))
	t.wmu.Unlock()
	if err != nil || !expectReply {
		return nil, err
	}
	select {
	case b := <-ch:
		return b, nil
	case <-t.done:
		return nil, fmt.Errorf("connection closed: %w", t.err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *stdioTransport) close() error {
	if t.closer != nil {
		return t.closer()
	}
	return nil
}

// NewStdio speaks ECP over a reader and writer (for example a server's stdout and
// stdin) and initializes.
func NewStdio(ctx context.Context, r io.Reader, w io.Writer, closer func() error, info Info) (*Client, error) {
	c := NewStdioRaw(r, w, closer)
	if err := c.Initialize(ctx, info); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// NewStdioRaw returns a stdio connection that has not initialized yet.
func NewStdioRaw(r io.Reader, w io.Writer, closer func() error) *Client {
	t := &stdioTransport{w: w, closer: closer, waiting: map[string]chan []byte{}, done: make(chan struct{})}
	c := &Client{t: t, notes: make(chan Notification, 64)}
	go t.readLoop(r, c)
	return c
}

// DialCommand starts a server process (such as "engram serve --client NAME") and
// speaks ECP over its stdio.
func DialCommand(ctx context.Context, name string, args []string, info Info) (*Client, error) {
	c, err := StartCommand(name, args)
	if err != nil {
		return nil, err
	}
	if err := c.Initialize(ctx, info); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// StartCommand starts a server process and returns a connection that has not
// initialized yet.
func StartCommand(name string, args []string) (*Client, error) {
	cmd := exec.Command(name, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	closer := func() error {
		_ = in.Close()
		return cmd.Wait()
	}
	return NewStdioRaw(out, in, closer), nil
}

// ---------------------------------------------------------------- methods

// Observe writes a memory.
func (c *Client) Observe(ctx context.Context, p ObserveParams) (ObserveResult, error) {
	var r ObserveResult
	return r, c.Call(ctx, "engram/observe", p, &r)
}

// Recall retrieves related memories.
func (c *Client) Recall(ctx context.Context, p RecallParams) ([]ScoredMemory, error) {
	var r struct {
		Memories []ScoredMemory `json:"memories"`
	}
	return r.Memories, c.Call(ctx, "engram/recall", p, &r)
}

// Get fetches memories by id.
func (c *Client) Get(ctx context.Context, ids ...string) ([]Memory, error) {
	var r struct {
		Memories []Memory `json:"memories"`
	}
	return r.Memories, c.Call(ctx, "engram/get", map[string]any{"ids": ids}, &r)
}

// Path returns the best chain between two memories; rule is "maximin" or "most_probable".
func (c *Client) Path(ctx context.Context, from, to, rule string) (PathResult, error) {
	var r PathResult
	return r, c.Call(ctx, "engram/path", map[string]any{"from": from, "to": to, "rule": rule}, &r)
}

// Attend returns what matters now for a context.
func (c *Client) Attend(ctx context.Context, p AttendParams) (AttendResult, error) {
	var r AttendResult
	return r, c.Call(ctx, "engram/attend", p, &r)
}

// Feedback reports how recalled memories worked out.
func (c *Client) Feedback(ctx context.Context, p FeedbackParams) (FeedbackResult, error) {
	var r FeedbackResult
	return r, c.Call(ctx, "engram/feedback", p, &r)
}

// Forget deletes memories and, with cascade, what was derived only from them.
func (c *Client) Forget(ctx context.Context, cascade bool, ids ...string) (int, error) {
	var r struct {
		Removed int `json:"removed"`
	}
	return r.Removed, c.Call(ctx, "engram/forget", map[string]any{"ids": ids, "cascade": cascade}, &r)
}

// Subscribe registers interest; notifications/engram/relevant follow.
func (c *Client) Subscribe(ctx context.Context, p SubscribeParams) (string, error) {
	var r struct {
		SubscriptionID string `json:"subscriptionId"`
	}
	return r.SubscriptionID, c.Call(ctx, "engram/subscribe", p, &r)
}

// Unsubscribe cancels a subscription.
func (c *Client) Unsubscribe(ctx context.Context, id string) error {
	return c.Call(ctx, "engram/unsubscribe", map[string]string{"subscriptionId": id}, nil)
}

// Consolidate runs a sleep cycle and returns the server's report.
func (c *Client) Consolidate(ctx context.Context, light bool) (json.RawMessage, error) {
	return c.CallRaw(ctx, "engram/consolidate", map[string]any{"light": light})
}

// Checkpoint saves a task; omit SessionID to start one.
func (c *Client) Checkpoint(ctx context.Context, cp Checkpoint) (Checkpoint, error) {
	var r Checkpoint
	return r, c.Call(ctx, "engram/session/checkpoint", cp, &r)
}

// Resume loads a task and reports what this client lacks.
func (c *Client) Resume(ctx context.Context, sessionID string, have *Environment) (ResumeResult, error) {
	var r ResumeResult
	return r, c.Call(ctx, "engram/session/resume", map[string]any{"sessionId": sessionID, "environment": have}, &r)
}

// ListSessions lists tasks; status is "open", "done" or "".
func (c *Client) ListSessions(ctx context.Context, status string) ([]Checkpoint, error) {
	var r struct {
		Sessions []Checkpoint `json:"sessions"`
	}
	return r.Sessions, c.Call(ctx, "engram/session/list", map[string]string{"status": status}, &r)
}

// Delegate hands a subtask to another agent.
func (c *Client) Delegate(ctx context.Context, sessionID, subtask, toAgent string) (Checkpoint, error) {
	var r Checkpoint
	return r, c.Call(ctx, "engram/session/delegate", map[string]string{"sessionId": sessionID, "subtask": subtask, "toAgent": toAgent}, &r)
}
