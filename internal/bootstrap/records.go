package bootstrap

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Record is the generic import format: a JSON array of these, or {"records": [...]}.
//
//	[{"key": "note-1", "kind": "fact", "content": "...", "tags": ["x"], "validFrom": "2026-08-01"}]
//
// Imported records are system-authored, so kind "decision" is not accepted:
// decisions come only from the user.
type Record struct {
	Key       string   `json:"key"`
	Kind      string   `json:"kind"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	ValidFrom string   `json:"validFrom"`
}

// ImportResult reports what a record import created.
type ImportResult struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

// ImportRecords imports generic JSON records into a namespace. Records without a key
// are keyed by their content, so running the same import twice does nothing.
func ImportRecords(st *store.Store, r io.Reader, namespace, service string) (ImportResult, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return ImportResult{}, err
	}
	var recs []Record
	if err := json.Unmarshal(raw, &recs); err != nil {
		var wrapped struct {
			Records []Record `json:"records"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil || wrapped.Records == nil {
			return ImportResult{}, fmt.Errorf("expected a JSON array of records or {\"records\": [...]}: %w", err)
		}
		recs = wrapped.Records
	}
	if service == "" {
		service = "import"
	}
	var res ImportResult
	for i, rec := range recs {
		kind := rec.Kind
		if kind == "" {
			kind = "fact"
		}
		if kind != "fact" && kind != "episode" && kind != "concept" {
			return res, fmt.Errorf("record %d: kind must be fact, episode or concept (imports cannot create decisions)", i)
		}
		key := rec.Key
		if key == "" {
			key = rec.Content
		}
		var vf time.Time
		if rec.ValidFrom != "" {
			if vf, err = store.ParseTime(rec.ValidFrom); err != nil {
				return res, fmt.Errorf("record %d: %w", i, err)
			}
		}
		_, created, err := st.InsertSystem(store.SystemMemory{
			Key: service + ":" + key, Kind: kind, Namespace: namespace, Service: service,
			Content: rec.Content, Tags: rec.Tags, Strength: ImportStrength, ValidFrom: vf,
		})
		if err != nil {
			return res, fmt.Errorf("record %d: %w", i, err)
		}
		if created {
			res.Imported++
		} else {
			res.Skipped++
		}
	}
	return res, nil
}

// jiraExport matches the JSON returned by Jira's issue search API (and saved exports of it).
type jiraExport struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary   string `json:"summary"`
			Created   string `json:"created"`
			Resolved  string `json:"resolutiondate"`
			IssueType struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Status struct {
				Name string `json:"name"`
			} `json:"status"`
			Labels []string `json:"labels"`
		} `json:"fields"`
	} `json:"issues"`
}

// ImportJira imports issues from a Jira search API response saved as JSON. Each issue
// becomes an episode: "INKIN-212 (Bug, Done): Payout job fails on refunds".
func ImportJira(st *store.Store, r io.Reader, namespace string) (ImportResult, error) {
	var ex jiraExport
	if err := json.NewDecoder(r).Decode(&ex); err != nil {
		return ImportResult{}, fmt.Errorf("not a Jira search export: %w", err)
	}
	var res ImportResult
	for _, is := range ex.Issues {
		f := is.Fields
		if is.Key == "" || strings.TrimSpace(f.Summary) == "" {
			continue
		}
		var meta []string
		if f.IssueType.Name != "" {
			meta = append(meta, f.IssueType.Name)
		}
		if f.Status.Name != "" {
			meta = append(meta, f.Status.Name)
		}
		content := is.Key
		if len(meta) > 0 {
			content += " (" + strings.Join(meta, ", ") + ")"
		}
		content += ": " + f.Summary
		var vf time.Time
		if t, err := parseJiraTime(f.Created); err == nil {
			vf = t
		}
		project := is.Key
		if i := strings.Index(project, "-"); i > 0 {
			project = project[:i]
		}
		_, created, err := st.InsertSystem(store.SystemMemory{
			Key: "jira:" + is.Key, Kind: "episode", Namespace: namespace, Service: "jira", Content: content,
			Tags: append([]string{"jira", project}, f.Labels...), Strength: ImportStrength, ValidFrom: vf,
		})
		if err != nil {
			return res, err
		}
		if created {
			res.Imported++
		} else {
			res.Skipped++
		}
	}
	return res, nil
}

func parseJiraTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}
