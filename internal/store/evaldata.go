package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/model"
)

// Measuring and learning recall (0.13.0): every recall is logged with the
// features of what it returned, feedback is linked to the recall it judged, the
// mechanisms can be switched off one at a time, an online experiment can route a
// share of recalls to a variant, and learned models are stored with the
// evaluation that justified them.

// RecallConfig switches recall's mechanisms and sets its constants. The zero
// value is the full engine with the hand-set constants.
type RecallConfig struct {
	Name         string  `json:"name,omitempty"`
	NoKeyword    bool    `json:"noKeyword,omitempty"`    // ignore BM25
	NoSemantic   bool    `json:"noSemantic,omitempty"`   // ignore embeddings
	NoPrior      bool    `json:"noPrior,omitempty"`      // rank by relevance only, not strength and use
	NoSpreading  bool    `json:"noSpreading,omitempty"`  // direct matches only
	NoHebbian    bool    `json:"noHebbian,omitempty"`    // spread without coactive (learned) pathways
	NoInhibition bool    `json:"noInhibition,omitempty"` // keep near-duplicates
	NoOverride   bool    `json:"noOverride,omitempty"`   // don't demote memories the user overrode
	SpreadAbout  bool    `json:"spreadAbout,omitempty"`  // also spread along "about" edges (concepts, docs, files)
	RelWeight    float64 `json:"relWeight,omitempty"`    // relevance vs prior; default 0.75
	Damping      float64 `json:"damping,omitempty"`      // per-hop damping; default 0.9
	Beam         int     `json:"beam,omitempty"`         // activations spread per hop; default 10
	Ranker       string  `json:"ranker,omitempty"`       // a model id; "" = the hand-set formula, "active" = the active ranker
	Calibrator   string  `json:"calibrator,omitempty"`   // a model id, "active", or "" for none
	NoModels     bool    `json:"noModels,omitempty"`     // ignore active models (the hand-set baseline)
}

func (c RecallConfig) relWeight() float64 {
	if c.RelWeight > 0 {
		return min(1, c.RelWeight)
	}
	return 0.75
}

func (c RecallConfig) damping() float64 {
	if c.Damping > 0 {
		return min(1, c.Damping)
	}
	return 0.9
}

func (c RecallConfig) beam() int {
	if c.Beam > 0 {
		return c.Beam
	}
	return 10
}

// Baselines and ablations, by name, for evaluation and experiments.
func NamedConfig(name string) (RecallConfig, error) {
	c := RecallConfig{Name: name}
	switch name {
	case "full", "":
		c.Name = "full"
	case "hand":
		c.NoModels = true
	case "bm25":
		c.NoSemantic, c.NoPrior, c.NoSpreading, c.NoInhibition, c.NoOverride, c.NoModels = true, true, true, true, true, true
	case "embeddings":
		c.NoKeyword, c.NoPrior, c.NoSpreading, c.NoInhibition, c.NoOverride, c.NoModels = true, true, true, true, true, true
	case "hybrid":
		c.NoPrior, c.NoSpreading, c.NoInhibition, c.NoOverride, c.NoModels = true, true, true, true, true
	case "no-keyword":
		c.NoKeyword, c.NoModels = true, true
	case "no-semantic":
		c.NoSemantic, c.NoModels = true, true
	case "no-prior":
		c.NoPrior, c.NoModels = true, true
	case "no-spreading":
		c.NoSpreading, c.NoModels = true, true
	case "no-hebbian":
		c.NoHebbian, c.NoModels = true, true
	case "no-inhibition":
		c.NoInhibition, c.NoModels = true, true
	case "no-override":
		c.NoOverride, c.NoModels = true, true
	case "with-about":
		c.SpreadAbout, c.NoModels = true, true
	default:
		return c, fmt.Errorf("unknown configuration %q (try full, hand, bm25, embeddings, hybrid, or no-keyword, no-semantic, no-prior, no-spreading, no-hebbian, no-inhibition, no-override, with-about)", name)
	}
	return c, nil
}

// Baselines are compared against the engine in every evaluation.
var Baselines = []string{"bm25", "embeddings", "hybrid"}

// Ablations remove one mechanism each from the hand-set engine.
var Ablations = []string{"no-keyword", "no-semantic", "no-prior", "no-spreading", "no-hebbian", "no-inhibition", "no-override"}

// Additions are candidate mechanisms that are off by default until measured.
var Additions = []string{"with-about"}

// ---- recall log ----

// LoggedResult is one result of a logged recall.
type LoggedResult struct {
	ID         string         `json:"id"`
	Rank       int            `json:"rank"`
	Score      float64        `json:"score"`
	Confidence float64        `json:"conf"`
	Features   model.Features `json:"f"`
}

