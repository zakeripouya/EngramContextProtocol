package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

// Sessions are working memory for a task in progress (milestone M7; spec,
// "Sessions and handoff"). Long-term memory follows the user everywhere; a
// checkpoint lets any paired agent continue a task another agent started,
// and reports what the resuming agent is missing instead of assuming it.

// Environment is what a task needs, or what a resuming agent has.
type Environment struct {
	Repo  string   `json:"repo,omitempty"`
	Tools []string `json:"tools,omitempty"`
}

// Checkpoint is the live state of a task.
type Checkpoint struct {
	SessionID     string       `json:"sessionId"`
	Goal          string       `json:"goal"`
	Plan          []string     `json:"plan"`
	Done          []string     `json:"done"`
	Next          string       `json:"next,omitempty"`
	OpenQuestions []string     `json:"openQuestions,omitempty"`
	Files         []string     `json:"files,omitempty"`
	Environment   *Environment `json:"environment,omitempty"`
	Namespace     string       `json:"namespace"`
	Status        string       `json:"status"` // open or done
	Result        string       `json:"result,omitempty"`
	ParentID      string       `json:"parentId,omitempty"`
	DelegatedTo   string       `json:"delegatedTo,omitempty"`
	CreatedBy     string       `json:"createdBy,omitempty"`
	LastClient    string       `json:"lastClient,omitempty"`
	CreatedAt     string       `json:"createdAt,omitempty"`
	UpdatedAt     string       `json:"updatedAt"`
}

// ResumeResult is a checkpoint plus what the resuming agent lacks.
type ResumeResult struct {
	Checkpoint Checkpoint `json:"checkpoint"`
	Missing    []string   `json:"missing"`
	Subtasks   []Subtask  `json:"subtasks,omitempty"`
	Memories   []Memory   `json:"memories,omitempty"` // recent memories recorded under this session
}

// Subtask summarizes a delegated child session.
type Subtask struct {
	SessionID   string `json:"sessionId"`
	Goal        string `json:"goal"`
	Status      string `json:"status"`
	DelegatedTo string `json:"delegatedTo,omitempty"`
	Result      string `json:"result,omitempty"`
}

const maxSessionText = 20000

func (s *Store) loadSession(id string) (Checkpoint, error) {
	var state string
	err := s.db.QueryRow(`SELECT state FROM sessions WHERE id=?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, ecp.Errorf(ecp.MemoryNotFound, "no session %q", id)
	}
	if err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if err := json.Unmarshal([]byte(state), &cp); err != nil {
		return Checkpoint{}, err
	}
	return cp, nil
}

func (s *Store) saveSession(tx *sql.Tx, cp Checkpoint) error {
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO sessions(id, namespace, status, parent_id, delegated_to, goal, state, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET namespace=excluded.namespace, status=excluded.status, parent_id=excluded.parent_id,
		  delegated_to=excluded.delegated_to, goal=excluded.goal, state=excluded.state, updated_at=excluded.updated_at`,
		cp.SessionID, cp.Namespace, cp.Status, nullable(cp.ParentID), nullable(cp.DelegatedTo), cp.Goal, string(b),
		cp.CreatedAt, cp.UpdatedAt)
	return err
}

func checkpointText(cp Checkpoint) string {
	parts := []string{cp.Goal, cp.Next, cp.Result}
	parts = append(parts, cp.Plan...)
	parts = append(parts, cp.Done...)
	parts = append(parts, cp.OpenQuestions...)
	parts = append(parts, cp.Files...)
	return strings.Join(parts, "\n")
}

