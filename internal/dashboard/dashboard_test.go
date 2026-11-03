package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func setup(t *testing.T) (*store.Store, http.Handler, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok := NewToken()
	return st, Handler(st, tok), tok
}

func do(t *testing.T, h http.Handler, method, path, host, tok string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req := httptest.NewRequest(method, path, &b)
	req.Host = host
	if tok != "" {
		req.Header.Set("X-Engram-Token", tok)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAPIGuard(t *testing.T) {
	_, h, tok := setup(t)
	if w := do(t, h, "GET", "/api/overview", "127.0.0.1:7078", "", nil); w.Code != 401 {
		t.Fatalf("no token: want 401, got %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/overview", "127.0.0.1:7078", "wrong", nil); w.Code != 401 {
		t.Fatalf("wrong token: want 401, got %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/overview", "evil.example:7078", tok, nil); w.Code != 403 {
		t.Fatalf("DNS rebinding host: want 403, got %d", w.Code)
	}
	w := do(t, h, "GET", "/api/overview", "localhost:7078", tok, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"health"`) {
		t.Fatalf("want overview, got %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/", "127.0.0.1:7078", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "app.js") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("the UI should be served with a CSP, got %d", w.Code)
	}
}

func TestInboxReviewAndConflict(t *testing.T) {
	st, h, tok := setup(t)
	if err := st.Pair("cli", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	src, _, err := st.InsertSystem(store.SystemMemory{Key: "s1", Kind: "episode", Namespace: "me", Service: "git", Content: "Commit: use pnpm in CI"})
	if err != nil {
		t.Fatal(err)
	}
	fact, _, err := st.InsertSystem(store.SystemMemory{Key: "d1", Kind: "fact", Namespace: "me", Service: "sleep", Content: "The project uses pnpm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddEdge(fact, src, "derived", 0.5); err != nil {
		t.Fatal(err)
	}
	a, _ := st.Observe("cli", store.ObserveInput{Kind: "fact", Content: "Deploys happen on Tuesdays"})
	b, _ := st.Observe("cli", store.ObserveInput{Kind: "fact", Content: "Deploys happen on Fridays"})
	if err := st.MarkConflict("cli", a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	var inbox struct {
		Items []store.InboxItem `json:"items"`
	}
	w := do(t, h, "GET", "/api/inbox", "127.0.0.1", tok, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &inbox)
	if len(inbox.Items) != 2 || inbox.Items[0].Kind != "distilled" || len(inbox.Items[0].Sources) != 1 || inbox.Items[1].Kind != "conflict" {
		t.Fatalf("want a distilled fact with its source and a conflict, got %+v", inbox.Items)
	}
	if w := do(t, h, "POST", "/api/inbox/review", "127.0.0.1", tok, map[string]any{"id": fact, "approve": true}); w.Code != 200 {
		t.Fatalf("review: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/inbox/review", "127.0.0.1", tok, map[string]any{"id": a.ID, "approve": false}); w.Code != 400 {
		t.Fatalf("only distilled facts can be reviewed, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/inbox/resolve", "127.0.0.1", tok, map[string]any{"keep": b.ID, "drop": a.ID}); w.Code != 200 {
		t.Fatalf("resolve: %d %s", w.Code, w.Body)
	}
	if items, _ := st.Inbox(); len(items) != 0 {
		t.Fatalf("the inbox should be empty, got %+v", items)
	}
	d, err := st.Detail(a.ID)
	if err != nil || d.Memory.ValidTo == "" {
		t.Fatalf("the dropped side should be archived: %+v %v", d.Memory, err)
	}
	if hits, _ := st.Recall("cli", store.RecallInput{Query: "when do deploys happen"}); len(hits) == 0 || hits[0].Memory.ID != b.ID {
		t.Fatalf("recall should now return the kept side first: %+v", hits)
	}
}

func TestReportEscapesContent(t *testing.T) {
	st, _, _ := setup(t)
	if _, _, err := st.InsertSystem(store.SystemMemory{Key: "x", Kind: "fact", Namespace: "me", Service: "sleep",
		Content: `<script>alert(1)</script> is not run`}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReport(st, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "<script>alert") || !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("memory content must be escaped in the report")
	}
	if !strings.Contains(out, "Engram report") || !strings.Contains(out, "Waiting for you (1)") {
		t.Fatal("report is missing sections")
	}
}

func TestReportShowsDistilledFacts(t *testing.T) {
	st, _, _ := setup(t)
	id, err := st.StartRun(false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(id, sleep.Report{RunID: id, Distilled: []sleep.Distilled{{Fact: "CI always runs the smoke suite", Verdict: "supported"}}}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReport(st, &b); err != nil {
		t.Fatalf("the report must render a night with distilled facts: %v", err)
	}
	if !strings.Contains(b.String(), "CI always runs the smoke suite") {
		t.Fatal("distilled fact missing from the report")
	}
}
