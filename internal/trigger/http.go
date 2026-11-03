package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

const maxPayload = 1 << 20

// Handler serves trigger endpoints:
//
//	POST /hooks/{name}   a webhook (generic JSON, or GitHub for kind github)
//	POST /jobs/{name}    a job result: {"status": "failed", "summary": "...", "output": "..."}
//	GET  /healthz
//
// Generic and job triggers authenticate with "Authorization: Bearer <secret>";
// GitHub triggers verify the X-Hub-Signature-256 HMAC with the same secret.
func Handler(st *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("POST /hooks/{name}", func(w http.ResponseWriter, r *http.Request) { handle(st, w, r, false) })
	mux.HandleFunc("POST /jobs/{name}", func(w http.ResponseWriter, r *http.Request) { handle(st, w, r, true) })
	return mux
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handle(st *store.Store, w http.ResponseWriter, r *http.Request, job bool) {
	name := r.PathValue("name")
	t, err := st.GetTrigger(name)
	if err != nil {
		reply(w, http.StatusNotFound, map[string]string{"error": "unknown trigger"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayload+1))
	if err != nil || len(body) > maxPayload {
		reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large or unreadable"})
		return
	}
	if !authorized(t, r, body) {
		reply(w, http.StatusUnauthorized, map[string]string{"error": "bad or missing credentials"})
		return
	}
	var ev Event
	switch {
	case job:
		ev, err = parseJob(body)
	case t.Kind == store.TriggerGitHub:
		ev, err = parseGitHub(r.Header.Get("X-GitHub-Event"), body)
		ev.Key = r.Header.Get("X-GitHub-Delivery")
	default:
		ev, err = parseGeneric(body)
	}
	if err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if ev.Title == "" {
		reply(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	ev.Trigger = t.Name
	res, err := Record(st, ev)
	var e *ecp.Error
	switch {
	case errors.Is(err, ErrPaused):
		reply(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		reply(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
	case errors.As(err, &e) && e.Code == ecp.ContentRejected:
		reply(w, http.StatusUnprocessableEntity, map[string]string{"error": e.Message})
	case err != nil:
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		reply(w, http.StatusOK, map[string]any{"memoryId": res.MemoryID, "created": res.Created, "notified": len(res.Notified)})
	}
}

func authorized(t store.Trigger, r *http.Request, body []byte) bool {
	if t.Kind == store.TriggerGitHub {
		sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
		got, err := hex.DecodeString(sig)
		if err != nil {
			return false
		}
		mac := hmac.New(sha256.New, []byte(t.Secret))
		mac.Write(body)
		return hmac.Equal(got, mac.Sum(nil))
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(t.Secret)) == 1
}

// Sign returns the GitHub-style signature header value for a body (for tests and clients).
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func parseJob(body []byte) (Event, error) {
	var p struct {
		Name       string   `json:"name"`
		Status     string   `json:"status"`
		Summary    string   `json:"summary"`
		Output     string   `json:"output"`
		DurationMS int64    `json:"duration_ms"`
		Tags       []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("job results are JSON: %v", err)
	}
	if p.Status != "ok" && p.Status != "failed" && p.Status != "succeeded" {
		return Event{}, fmt.Errorf(`status must be "ok" or "failed"`)
	}
	ev := Event{Status: p.Status, Body: p.Output, Tags: p.Tags}
	if p.Status == "succeeded" {
		ev.Status = "ok"
	}
	ev.Title = JobTitle(p.Name, ev.Status, p.Summary, p.DurationMS)
	return ev, nil
}

// JobTitle formats a job result as one line.
func JobTitle(name, status, summary string, durationMS int64) string {
	verb := "succeeded"
	if status == "failed" {
		verb = "failed"
	}
	t := "Job"
	if name != "" {
		t += " " + name
	}
	t += " " + verb
	if durationMS > 0 {
		t += fmt.Sprintf(" in %.1fs", float64(durationMS)/1000)
	}
	if summary = strings.TrimSpace(summary); summary != "" {
		t += ": " + summary
	}
	return t
}

func parseGeneric(body []byte) (Event, error) {
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{Title: "Webhook received", Body: string(body)}, nil
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := p[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	ev := Event{Title: str("title", "summary", "message", "text", "event"), Body: str("body", "details", "description", "output")}
	if ev.Title == "" {
		ev.Title = "Webhook received"
		ev.Body = string(body)
	}
	switch strings.ToLower(str("status", "state", "conclusion")) {
	case "failed", "failure", "error", "errored":
		ev.Status = "failed"
	}
	if tags, ok := p["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok {
				ev.Tags = append(ev.Tags, s)
			}
		}
	}
	return ev, nil
}

// parseGitHub summarizes the GitHub events that matter for memory; others are ignored.
func parseGitHub(kind string, body []byte) (Event, error) {
	var p struct {
		Action     string `json:"action"`
		Ref        string `json:"ref"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			Message string `json:"message"`
		} `json:"head_commit"`
		PullRequest *struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			Merged bool   `json:"merged"`
		} `json:"pull_request"`
		Issue *struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
		} `json:"issue"`
		WorkflowRun *struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			HeadBranch string `json:"head_branch"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_run"`
		Comment *struct {
			Body string `json:"body"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, fmt.Errorf("invalid GitHub payload: %v", err)
	}
	repo := p.Repository.FullName
	ev := Event{Tags: []string{"github"}}
	if repo != "" {
		ev.Tags = append(ev.Tags, repo)
	}
	switch kind {
	case "ping":
		return Event{}, nil
	case "workflow_run":
		if p.WorkflowRun == nil || p.Action != "completed" {
			return Event{}, nil
		}
		wr := p.WorkflowRun
		ev.Title = fmt.Sprintf("CI workflow %q %s on %s in %s", wr.Name, wr.Conclusion, wr.HeadBranch, repo)
		ev.Body = wr.HTMLURL
		if wr.Conclusion == "failure" || wr.Conclusion == "timed_out" {
			ev.Status = "failed"
		}
	case "pull_request":
		if p.PullRequest == nil {
			return Event{}, nil
		}
		action := p.Action
		if action == "closed" && p.PullRequest.Merged {
			action = "merged"
		}
		if action != "opened" && action != "merged" && action != "closed" && action != "reopened" {
			return Event{}, nil
		}
		ev.Title = fmt.Sprintf("Pull request #%d %s in %s: %s", p.PullRequest.Number, action, repo, p.PullRequest.Title)
		ev.Body = p.PullRequest.Body
	case "issues":
		if p.Issue == nil || (p.Action != "opened" && p.Action != "closed" && p.Action != "reopened") {
			return Event{}, nil
		}
		ev.Title = fmt.Sprintf("Issue #%d %s in %s: %s", p.Issue.Number, p.Action, repo, p.Issue.Title)
		ev.Body = p.Issue.Body
	case "issue_comment":
		if p.Issue == nil || p.Comment == nil || p.Action != "created" {
			return Event{}, nil
		}
		ev.Title = fmt.Sprintf("Comment on #%d in %s: %s", p.Issue.Number, repo, p.Issue.Title)
		ev.Body = p.Comment.Body
	case "push":
		if p.HeadCommit == nil {
			return Event{}, nil
		}
		ev.Title = fmt.Sprintf("Push to %s in %s: %s", strings.TrimPrefix(p.Ref, "refs/heads/"), repo,
			strings.SplitN(p.HeadCommit.Message, "\n", 2)[0])
	default:
		return Event{}, nil
	}
	return ev, nil
}