// SaveCheckpoint creates or updates a session. An empty SessionID starts a new
// one. Marking a session done records an episode in long-term memory and, for a
// delegated subtask, reports the result back to the parent task.
func (s *Store) SaveCheckpoint(client string, in Checkpoint) (Checkpoint, error) {
	in.Goal = strings.TrimSpace(in.Goal)
	if in.Status == "" {
		in.Status = "open"
	}
	if in.Status != "open" && in.Status != "done" {
		return Checkpoint{}, ecp.Errorf(ecp.InvalidParams, "status must be open or done")
	}
	var prev *Checkpoint
	if in.SessionID != "" {
		p, err := s.loadSession(in.SessionID)
		if err != nil {
			return Checkpoint{}, err
		}
		prev = &p
		if ok, err := s.can(client, p.Namespace, true); err != nil {
			return Checkpoint{}, err
		} else if !ok {
			return Checkpoint{}, ecp.Errorf(ecp.ScopeDenied, "client %q cannot write to namespace %q", client, p.Namespace)
		}
		// Fields the caller left out keep their previous values.
		if in.Goal == "" {
			in.Goal = p.Goal
		}
		if in.Namespace == "" {
			in.Namespace = p.Namespace
		}
		if in.Plan == nil {
			in.Plan = p.Plan
		}
		if in.Done == nil {
			in.Done = p.Done
		}
		if in.Next == "" && in.Status == "open" {
			in.Next = p.Next
		}
		if in.OpenQuestions == nil {
			in.OpenQuestions = p.OpenQuestions
		}
		if in.Files == nil {
			in.Files = p.Files
		}
		if in.Environment == nil {
			in.Environment = p.Environment
		}
		in.ParentID, in.DelegatedTo, in.CreatedBy, in.CreatedAt = p.ParentID, p.DelegatedTo, p.CreatedBy, p.CreatedAt
		if in.Namespace != p.Namespace {
			return Checkpoint{}, ecp.Errorf(ecp.InvalidParams, "a session cannot move between namespaces")
		}
	} else {
		in.SessionID = newID()
		in.CreatedBy = client
		in.CreatedAt = ts(Now())
		in.ParentID, in.DelegatedTo = "", ""
	}
	if in.Goal == "" {
		return Checkpoint{}, ecp.Errorf(ecp.InvalidParams, "goal is required")
	}
	if in.Namespace == "" {
		in.Namespace = "me"
	}
	if !validNamespace(in.Namespace) || in.Namespace == "*" {
		return Checkpoint{}, ecp.Errorf(ecp.InvalidParams, "invalid namespace %q", in.Namespace)
	}
	if ok, err := s.can(client, in.Namespace, true); err != nil {
		return Checkpoint{}, err
	} else if !ok {
		return Checkpoint{}, ecp.Errorf(ecp.ScopeDenied, "client %q cannot write to namespace %q", client, in.Namespace)
	}
	text := checkpointText(in)
	if len(text) > maxSessionText {
		return Checkpoint{}, ecp.Errorf(ecp.InvalidParams, "checkpoint is too large (%d characters, limit %d)", len(text), maxSessionText)
	}
	if hit := FindSensitive(text); hit != "" {
		_ = audit(s.db, client, "reject", in.Namespace, hit)
		return Checkpoint{}, ecp.Errorf(ecp.ContentRejected, "checkpoint looks like it contains a %s; nothing was stored", hit)
	}
	if in.Plan == nil {
		in.Plan = []string{}
	}
	if in.Done == nil {
		in.Done = []string{}
	}
	in.LastClient = client
	in.UpdatedAt = ts(Now())

	finishing := in.Status == "done" && (prev == nil || prev.Status != "done")
	tx, err := s.db.Begin()
	if err != nil {
		return Checkpoint{}, err
	}
	defer tx.Rollback()
	if err := s.saveSession(tx, in); err != nil {
		return Checkpoint{}, err
	}
	if finishing && in.ParentID != "" {
		var state string
		if err := tx.QueryRow(`SELECT state FROM sessions WHERE id=?`, in.ParentID).Scan(&state); err == nil {
			var parent Checkpoint
			if json.Unmarshal([]byte(state), &parent) == nil {
				line := fmt.Sprintf("Subtask %q finished by %s", in.Goal, client)
				if in.Result != "" {
					line += ": " + in.Result
				}
				parent.Done = append(parent.Done, line)
				parent.UpdatedAt = in.UpdatedAt
				if err := s.saveSession(tx, parent); err != nil {
					return Checkpoint{}, err
				}
			}
		}
	}
	if err := audit(tx, client, "checkpoint", in.Namespace, in.SessionID); err != nil {
		return Checkpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return Checkpoint{}, err
	}
	if finishing {
		// The finished task becomes an episode in long-term memory.
		content := "Finished task: " + in.Goal
		if in.Result != "" {
			content += ". Result: " + in.Result
		}
		if _, err := s.Observe(client, ObserveInput{Kind: "episode", Content: Short(content), Namespace: in.Namespace,
			Tags: []string{"session"}, SessionID: in.SessionID}); err != nil {
			return in, err
		}
	}
	return in, nil
}