// LoggedRecall is one recall as it was served, with the feedback later linked to it.
type LoggedRecall struct {
	ID         string         `json:"id"`
	At         time.Time      `json:"at"`
	Client     string         `json:"client"`
	Query      string         `json:"query"`
	K          int            `json:"k"`
	Hops       int            `json:"hops"`
	Namespaces []string       `json:"namespaces,omitempty"`
	Variant    string         `json:"variant"`
	Results    []LoggedResult `json:"results"`
	// Grades of memories that got feedback within this recall: 2 accepted and
	// succeeded, 1 accepted, 0 overridden or failed.
	Grades map[string]float64 `json:"grades,omitempty"`
}

func (s *Store) logRecall(id, client string, in RecallInput, variant string, results []Scored) error {
	logged := make([]LoggedResult, len(results))
	for i, r := range results {
		logged[i] = LoggedResult{ID: r.Memory.ID, Rank: i + 1, Score: r.Score, Confidence: r.Confidence}
		if r.Features != nil {
			logged[i].Features = *r.Features
		}
	}
	rb, _ := json.Marshal(logged)
	nb, _ := json.Marshal(in.Namespaces)
	_, err := s.db.Exec(`INSERT INTO recall_log(id, at, client, query, k, hops, namespaces, variant, results)
		VALUES (?,?,?,?,?,?,?,?,?)`, id, ts(Now()), client, in.Query, in.K, in.Hops, string(nb), variant, string(rb))
	return err
}

// PruneRecallLog deletes logged recalls older than before that got no
// feedback; judged ones are kept as training and evaluation data.
func (s *Store) PruneRecallLog(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM recall_log WHERE at < ? AND id NOT IN (SELECT recall_id FROM feedback WHERE recall_id IS NOT NULL)`, ts(before))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// recallFor finds the recall a piece of feedback judges: the client's latest
// recall in the last two hours that returned the memory.
func recallFor(tx *sql.Tx, client, memoryID string, now time.Time) string {
	var id string
	err := tx.QueryRow(`SELECT id FROM recall_log WHERE client = ? AND at >= ? AND results LIKE ? ORDER BY at DESC, rowid DESC LIMIT 1`,
		client, ts(now.Add(-2*time.Hour)), `%"id":"`+memoryID+`"%`).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// Grade turns a feedback row into a relevance grade.
func Grade(outcome, objective string) float64 {
	switch {
	case outcome == "overridden" || objective == "failed":
		return 0
	case objective == "succeeded":
		return 2
	}
	return 1
}

// RecallLog returns logged recalls between from and to (zero times are open),
// oldest first, with their linked feedback grades. With labeledOnly, only
// recalls that got feedback are returned.
func (s *Store) RecallLog(from, to time.Time, labeledOnly bool) ([]LoggedRecall, error) {
	q := `SELECT id, at, client, query, k, hops, namespaces, variant, results FROM recall_log WHERE 1=1`
	var args []any
	if !from.IsZero() {
		q += ` AND at >= ?`
		args = append(args, ts(from))
	}
	if !to.IsZero() {
		q += ` AND at < ?`
		args = append(args, ts(to))
	}
	if labeledOnly {
		q += ` AND id IN (SELECT recall_id FROM feedback WHERE recall_id IS NOT NULL)`
	}
	rows, err := s.db.Query(q+` ORDER BY at, rowid`, args...)
	if err != nil {
		return nil, err
	}
	var out []LoggedRecall
	byID := map[string]int{}
	for rows.Next() {
		var r LoggedRecall
		var at, ns, res string
		if err := rows.Scan(&r.ID, &at, &r.Client, &r.Query, &r.K, &r.Hops, &ns, &r.Variant, &res); err != nil {
			rows.Close()
			return nil, err
		}
		r.At, _ = time.Parse(time.RFC3339, at)
		_ = json.Unmarshal([]byte(ns), &r.Namespaces)
		if err := json.Unmarshal([]byte(res), &r.Results); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recall %s: %w", r.ID, err)
		}
		byID[r.ID] = len(out)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	frows, err := s.db.Query(`SELECT recall_id, memory_id, outcome, COALESCE(objective,'') FROM feedback WHERE recall_id IS NOT NULL ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var rid, mid, outcome, objective string
		if err := frows.Scan(&rid, &mid, &outcome, &objective); err != nil {
			return nil, err
		}
		i, ok := byID[rid]
		if !ok {
			continue
		}
		if out[i].Grades == nil {
			out[i].Grades = map[string]float64{}
		}
		out[i].Grades[mid] = Grade(outcome, objective) // the latest judgment wins
	}
	return out, frows.Err()
}

// ---- online experiments ----

// Experiment routes a share of real recalls to a variant configuration.
type Experiment struct {
	Name    string       `json:"name"`
	Variant RecallConfig `json:"variant"`
	Share   float64      `json:"share"` // 0..1 of recalls that get the variant
	Started string       `json:"started"`
}

