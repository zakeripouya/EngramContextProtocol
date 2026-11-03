// Package conformance checks an Engram Context Protocol server against the
// requirements of docs/spec/ecp-v0.md. Any implementation can run it, over HTTP
// (an /ecp endpoint and a bearer token) or stdio (a command that starts the server).
//
// The suite needs a client identity paired with read and write access to one test
// namespace (default "conformance/test") and no access to another (default
// "conformance/denied"). It writes only to the test namespace and forgets what it
// wrote when the server supports forget.
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/sdk/go/ecpclient"
)

// Level is how strongly the spec requires a behavior.
type Level string

// Requirement levels.
const (
	Must   Level = "MUST"
	Should Level = "SHOULD"
)

// Status of one check.
const (
	Pass = "pass"
	Fail = "FAIL"
	Skip = "skip"
)

// Result is one check's outcome.
type Result struct {
	ID     string `json:"id"`
	Level  Level  `json:"level"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Target says how to reach the server under test.
type Target struct {
	URL     string   // HTTP endpoint, such as http://127.0.0.1:7079/ecp
	Token   string   // bearer token for URL
	Command []string // or: a command that serves ECP on stdio
	// Connect, if set, opens an uninitialized connection instead (for in-process servers).
	Connect func(ctx context.Context) (*ecpclient.Client, error)

	Client    string // the identity the server should attribute writes to; default "conformance"
	Namespace string // writable test namespace; default "conformance/test"
	Denied    string // a namespace the client may not read or write; default "conformance/denied"
}

// Report is the outcome of a run.
type Report struct {
	Server  string   `json:"server"`
	Results []Result `json:"results"`
}

// Failed counts failed checks at a level.
func (r Report) Failed(l Level) int {
	n := 0
	for _, x := range r.Results {
		if x.Status == Fail && x.Level == l {
			n++
		}
	}
	return n
}

// Print writes a readable report.
func (r Report) Print(w io.Writer) {
	fmt.Fprintf(w, "ECP conformance: %s\n\n", r.Server)
	for _, x := range r.Results {
		fmt.Fprintf(w, "  %-4s  %-6s  %-4s %s", x.Status, x.Level, x.ID, x.Title)
		if x.Detail != "" {
			fmt.Fprintf(w, "  (%s)", x.Detail)
		}
		fmt.Fprintln(w)
	}
	pass, skip := 0, 0
	for _, x := range r.Results {
		switch x.Status {
		case Pass:
			pass++
		case Skip:
			skip++
		}
	}
	fmt.Fprintf(w, "\n%d passed, %d MUST and %d SHOULD failed, %d skipped\n", pass, r.Failed(Must), r.Failed(Should), skip)
}

type runner struct {
	t       Target
	rep     Report
	nonce   string
	created []string
}

func (r *runner) add(id string, l Level, title string, err error) {
	res := Result{ID: id, Level: l, Title: title, Status: Pass}
	if err != nil {
		res.Status, res.Detail = Fail, err.Error()
	}
	r.rep.Results = append(r.rep.Results, res)
}

func (r *runner) skip(id string, l Level, title, why string) {
	r.rep.Results = append(r.rep.Results, Result{ID: id, Level: l, Title: title, Status: Skip, Detail: why})
}

func (r *runner) connect(ctx context.Context) (*ecpclient.Client, error) {
	if r.t.Connect != nil {
		return r.t.Connect(ctx)
	}
	if r.t.URL != "" {
		return ecpclient.NewHTTP(r.t.URL, r.t.Token), nil
	}
	if len(r.t.Command) == 0 {
		return nil, fmt.Errorf("target needs URL or Command")
	}
	return ecpclient.StartCommand(r.t.Command[0], r.t.Command[1:])
}

func (r *runner) info() ecpclient.Info {
	return ecpclient.Info{Name: r.t.Client, Version: "conformance"}
}

func wantCode(err error, code int) error {
	if err == nil {
		return fmt.Errorf("succeeded; want error %d", code)
	}
	if !ecpclient.IsCode(err, code) {
		return fmt.Errorf("got %v; want error %d", err, code)
	}
	return nil
}

// Run executes the suite.
func Run(ctx context.Context, t Target) (Report, error) {
	if t.Client == "" {
		t.Client = "conformance"
	}
	if t.Namespace == "" {
		t.Namespace = "conformance/test"
	}
	if t.Denied == "" {
		t.Denied = "conformance/denied"
	}
	r := &runner{t: t, nonce: fmt.Sprintf("c%x", time.Now().UnixNano()%0xffffffff)}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Lifecycle on a raw connection: an unsupported version, then a request before
	// a successful initialize.
	raw, err := r.connect(ctx)
	if err != nil {
		return r.rep, err
	}
	_, err = raw.CallRaw(ctx, "initialize", map[string]any{"protocolVersion": "1999-01-01", "supportedVersions": []string{"1999-01-01"},
		"capabilities": map[string]any{}, "clientInfo": map[string]string{"name": t.Client, "version": "0"}})
	r.add("L1", Must, "initialize with no shared version fails with -32002", wantCode(err, ecpclient.CodeUnsupportedVersion))
	_, err = raw.CallRaw(ctx, "engram/recall", map[string]any{"query": "anything"})
	r.add("L2", Must, "requests before a successful initialize fail with -32001", wantCode(err, ecpclient.CodeNotInitialized))
	_ = raw.Close()

	c, err := r.connect(ctx)
	if err != nil {
		return r.rep, err
	}
	defer c.Close()
	err = c.Initialize(ctx, r.info())
	if err == nil && c.Init.ProtocolVersion != ecpclient.ProtocolVersion {
		err = fmt.Errorf("negotiated %q", c.Init.ProtocolVersion)
	}
	if err == nil && c.Init.ServerInfo.Name == "" {
		err = fmt.Errorf("serverInfo.name is empty")
	}
	r.add("L3", Must, "initialize negotiates the shared version and returns serverInfo", err)
	if err != nil {
		return r.rep, nil // nothing else can run
	}
	r.rep.Server = c.Init.ServerInfo.Name + " " + c.Init.ServerInfo.Version
	var capErr error
	if !c.Init.Has("observe") || !c.Init.Has("recall") {
		capErr = fmt.Errorf("capabilities: %v", c.Init.Capabilities)
	}
	r.add("L4", Must, "the server declares observe and recall", capErr)

	r.jsonrpc(ctx, c)
	r.memory(ctx, c)
	r.scopes(ctx, c)
	r.optional(ctx, c)

	if c.Init.Has("forget") && len(r.created) > 0 {
		_, _ = c.Forget(ctx, true, r.created...)
	}
	return r.rep, nil
}

func (r *runner) jsonrpc(ctx context.Context, c *ecpclient.Client) {
	_, err := c.CallRaw(ctx, "engram/no_such_method", nil)
	r.add("J1", Must, "unknown methods fail with -32601", wantCode(err, ecpclient.CodeMethodNotFound))
	_, err = c.CallRaw(ctx, "engram/recall", map[string]any{"query": 42})
	r.add("J2", Must, "invalid params fail with -32602", wantCode(err, ecpclient.CodeInvalidParams))

	if r.t.URL == "" {
		// On stdio a malformed message has no id a client could match a reply to.
		r.skip("J3", Must, "malformed JSON fails with -32700", "checked over HTTP only")
	} else {
		out, err := c.Send(ctx, []byte(`{"jsonrpc":"2.0","id":"p1","method":`), true)
		if err == nil {
			var resp struct{ Error *ecpclient.Error }
			if json.Unmarshal(out, &resp) != nil || resp.Error == nil || resp.Error.Code != ecpclient.CodeParseError {
				err = fmt.Errorf("got %s; want error -32700", strings.TrimSpace(string(out)))
			}
		}
		r.add("J3", Must, "malformed JSON fails with -32700", err)
	}

	err = c.Notify(ctx, "notifications/initialized", nil)
	if err == nil {
		_, err = c.CallRaw(ctx, "ping", nil)
	}
	r.add("J4", Must, "notifications get no response, and the connection stays usable", err)

	batch := `[{"jsonrpc":"2.0","id":"b1","method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":"b2","method":"engram/no_such_method"}]`
	if r.t.URL == "" {
		r.skip("J5", Should, "batches get one response per request", "checked over HTTP only")
	} else {
		out, err := c.Send(ctx, []byte(batch), true)
		if err == nil {
			var resps []json.RawMessage
			if json.Unmarshal(out, &resps) != nil || len(resps) != 2 {
				err = fmt.Errorf("got %s", strings.TrimSpace(string(out)))
			}
		}
		r.add("J5", Should, "batches get one response per request", err)
	}
}

func (r *runner) observe(ctx context.Context, c *ecpclient.Client, p ecpclient.ObserveParams) (string, error) {
	if p.Namespace == "" {
		p.Namespace = r.t.Namespace
	}
	res, err := c.Observe(ctx, p)
	if err == nil {
		if res.ID == "" {
			return "", fmt.Errorf("observe returned no id")
		}
		r.created = append(r.created, res.ID)
	}
	return res.ID, err
}

func (r *runner) memory(ctx context.Context, c *ecpclient.Client) {
	content := "Conformance " + r.nonce + ": release branches are cut on Mondays"
	id, err := r.observe(ctx, c, ecpclient.ObserveParams{Kind: "fact", Content: content, Tags: []string{"conformance"}})
	var got []ecpclient.Memory
	if err == nil {
		got, err = c.Get(ctx, id)
	}
	if err == nil && (len(got) != 1 || got[0].Content != content || got[0].Kind != "fact" || got[0].Namespace != r.t.Namespace) {
		err = fmt.Errorf("get returned %+v", got)
	}
	r.add("M1", Must, "observe stores a memory that get returns unchanged", err)

	if err == nil {
		if got[0].Provenance.Client != r.t.Client {
			err = fmt.Errorf("provenance.client is %q, want %q", got[0].Provenance.Client, r.t.Client)
		} else if got[0].Provenance.At == "" || got[0].CreatedAt == "" {
			err = fmt.Errorf("provenance.at or createdAt missing")
		}
	}
	r.add("M2", Must, "the server sets provenance from the identified client", err)

	_, err = c.Observe(ctx, ecpclient.ObserveParams{Kind: "decision", Namespace: r.t.Namespace, Author: "agent",
		Content: "Conformance " + r.nonce + " agent decision", Decision: &ecpclient.Decision{Situation: "s", Choice: "c"}})
	r.add("M3", Must, "a decision without author \"user\" fails with -32006", wantCode(err, ecpclient.CodeInvalidProvenance))

	_, err = r.observe(ctx, c, ecpclient.ObserveParams{Kind: "decision", Author: "user",
		Content: "Conformance " + r.nonce + ": the user chose squash merges", Decision: &ecpclient.Decision{Situation: "merge style", Choice: "squash"}})
	r.add("M4", Must, "a decision with author \"user\" is accepted", err)

	hits, err := c.Recall(ctx, ecpclient.RecallParams{Query: "Conformance " + r.nonce + " release branches", Namespaces: []string{r.t.Namespace}})
	if err == nil {
		found := false
		for _, h := range hits {
			found = found || h.Memory.ID == id
		}
		if !found {
			err = fmt.Errorf("observed memory not recalled (%d results)", len(hits))
		}
	}
	r.add("M5", Must, "recall returns an observed memory with a score", err)

	oldID, err := r.observe(ctx, c, ecpclient.ObserveParams{Kind: "fact", Content: "Conformance " + r.nonce + " pricing is tiered", ValidFrom: "2020-01-01T00:00:00Z"})
	var newID string
	if err == nil {
		newID, err = r.observe(ctx, c, ecpclient.ObserveParams{Kind: "fact", Content: "Conformance " + r.nonce + " pricing is flat",
			ValidFrom: "2021-01-01T00:00:00Z", Supersedes: oldID})
	}
	if err == nil {
		hits, err = c.Recall(ctx, ecpclient.RecallParams{Query: "Conformance " + r.nonce + " pricing", Namespaces: []string{r.t.Namespace}})
		for _, h := range hits {
			if err == nil && h.Memory.ID == oldID {
				err = fmt.Errorf("a superseded memory was recalled as current")
			}
		}
	}
	r.add("M6", Must, "a superseded memory is no longer current", err)

	if err == nil {
		hits, err = c.Recall(ctx, ecpclient.RecallParams{Query: "Conformance " + r.nonce + " pricing", Namespaces: []string{r.t.Namespace}, AsOf: "2020-06-01"})
		if err == nil {
			found := false
			for _, h := range hits {
				found = found || h.Memory.ID == oldID
				if h.Memory.ID == newID {
					err = fmt.Errorf("asOf returned a fact that was not yet true")
				}
			}
			if !found && err == nil {
				err = fmt.Errorf("asOf did not return the fact true at that time")
			}
		}
	}
	r.add("M7", Should, "asOf answers with what was true at that time", err)

	_, err = c.Observe(ctx, ecpclient.ObserveParams{Kind: "fact", Namespace: r.t.Namespace,
		Content: "deploy token ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"})
	r.add("M8", Should, "content that looks like a secret fails with -32008", wantCode(err, ecpclient.CodeContentRejected))

	if !c.Init.Has("forget") {
		r.skip("M9", Must, "forget removes a memory; get then fails with -32005", "forget not declared")
		return
	}
	fid, err := c.Observe(ctx, ecpclient.ObserveParams{Kind: "fact", Namespace: r.t.Namespace, Content: "Conformance " + r.nonce + " to forget"})
	if err == nil {
		_, err = c.Forget(ctx, true, fid.ID)
	}
	if err == nil {
		_, err = c.Get(ctx, fid.ID)
		err = wantCode(err, ecpclient.CodeMemoryNotFound)
	}
	r.add("M9", Must, "forget removes a memory; get then fails with -32005", err)
}

func (r *runner) scopes(ctx context.Context, c *ecpclient.Client) {
	_, err := c.Observe(ctx, ecpclient.ObserveParams{Kind: "fact", Namespace: r.t.Denied, Content: "Conformance " + r.nonce + " out of scope"})
	r.add("S1", Must, "writing outside the client's grants fails with -32004", wantCode(err, ecpclient.CodeScopeDenied))
	_, err = c.Recall(ctx, ecpclient.RecallParams{Query: "anything at all", Namespaces: []string{r.t.Denied}})
	r.add("S2", Must, "reading outside the client's grants fails with -32004", wantCode(err, ecpclient.CodeScopeDenied))
}

func (r *runner) optional(ctx context.Context, c *ecpclient.Client) {
	a, errA := r.observe(ctx, c, ecpclient.ObserveParams{Kind: "fact", Content: "Conformance " + r.nonce + " alpha"})
	b, errB := r.observe(ctx, c, ecpclient.ObserveParams{Kind: "fact", Content: "Conformance " + r.nonce + " beta", Contradicts: []string{a}})
	setup := errA
	if setup == nil {
		setup = errB
	}

	if c.Init.Has("attend") {
		res, err := c.Attend(ctx, ecpclient.AttendParams{Context: "Conformance " + r.nonce + " alpha beta", Namespaces: []string{r.t.Namespace}})
		if err == nil && (res.Memories == nil || res.Conflicts == nil) {
			err = fmt.Errorf("attend must return memories and conflicts arrays")
		}
		r.add("O1", Must, "attend returns memories and conflicts (attend declared)", err)
	} else {
		r.skip("O1", Must, "attend returns memories and conflicts", "attend not declared")
	}
	if c.Init.Has("recall") && setup == nil {
		res, err := c.Path(ctx, a, b, "maximin")
		if ecpclient.IsCode(err, ecpclient.CodeMethodNotFound) {
			r.skip("O2", Should, "path returns a path array", "engram/path not implemented")
		} else {
			if err == nil && res.Path == nil {
				err = fmt.Errorf("path must be an array")
			}
			r.add("O2", Should, "path returns a path array", err)
		}
	}
	switch {
	case !c.Init.Has("feedback"):
		r.skip("O3", Must, "feedback accepts an outcome", "feedback not declared")
	case setup != nil:
		r.add("O3", Must, "feedback accepts an outcome (feedback declared)", fmt.Errorf("setup failed: %v", setup))
	default:
		_, err := c.Feedback(ctx, ecpclient.FeedbackParams{MemoryIDs: []string{a}, Outcome: "accepted", Objective: "succeeded"})
		r.add("O3", Must, "feedback accepts an outcome (feedback declared)", err)
		_, err = c.Feedback(ctx, ecpclient.FeedbackParams{MemoryIDs: []string{a}, Outcome: "maybe"})
		r.add("O4", Should, "feedback with an unknown outcome fails with -32602", wantCode(err, ecpclient.CodeInvalidParams))
	}
	if c.Init.Has("sessions") {
		cp, err := c.Checkpoint(ctx, ecpclient.Checkpoint{Goal: "Conformance " + r.nonce + " task", Next: "step two", Namespace: r.t.Namespace})
		var res ecpclient.ResumeResult
		if err == nil {
			res, err = c.Resume(ctx, cp.SessionID, nil)
		}
		if err == nil && (res.Checkpoint.Next != "step two" || res.Missing == nil) {
			err = fmt.Errorf("resume returned %+v", res)
		}
		r.add("O5", Must, "a checkpoint can be resumed with its next step (sessions declared)", err)
	} else {
		r.skip("O5", Must, "a checkpoint can be resumed", "sessions not declared")
	}
	if c.Init.Has("subscribe") {
		id, err := c.Subscribe(ctx, ecpclient.SubscribeParams{Context: "Conformance " + r.nonce, Namespaces: []string{r.t.Namespace}})
		if err == nil && id == "" {
			err = fmt.Errorf("no subscriptionId")
		}
		if err == nil {
			err = c.Unsubscribe(ctx, id)
		}
		r.add("O6", Must, "subscribe returns an id that unsubscribe accepts (subscribe declared)", err)
		_, err = c.Subscribe(ctx, ecpclient.SubscribeParams{Context: "x", Namespaces: []string{r.t.Denied}})
		r.add("O7", Must, "subscribing outside the client's grants fails with -32004", wantCode(err, ecpclient.CodeScopeDenied))
	} else {
		r.skip("O6", Must, "subscribe and unsubscribe", "subscribe not declared")
	}
}