func (s *Store) readableSession(client, id string) (Checkpoint, error) {
	cp, err := s.loadSession(id)
	if err != nil {
		return Checkpoint{}, err
	}
	if ok, err := s.can(client, cp.Namespace, false); err != nil {
		return Checkpoint{}, err
	} else if !ok {
		return Checkpoint{}, ecp.Errorf(ecp.ScopeDenied, "client %q cannot read namespace %q", client, cp.Namespace)
	}
	return cp, nil
}

// Resume loads a checkpoint for a client and reports what it is missing: the
// repository, tools and write access the task needs that the client does not
// declare or hold. The client then asks the user instead of assuming.
func (s *Store) Resume(client, id string, have *Environment) (ResumeResult, error) {
	cp, err := s.readableSession(client, id)
	if err != nil {
		return ResumeResult{}, err
	}
	res := ResumeResult{Checkpoint: cp, Missing: []string{}}
	if need := cp.Environment; need != nil {
		var hv Environment
		if have != nil {
			hv = *have
		}
		if need.Repo != "" && !sameRepo(need.Repo, hv.Repo) {
			if hv.Repo == "" {
				res.Missing = append(res.Missing, "repo: "+need.Repo+" (no repository declared by the resuming agent)")
			} else {
				res.Missing = append(res.Missing, "repo: "+need.Repo+" (the resuming agent is in "+hv.Repo+")")
			}
		}
		got := map[string]bool{}
		for _, t := range hv.Tools {
			got[strings.ToLower(strings.TrimSpace(t))] = true
		}
		for _, t := range need.Tools {
			if !got[strings.ToLower(strings.TrimSpace(t))] {
				res.Missing = append(res.Missing, "tool: "+t)
			}
		}
	}
	if ok, err := s.can(client, cp.Namespace, true); err != nil {
		return ResumeResult{}, err
	} else if !ok {
		res.Missing = append(res.Missing, "permission: write access to "+cp.Namespace+" (needed to save progress)")
	}

	rows, err := s.db.Query(`SELECT state FROM sessions WHERE parent_id=? ORDER BY created_at`, id)
	if err != nil {
		return ResumeResult{}, err
	}
	for rows.Next() {
		var state string
		var c Checkpoint
		if rows.Scan(&state) == nil && json.Unmarshal([]byte(state), &c) == nil {
			res.Subtasks = append(res.Subtasks, Subtask{SessionID: c.SessionID, Goal: c.Goal, Status: c.Status,
				DelegatedTo: c.DelegatedTo, Result: c.Result})
		}
	}
	rows.Close()

	var ids []string
	idRows, err := s.db.Query(`SELECT id FROM memories WHERE session_id=? AND namespace=? ORDER BY created_at DESC LIMIT 10`,
		id, cp.Namespace)
	if err != nil {
		return ResumeResult{}, err
	}
	for idRows.Next() {
		var mid string
		if idRows.Scan(&mid) == nil {
			ids = append(ids, mid)
		}
	}
	idRows.Close()
	if len(ids) > 0 {
		if mems, err := s.Get(client, ids); err == nil {
			res.Memories = mems
		}
	}

	if _, err := s.db.Exec(`UPDATE sessions SET state=json_set(state, '$.lastClient', ?) WHERE id=?`, client, id); err != nil {
		return ResumeResult{}, err
	}
	_ = audit(s.db, client, "resume", cp.Namespace, id)
	return res, nil
}

func sameRepo(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
		for _, p := range []string{"https://", "http://", "ssh://", "git@"} {
			s = strings.TrimPrefix(s, p)
		}
		s = strings.Replace(s, ":", "/", 1)
		return s
	}
	na, nb := norm(a), norm(b)
	if na == "" || nb == "" {
		return false
	}
	// A local path and a remote URL match on their last two segments (owner/repo) or the last one.
	tail := func(s string, n int) string {
		parts := strings.Split(s, "/")
		if len(parts) > n {
			parts = parts[len(parts)-n:]
		}
		return strings.Join(parts, "/")
	}
	return na == nb || tail(na, 2) == tail(nb, 2) || tail(na, 1) == tail(nb, 1)
}