const experimentKey = "recall.experiment"

// StartExperiment begins (or replaces) the online experiment.
func (s *Store) StartExperiment(e Experiment) error {
	if e.Name == "" || strings.ContainsAny(e.Name, "/ ") {
		return fmt.Errorf("an experiment needs a name without spaces or slashes")
	}
	if e.Share <= 0 || e.Share >= 1 {
		return fmt.Errorf("share must be between 0 and 1, not %v", e.Share)
	}
	e.Started = ts(Now())
	b, _ := json.Marshal(e)
	return s.SetSetting(experimentKey, string(b))
}

// StopExperiment ends the online experiment; its logged recalls stay.
func (s *Store) StopExperiment() error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, experimentKey)
	return err
}

// CurrentExperiment returns the running experiment, if any.
func (s *Store) CurrentExperiment() (*Experiment, error) {
	v, err := s.Setting(experimentKey)
	if err != nil || v == "" {
		return nil, err
	}
	var e Experiment
	if err := json.Unmarshal([]byte(v), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// assign decides a recall's arm from its id, so assignment is random but replayable.
func assign(recallID string, share float64) bool {
	h := sha256.Sum256([]byte(recallID))
	return float64(binary.BigEndian.Uint64(h[:8])>>11)/float64(1<<53) < share
}

// ---- models ----

// ModelRecord is a stored model.
type ModelRecord struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Algo        string          `json:"algo"`
	Status      string          `json:"status"` // candidate, active, retired
	CreatedAt   string          `json:"createdAt"`
	ActivatedAt string          `json:"activatedAt,omitempty"`
	Features    []string        `json:"features"`
	Params      json.RawMessage `json:"-"`
	Train       json.RawMessage `json:"train,omitempty"`   // what it was trained on
	Metrics     json.RawMessage `json:"metrics,omitempty"` // held-out evaluation
}

// SaveModel stores a model as a candidate and returns its id.
func (s *Store) SaveModel(m ModelRecord) (string, error) {
	if m.Kind == "" || m.Algo == "" || len(m.Params) == 0 {
		return "", fmt.Errorf("a model needs a kind, an algorithm and parameters")
	}
	if _, err := model.Load(m.Algo, m.Params); err != nil {
		return "", err
	}
	m.ID = "mdl_" + newID()
	fb, _ := json.Marshal(m.Features)
	_, err := s.db.Exec(`INSERT INTO models(id, kind, algo, status, created_at, features, params, train, metrics)
		VALUES (?,?,?,?,?,?,?,?,?)`, m.ID, m.Kind, m.Algo, "candidate", ts(Now()), string(fb), string(m.Params),
		nullable(string(m.Train)), nullable(string(m.Metrics)))
	return m.ID, err
}

// SetModelMetrics records a model's held-out evaluation.
func (s *Store) SetModelMetrics(id string, metrics any) error {
	b, _ := json.Marshal(metrics)
	_, err := s.db.Exec(`UPDATE models SET metrics = ? WHERE id = ?`, string(b), id)
	return err
}

// Models lists stored models, newest first; kind "" lists every kind.
func (s *Store) Models(kind string) ([]ModelRecord, error) {
	q := `SELECT id, kind, algo, status, created_at, COALESCE(activated_at,''), features, params, COALESCE(train,''), COALESCE(metrics,'') FROM models`
	var args []any
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	rows, err := s.db.Query(q+` ORDER BY created_at DESC, rowid DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelRecord
	for rows.Next() {
		var m ModelRecord
		var fb, params, train, metrics string
		if err := rows.Scan(&m.ID, &m.Kind, &m.Algo, &m.Status, &m.CreatedAt, &m.ActivatedAt, &fb, &params, &train, &metrics); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(fb), &m.Features)
		m.Params = json.RawMessage(params)
		if train != "" {
			m.Train = json.RawMessage(train)
		}
		if metrics != "" {
			m.Metrics = json.RawMessage(metrics)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ModelByID returns one model.
func (s *Store) ModelByID(id string) (ModelRecord, error) {
	all, err := s.Models("")
	if err != nil {
		return ModelRecord{}, err
	}
	for _, m := range all {
		if m.ID == id || (len(id) >= 8 && strings.HasPrefix(m.ID, id)) {
			return m, nil
		}
	}
	return ModelRecord{}, fmt.Errorf("no model %q", id)
}

// ActivateModel makes a model the active one of its kind; the previous one is retired.
func (s *Store) ActivateModel(id string) error {
	m, err := s.ModelByID(id)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE models SET status = 'retired' WHERE kind = ? AND status = 'active'`, m.Kind); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE models SET status = 'active', activated_at = ? WHERE id = ?`, ts(Now()), m.ID); err != nil {
		return err
	}
	if err := audit(tx, "engram-cli", "activate-model", "", fmt.Sprintf(`{"id":%q,"kind":%q,"algo":%q}`, m.ID, m.Kind, m.Algo)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.models.reset()
	return nil
}

// DeactivateModels retires the active model of a kind, going back to the hand-set rules.
func (s *Store) DeactivateModels(kind string) (int, error) {
	res, err := s.db.Exec(`UPDATE models SET status = 'retired' WHERE kind = ? AND status = 'active'`, kind)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	s.models.reset()
	return int(n), nil
}

// modelCache keeps loaded models by id, and the active id per kind for a short while.
type modelCache struct {
	mu      sync.Mutex
	loaded  map[string]any
	active  map[string]string
	checked time.Time
}

func (c *modelCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active, c.checked = nil, time.Time{}
}

// resolveModel returns the model a config names: an id, "active" (the active
// model of the kind, if any), or "" (none).
func (s *Store) resolveModel(kind, ref string) (any, string, error) {
	if ref == "" {
		return nil, "", nil
	}
	c := &s.models
	c.mu.Lock()
	defer c.mu.Unlock()
	if ref == "active" {
		if c.active == nil || time.Since(c.checked) > 30*time.Second {
			c.active = map[string]string{}
			rows, err := s.db.Query(`SELECT kind, id FROM models WHERE status = 'active'`)
			if err != nil {
				return nil, "", err
			}
			for rows.Next() {
				var k, id string
				if err := rows.Scan(&k, &id); err == nil {
					c.active[k] = id
				}
			}
			rows.Close()
			c.checked = time.Now()
		}
		ref = c.active[kind]
		if ref == "" {
			return nil, "", nil
		}
	}
	if m, ok := c.loaded[ref]; ok {
		return m, ref, nil
	}
	var algo, params string
	if err := s.db.QueryRow(`SELECT algo, params FROM models WHERE id = ?`, ref).Scan(&algo, &params); err != nil {
		return nil, "", fmt.Errorf("model %s: %w", ref, err)
	}
	m, err := model.Load(algo, []byte(params))
	if err != nil {
		return nil, "", err
	}
	if c.loaded == nil {
		c.loaded = map[string]any{}
	}
	c.loaded[ref] = m
	return m, ref, nil
}

// ---- evaluation runs ----

// EvalRun is one recorded evaluation: what was measured, on what, with what result.
type EvalRun struct {
	ID      int64           `json:"id"`
	At      string          `json:"at"`
	Kind    string          `json:"kind"` // ranking, permits, bench
	Name    string          `json:"name"`
	Config  json.RawMessage `json:"config"`
	Dataset json.RawMessage `json:"dataset"`
	Metrics json.RawMessage `json:"metrics"`
	Notes   string          `json:"notes,omitempty"`
}

// RecordEvalRun stores an evaluation and returns its id.
func (s *Store) RecordEvalRun(kind, name string, config, dataset, metrics any, notes string) (int64, error) {
	cb, _ := json.Marshal(config)
	db, _ := json.Marshal(dataset)
	mb, _ := json.Marshal(metrics)
	res, err := s.db.Exec(`INSERT INTO eval_runs(at, kind, name, config, dataset, metrics, notes) VALUES (?,?,?,?,?,?,?)`,
		ts(Now()), kind, name, string(cb), string(db), string(mb), nullable(notes))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EvalRuns lists recorded evaluations, newest first.
func (s *Store) EvalRuns(kind string, limit int) ([]EvalRun, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, at, kind, name, config, dataset, metrics, COALESCE(notes,'') FROM eval_runs`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	args = append(args, limit)
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvalRun
	for rows.Next() {
		var r EvalRun
		var c, d, m string
		if err := rows.Scan(&r.ID, &r.At, &r.Kind, &r.Name, &c, &d, &m, &r.Notes); err != nil {
			return nil, err
		}
		r.Config, r.Dataset, r.Metrics = json.RawMessage(c), json.RawMessage(d), json.RawMessage(m)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EvalRunByID returns one evaluation.
func (s *Store) EvalRunByID(id int64) (EvalRun, error) {
	var r EvalRun
	var c, d, m string
	err := s.db.QueryRow(`SELECT id, at, kind, name, config, dataset, metrics, COALESCE(notes,'') FROM eval_runs WHERE id = ?`, id).
		Scan(&r.ID, &r.At, &r.Kind, &r.Name, &c, &d, &m, &r.Notes)
	if err == sql.ErrNoRows {
		return r, fmt.Errorf("no evaluation run %d", id)
	}
	r.Config, r.Dataset, r.Metrics = json.RawMessage(c), json.RawMessage(d), json.RawMessage(m)
	return r, err
}
