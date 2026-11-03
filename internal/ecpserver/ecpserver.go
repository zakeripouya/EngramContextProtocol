// Package ecpserver serves the native Engram Context Protocol (milestone M11;
// docs/spec/ecp-v0.md): JSON-RPC 2.0 engram/* methods with initialize and
// capability negotiation, over stdio and over HTTP, with server-pushed
// notifications (relevant, conflict, consolidated).
//
// The MCP compatibility profile (internal/server) and native ECP share the same
// store, grants and learning; they differ only in the wire surface.
package ecpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/llm"
	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// SupportedVersions are the ECP versions this server speaks, newest first.
var SupportedVersions = []string{ecp.ProtocolVersion}

// Options configure a server.
type Options struct {
	Version     string          // server version reported in initialize
	Chat        llm.Chat        // language model for engram/consolidate; nil skips distillation
	NewChat     func() llm.Chat // or: connect one lazily, on the first consolidate
	SnapshotDir string          // where engram/consolidate snapshots before changing anything
	PollEvery   time.Duration   // how often notifications and events are checked; default 2s
	RateWindow  time.Duration   // minimum time between relevant pushes per subscription; default 30s
}

// Server is a native ECP server over one store.
type Server struct {
	st *store.Store
	o  Options

	mu       sync.Mutex
	sessions map[string]*Session
}

// New creates a server.
func New(st *store.Store, o Options) *Server {
	if o.PollEvery <= 0 {
		o.PollEvery = 2 * time.Second
	}
	if o.RateWindow <= 0 {
		o.RateWindow = 30 * time.Second
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	return &Server{st: st, o: o, sessions: map[string]*Session{}}
}

// Session is one client connection.
type Session struct {
	ID string

	mu          sync.Mutex
	client      string
	fixed       bool // identity set by the transport (token or --client), not by clientInfo
	service     bool // a headless program: writes default to author "system"
	initialized bool
	notify      struct{ relevant, conflict, consolidated bool }
	subs        map[string]time.Time // subscription id → last relevant push
	lastEvent   int64
	send        func([]byte) error // nil while no stream is attached
	queue       [][]byte           // notifications held until a stream attaches (HTTP)
	lastSeen    time.Time
	end         chan struct{} // closed when the session ends, to release its stream
	endOnce     sync.Once
}

// NewSession registers a session. client and service are set when the transport
// authenticated the caller; otherwise initialize takes the name from clientInfo.
func (s *Server) NewSession(client string, service bool) *Session {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	sess := &Session{ID: hex.EncodeToString(b), client: client, fixed: client != "", service: service,
		subs: map[string]time.Time{}, lastSeen: time.Now(), end: make(chan struct{})}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	return sess
}

// Session returns a registered session.
func (s *Server) Session(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// Close ends a session and drops its subscriptions.
func (s *Server) Close(sess *Session) {
	s.mu.Lock()
	delete(s.sessions, sess.ID)
	s.mu.Unlock()
	sess.mu.Lock()
	ids := make([]string, 0, len(sess.subs))
	for id := range sess.subs {
		ids = append(ids, id)
	}
	sess.send = nil
	sess.mu.Unlock()
	sess.endOnce.Do(func() { close(sess.end) })
	s.st.DropSubscriptions(ids)
}

// Client returns the session's identity.
func (sess *Session) Client() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.client
}

// Attach sets the function that writes notifications to the client and flushes held ones.
func (sess *Session) Attach(send func([]byte) error) {
	sess.mu.Lock()
	sess.send = send
	queued := sess.queue
	sess.queue = nil
	sess.mu.Unlock()
	for _, m := range queued {
		if send(m) != nil {
			return
		}
	}
}

// Detach removes the notification stream (the session stays).
func (sess *Session) Detach() {
	sess.mu.Lock()
	sess.send = nil
	sess.mu.Unlock()
}

func (sess *Session) push(method string, params any) {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return
	}
	sess.mu.Lock()
	send := sess.send
	if send == nil {
		if len(sess.queue) < 200 {
			sess.queue = append(sess.queue, b)
		}
		sess.mu.Unlock()
		return
	}
	sess.mu.Unlock()
	_ = send(b)
}