// ListSessions returns sessions in namespaces the client may read, newest first.
// status is "open", "done" or "" for both. Subtasks delegated to the client come first.
func (s *Store) ListSessions(client, status string) ([]Checkpoint, error) {
	nss, all, err := s.readable(client)
	if err != nil {
		return nil, err
	}
	q := `SELECT state FROM sessions`
	var conds []string
	var args []any
	if status != "" {
		conds = append(conds, "status=?")
		args = append(args, status)
	}
	if !all {
		if len(nss) == 0 {
			return []Checkpoint{}, nil
		}
		conds = append(conds, "namespace IN ("+strings.TrimSuffix(strings.Repeat("?,", len(nss)), ",")+")")
		for _, ns := range nss {
			args = append(args, ns)
		}
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY updated_at DESC LIMIT 200"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Checkpoint{}
	for rows.Next() {
		var state string
		var c Checkpoint
		if err := rows.Scan(&state); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(state), &c) == nil {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].DelegatedTo == client && out[j].DelegatedTo != client
	})
	return out, rows.Err()
}

// Delegate hands a subtask to another agent as a child session carrying the
// parent's environment. When the child is marked done, its result is appended
// to the parent's completed steps.
func (s *Store) Delegate(client, parentID, subtask, toAgent string) (Checkpoint, error) {
	parent, err := s.readableSession(client, parentID)
	if err != nil {
		return Checkpoint{}, err
	}
	subtask = strings.TrimSpace(subtask)
	if subtask == "" {
		return Checkpoint{}, ecp.Errorf(ecp.InvalidParams, "subtask is required")
	}
	if ok, err := s.can(client, parent.Namespace, true); err != nil {
		return Checkpoint{}, err
	} else if !ok {
		return Checkpoint{}, ecp.Errorf(ecp.ScopeDenied, "client %q cannot write to namespace %q", client, parent.Namespace)
	}
	if hit := FindSensitive(subtask); hit != "" {
		return Checkpoint{}, ecp.Errorf(ecp.ContentRejected, "subtask looks like it contains a %s; nothing was stored", hit)
	}
	now := ts(Now())
	child := Checkpoint{
		SessionID: newID(), Goal: subtask, Plan: []string{}, Done: []string{},
		Next:        "Start the subtask; it was delegated from: " + parent.Goal,
		Environment: parent.Environment, Namespace: parent.Namespace, Status: "open",
		ParentID: parent.SessionID, DelegatedTo: strings.TrimSpace(toAgent),
		CreatedBy: client, LastClient: client, CreatedAt: now, UpdatedAt: now,
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Checkpoint{}, err
	}
	defer tx.Rollback()
	if err := s.saveSession(tx, child); err != nil {
		return Checkpoint{}, err
	}
	who := child.DelegatedTo
	if who == "" {
		who = "any agent"
	}
	parent.Done = append(parent.Done, fmt.Sprintf("Delegated %q to %s (session %s)", subtask, who, child.SessionID))
	parent.UpdatedAt = now
	if err := s.saveSession(tx, parent); err != nil {
		return Checkpoint{}, err
	}
	if err := audit(tx, client, "delegate", parent.Namespace, child.SessionID); err != nil {
		return Checkpoint{}, err
	}
	return child, tx.Commit()
}

// Session returns one session for the CLI, without grant checks.
func (s *Store) Session(id string) (Checkpoint, error) { return s.loadSession(id) }

// AllSessions lists every session for the CLI and dashboard.
func (s *Store) AllSessions(status string) ([]Checkpoint, error) {
	q, args := `SELECT state FROM sessions`, []any{}
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	rows, err := s.db.Query(q+` ORDER BY updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Checkpoint{}
	for rows.Next() {
		var state string
		var c Checkpoint
		if rows.Scan(&state) == nil && json.Unmarshal([]byte(state), &c) == nil {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}