// ---------------------------------------------------------------- JSON-RPC

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

var nullID = json.RawMessage("null")

// Handle processes one JSON-RPC message or batch and returns the response bytes,
// or nil when nothing is to be sent back (notifications only).
func (s *Server) Handle(ctx context.Context, sess *Session, raw []byte) []byte {
	sess.mu.Lock()
	sess.lastSeen = time.Now()
	sess.mu.Unlock()
	trimmed := []byte(string(raw))
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\r' || trimmed[0] == '\n') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return mustJSON(response{JSONRPC: "2.0", ID: nullID, Error: &rpcError{Code: int(ecp.ParseError), Message: "parse error"}})
		}
		if len(batch) == 0 {
			return mustJSON(response{JSONRPC: "2.0", ID: nullID, Error: &rpcError{Code: int(ecp.InvalidRequest), Message: "empty batch"}})
		}
		var out []json.RawMessage
		for _, m := range batch {
			if r := s.handleOne(ctx, sess, m); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return mustJSON(out)
	}
	return s.handleOne(ctx, sess, trimmed)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) handleOne(ctx context.Context, sess *Session, raw []byte) []byte {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return mustJSON(response{JSONRPC: "2.0", ID: nullID, Error: &rpcError{Code: int(ecp.ParseError), Message: "parse error"}})
	}
	isNotification := len(req.ID) == 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		if isNotification {
			return nil
		}
		return mustJSON(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: int(ecp.InvalidRequest), Message: "not a JSON-RPC 2.0 request"}})
	}
	result, err := s.dispatch(ctx, sess, req.Method, req.Params)
	if isNotification {
		return nil
	}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if err != nil {
		resp.Error = toRPCError(err)
	} else {
		if result == nil {
			result = struct{}{}
		}
		resp.Result = result
	}
	return mustJSON(resp)
}

func toRPCError(err error) *rpcError {
	var e *ecp.Error
	if errors.As(err, &e) {
		return &rpcError{Code: int(e.Code), Message: e.Message}
	}
	var pe paramsError
	if errors.As(err, &pe) {
		return &rpcError{Code: int(ecp.InvalidParams), Message: pe.Error()}
	}
	return &rpcError{Code: int(ecp.InternalError), Message: err.Error()}
}

type paramsError struct{ err error }

func (p paramsError) Error() string { return "invalid params: " + p.err.Error() }

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return paramsError{err}
	}
	return nil
}

// ---------------------------------------------------------------- methods

// ClientCapabilities are what a client declares.
type ClientCapabilities struct {
	Notifications struct {
		Relevant     bool `json:"relevant"`
		Conflict     bool `json:"conflict"`
		Consolidated bool `json:"consolidated"`
	} `json:"notifications"`
}

// InitializeParams is the first request.
type InitializeParams struct {
	ProtocolVersion   string             `json:"protocolVersion"`
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      ClientCapabilities `json:"capabilities"`
	ClientInfo        struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Agent   string `json:"agent"`
	} `json:"clientInfo"`
}

// ServerCapabilities lists what this server supports.
func ServerCapabilities() map[string]any {
	return map[string]any{
		"observe":     map[string]any{"kinds": []string{"episode", "decision", "suggestion", "fact"}},
		"recall":      map[string]any{"spreadingActivation": true, "maxK": 50},
		"attend":      map[string]any{},
		"feedback":    map[string]any{},
		"subscribe":   map[string]any{"topics": false},
		"consolidate": map[string]any{},
		"forget":      map[string]any{"cascade": true},
		"sessions":    map[string]any{},
	}
}

func (s *Server) dispatch(ctx context.Context, sess *Session, method string, params json.RawMessage) (any, error) {
	sess.mu.Lock()
	ready := sess.initialized
	client, service := sess.client, sess.service
	sess.mu.Unlock()

	switch method {
	case "initialize":
		return s.initialize(sess, params)
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return struct{}{}, nil
	}
	if !ready {
		return nil, ecp.Errorf(ecp.NotInitialized, "send initialize first")
	}
	st := s.st
	switch method {
	case "engram/observe":
		var p struct {
			Kind        string          `json:"kind"`
			Content     string          `json:"content"`
			Decision    *store.Decision `json:"decision"`
			Tags        []string        `json:"tags"`
			Scope       string          `json:"scope"`
			Namespace   string          `json:"namespace"`
			Service     string          `json:"service"`
			SessionID   string          `json:"sessionId"`
			Author      string          `json:"author"`
			ValidFrom   string          `json:"validFrom"`
			Supersedes  string          `json:"supersedes"`
			Contradicts []string        `json:"contradicts"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if service && p.Author == "" {
			p.Author = "system" // headless programs write as the system, not as an agent
		}
		res, err := st.Observe(client, store.ObserveInput{Kind: p.Kind, Content: p.Content, Decision: p.Decision, Tags: p.Tags,
			Scope: p.Scope, Namespace: p.Namespace, Service: p.Service, SessionID: p.SessionID, Author: p.Author,
			ValidFrom: p.ValidFrom, Supersedes: p.Supersedes})
		if err != nil {
			return nil, err
		}
		for _, other := range p.Contradicts {
			if err := st.MarkConflict(client, res.ID, other); err != nil {
				return nil, err
			}
		}
		return res, nil

	case "engram/recall":
		var p struct {
			Query        string   `json:"query"`
			K            int      `json:"k"`
			Kinds        []string `json:"kinds"`
			Tags         []string `json:"tags"`
			Namespaces   []string `json:"namespaces"`
			Spread       *int     `json:"spread"`
			AsOf         string   `json:"asOf"`
			IncludePaths bool     `json:"includePaths"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		hops := 2
		if p.Spread != nil {
			hops = max(0, min(*p.Spread, 3))
		}
		res, err := st.Recall(client, store.RecallInput{Query: p.Query, K: p.K, Kinds: p.Kinds, Tags: p.Tags,
			Namespaces: p.Namespaces, AsOf: p.AsOf, Hops: hops})
		if err != nil {
			return nil, err
		}
		if !p.IncludePaths {
			for i := range res {
				res[i].Path = nil
			}
		}
		return map[string]any{"memories": res}, nil

	case "engram/get":
		var p struct {
			IDs []string `json:"ids"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		mems, err := st.Get(client, p.IDs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"memories": mems}, nil

	case "engram/path":
		var p struct {
			From string `json:"from"`
			To   string `json:"to"`
			Rule string `json:"rule"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return st.FindPath(client, p.From, p.To, p.Rule)

	case "engram/attend":
		var p struct {
			Context    string   `json:"context"`
			Goals      []string `json:"goals"`
			K          int      `json:"k"`
			Mode       string   `json:"mode"`
			Namespaces []string `json:"namespaces"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return st.Attend(client, store.AttendInput{Context: p.Context, Goals: p.Goals, K: p.K, Mode: p.Mode, Namespaces: p.Namespaces})

	case "engram/feedback":
		var p struct {
			MemoryIDs  []string        `json:"memoryIds"`
			Outcome    string          `json:"outcome"`
			Objective  string          `json:"objective"`
			Correction *store.Decision `json:"correction"`
			Query      string          `json:"query"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return st.Feedback(client, store.FeedbackInput{MemoryIDs: p.MemoryIDs, Outcome: p.Outcome, Objective: p.Objective,
			Correction: p.Correction, Query: p.Query})

	case "engram/forget":
		var p struct {
			IDs     []string `json:"ids"`
			Cascade *bool    `json:"cascade"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		n, err := st.Forget(client, p.IDs, p.Cascade == nil || *p.Cascade)
		if err != nil {
			return nil, err
		}
		return map[string]int{"removed": n}, nil

	case "engram/subscribe":
		var p struct {
			Context    string   `json:"context"`
			Tags       []string `json:"tags"`
			Namespaces []string `json:"namespaces"`
			Threshold  float64  `json:"threshold"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		sub, err := st.Subscribe(client, store.SubscribeInput{Context: p.Context, Tags: p.Tags, Namespaces: p.Namespaces, Threshold: p.Threshold})
		if err != nil {
			return nil, err
		}
		sess.mu.Lock()
		sess.subs[sub.ID] = time.Time{}
		sess.mu.Unlock()
		return map[string]string{"subscriptionId": sub.ID}, nil

	case "engram/unsubscribe":
		var p struct {
			SubscriptionID string `json:"subscriptionId"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if err := st.Unsubscribe(client, p.SubscriptionID); err != nil {
			return nil, err
		}
		sess.mu.Lock()
		delete(sess.subs, p.SubscriptionID)
		sess.mu.Unlock()
		return struct{}{}, nil

	case "engram/consolidate":
		var p struct {
			Namespace string `json:"namespace"`
			Light     bool   `json:"light"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if p.Namespace != "" {
			return nil, ecp.Errorf(ecp.InvalidParams, "consolidating a single namespace is not supported yet; omit namespace")
		}
		grants, err := st.Grants(client)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(grants, func(g store.Grant) bool { return g.Namespace == "*" && g.CanWrite }) {
			return nil, ecp.Errorf(ecp.ScopeDenied, "consolidation changes every namespace; it needs a write grant on '*' (engram pair %s --write '*')", client)
		}
		chat := s.o.Chat
		if chat == nil && s.o.NewChat != nil && !p.Light {
			chat = s.o.NewChat()
		}
		return sleep.Run(ctx, st, sleep.Options{Light: p.Light, SnapshotDir: s.o.SnapshotDir, Chat: chat})

	case "engram/session/checkpoint":
		var p store.Checkpoint
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return st.SaveCheckpoint(client, p)

	case "engram/session/resume":
		var p struct {
			SessionID   string             `json:"sessionId"`
			Environment *store.Environment `json:"environment"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return st.Resume(client, p.SessionID, p.Environment)

	case "engram/session/list":
		var p struct {
			Status string `json:"status"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		list, err := st.ListSessions(client, p.Status)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sessions": list}, nil

	case "engram/session/delegate":
		var p struct {
			SessionID string `json:"sessionId"`
			Subtask   string `json:"subtask"`
			ToAgent   string `json:"toAgent"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return st.Delegate(client, p.SessionID, p.Subtask, p.ToAgent)
	}
	return nil, ecp.Errorf(ecp.MethodNotFound, "unknown method %q", method)
}

func (s *Server) initialize(sess *Session, params json.RawMessage) (any, error) {
	var p InitializeParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.initialized {
		return nil, ecp.Errorf(ecp.InvalidRequest, "already initialized")
	}
	version := ""
	if slices.Contains(SupportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	} else {
		for _, v := range SupportedVersions { // newest first
			if slices.Contains(p.SupportedVersions, v) {
				version = v
				break
			}
		}
	}
	if version == "" {
		return nil, &ecp.Error{Code: ecp.UnsupportedVersion, Message: fmt.Sprintf("no shared protocol version; this server supports %v", SupportedVersions)}
	}
	if !sess.fixed {
		if p.ClientInfo.Name == "" {
			return nil, ecp.Errorf(ecp.InvalidParams, "clientInfo.name is required")
		}
		sess.client = p.ClientInfo.Name
	}
	sess.notify.relevant = p.Capabilities.Notifications.Relevant
	sess.notify.conflict = p.Capabilities.Notifications.Conflict
	sess.notify.consolidated = p.Capabilities.Notifications.Consolidated
	if id, err := s.st.LatestEventID(); err == nil {
		sess.lastEvent = id // only events from now on are pushed
	}
	sess.initialized = true
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    ServerCapabilities(),
		"serverInfo":      map[string]string{"name": "engram", "version": s.o.Version},
	}, nil
}

// ---------------------------------------------------------------- notifications

// Run delivers notifications until ctx ends: relevant events for each session's
// subscriptions (rate-limited per subscription), and conflict and consolidation
// events from the shared event log, whichever process caused them.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.o.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Poll()
		}
	}
}

// Poll runs one delivery pass (exported for tests and single-shot callers).
func (s *Server) Poll() {
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	var events []store.Event
	minEvent := int64(-1)
	for _, sess := range sessions {
		sess.mu.Lock()
		if sess.initialized && (minEvent < 0 || sess.lastEvent < minEvent) {
			minEvent = sess.lastEvent
		}
		sess.mu.Unlock()
	}
	if minEvent >= 0 {
		events, _ = s.st.EventsAfter(minEvent, 200)
	}
	for _, sess := range sessions {
		s.deliver(sess, events)
	}
	// Forget sessions idle for an hour with no stream attached (HTTP clients that went away).
	s.mu.Lock()
	var stale []*Session
	for _, sess := range s.sessions {
		sess.mu.Lock()
		if sess.send == nil && time.Since(sess.lastSeen) > time.Hour {
			stale = append(stale, sess)
		}
		sess.mu.Unlock()
	}
	s.mu.Unlock()
	for _, sess := range stale {
		s.Close(sess)
	}
}

func (s *Server) deliver(sess *Session, events []store.Event) {
	sess.mu.Lock()
	if !sess.initialized {
		sess.mu.Unlock()
		return
	}
	client := sess.client
	want := sess.notify
	subIDs := make([]string, 0, len(sess.subs))
	for id := range sess.subs {
		subIDs = append(subIDs, id)
	}
	last := sess.lastEvent
	sess.mu.Unlock()

	if len(subIDs) > 0 {
		_ = s.st.TouchSubscriptions(subIDs)
	}
	if want.relevant && len(subIDs) > 0 {
		notes, err := s.st.PendingFor(client, subIDs)
		if err == nil {
			var delivered []int64
			sent := map[string]bool{}
			for _, n := range notes {
				sess.mu.Lock()
				lastPush := sess.subs[n.SubscriptionID]
				ok := !sent[n.SubscriptionID] && time.Since(lastPush) >= s.o.RateWindow
				if ok {
					sess.subs[n.SubscriptionID] = time.Now()
				}
				sess.mu.Unlock()
				if !ok {
					continue // held until the window passes; never dropped
				}
				sent[n.SubscriptionID] = true
				mems := make([]store.Scored, 0, len(n.Memories))
				for _, m := range n.Memories {
					mems = append(mems, store.Scored{Memory: m, Score: n.Salience, Confidence: n.Salience, Reason: n.Reason})
				}
				sess.push("notifications/engram/relevant", map[string]any{
					"subscriptionId": n.SubscriptionID, "memories": mems, "reason": n.Reason})
				delivered = append(delivered, n.ID)
			}
			_ = s.st.MarkNotificationsRead(delivered)
		}
	}
	newest := last
	for _, e := range events {
		if e.ID <= last {
			continue
		}
		newest = e.ID
		switch e.Kind {
		case "conflict":
			if !want.conflict {
				continue
			}
			var p struct{ A, B string }
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			mems, err := s.st.Get(client, []string{p.A, p.B})
			if err != nil || len(mems) != 2 {
				continue // never push what the client cannot read
			}
			sess.push("notifications/engram/conflict", map[string]any{"a": mems[0], "b": mems[1],
				"summary": fmt.Sprintf("%q disagrees with %q", store.Short(mems[0].Content), store.Short(mems[1].Content))})
		case "consolidated":
			if !want.consolidated {
				continue
			}
			var p map[string]any
			if json.Unmarshal(e.Payload, &p) == nil {
				sess.push("notifications/engram/consolidated", p)
			}
		}
	}
	sess.mu.Lock()
	if newest > sess.lastEvent {
		sess.lastEvent = newest
	}
	sess.mu.Unlock()
}
