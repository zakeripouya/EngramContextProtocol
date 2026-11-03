package permit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Thresholds for an "allow" prediction inside the eligible tier.
var (
	MinPrecedents = 8    // real, resolved decisions in the same category (8 of 8 is the first to reach 0.90)
	MinConfidence = 0.90 // posterior probability that the user approves and it works
)

// Denial inference: Claude Code fires no hook when the user denies a prompt.
var (
	DeniedAfterMoveOn = 30 * time.Second // the session did something else at least this long after the request
	DeniedAfterAge    = 6 * time.Hour    // or the request is simply this old
)

// SettingEnabled turns logging on or off (the kill switch). Default on once the hook is installed.
const SettingEnabled = "permit.enabled"

// Prediction is what Engram would have answered.
type Prediction struct {
	Classification
	Decision   string  `json:"decision"` // allow or ask
	Confidence float64 `json:"confidence"`
	Precedents int     `json:"precedents"`
	// Set when an activated learned model made the call (still shadow mode).
	Model           string  `json:"model,omitempty"`
	ModelConfidence float64 `json:"modelConfidence,omitempty"`
}

// Predict combines the rule tier with the user's past decisions in the same category.
// Confidence is the posterior mean of a Beta(1,1) prior over "approved and did not
// fail": a failed run after an approval counts against, so the model learns from
// outcomes, not only from approvals.
func Predict(st *store.Store, client, tool string, input map[string]any, cwd string) (Prediction, error) {
	c := Classify(tool, input, cwd)
	p := Prediction{Classification: c, Decision: "ask"}
	if c.Tier == Never {
		p.Reason = "never auto-approved: " + c.Reason
		return p, nil
	}
	approved, denied, failed, err := st.PermitPrecedents(client, c.Category)
	if err != nil {
		return p, err
	}
	n := approved + denied
	p.Precedents = n
	good := float64(approved - failed)
	p.Confidence = round3((good + 1) / (float64(n) + 2))
	switch {
	case c.Tier == Unknown:
		p.Reason = "always ask: " + c.Reason
	case n < MinPrecedents:
		p.Reason = fmt.Sprintf("eligible, but only %d of %d precedents", n, MinPrecedents)
	case p.Confidence < MinConfidence:
		p.Reason = fmt.Sprintf("eligible, but confidence %.2f is under %.2f (%d approved, %d denied, %d failed)",
			p.Confidence, MinConfidence, approved, denied, failed)
	default:
		p.Decision = "allow"
		p.Reason = fmt.Sprintf("eligible; you approved %d of %d similar requests (%d failed afterwards)", approved, n, failed)
	}
	// An activated learned model replaces the count rule inside the same safety
	// rails: only the eligible tier, and only with enough precedents.
	if sc, id := activeModel(st); sc != nil && c.Tier != Never {
		if hist, err := Chronological(st); err == nil {
			h := NewHistory()
			for _, r := range hist {
				if r.Client == client {
					h.Add(r)
				}
			}
			req := store.PermitRequest{Client: client, Tool: tool, InputHash: inputHash(tool, input), Category: c.Category,
				Tier: c.Tier, At: store.Now().Format(time.RFC3339)}
			pm := round3(sc.Score(h.Vector(req)))
			p.Model, p.ModelConfidence = id, pm
			switch {
			case c.Tier == Eligible && n >= MinPrecedents && pm >= MinConfidence:
				p.Decision = "allow"
				p.Reason = fmt.Sprintf("eligible; learned model %s gives %.2f (%d precedents)", id, pm, n)
			case p.Decision == "allow":
				p.Decision = "ask"
				p.Reason = fmt.Sprintf("eligible, but learned model %s gives only %.2f", id, pm)
			}
		}
	}
	return p, nil
}

func round3(x float64) float64 { return float64(int(x*1000+0.5)) / 1000 }

// HookInput is the JSON a Claude Code hook receives on stdin.
type HookInput struct {
	SessionID string         `json:"session_id"`
	Cwd       string         `json:"cwd"`
	Event     string         `json:"hook_event_name"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

func inputHash(tool string, input map[string]any) string {
	b, _ := json.Marshal(input) // map keys are sorted, so equal inputs hash equally
	h := sha256.Sum256(append([]byte(tool+"\x00"), b...))
	return hex.EncodeToString(h[:12])
}

// Enabled reports whether the kill switch is off.
func Enabled(st *store.Store) bool {
	v, err := st.Setting(SettingEnabled)
	return err != nil || v != "false"
}

// HandleHook processes one Claude Code hook event in shadow mode. It never writes a
// decision: the caller prints nothing and exits 0, so Claude Code asks the user as
// it would without Engram. Every error is swallowed by the caller for the same reason.
func HandleHook(st *store.Store, client string, r io.Reader) (*Prediction, error) {
	var in HookInput
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&in); err != nil {
		return nil, err
	}
	if !Enabled(st) || in.SessionID == "" || in.ToolName == "" {
		return nil, nil
	}
	hash := inputHash(in.ToolName, in.ToolInput)
	switch in.Event {
	case "PermissionRequest":
		_, _ = st.InferPermitDenials(DeniedAfterMoveOn, DeniedAfterAge)
		_ = st.NotePermitActivity(client, in.SessionID)
		p, err := Predict(st, client, in.ToolName, in.ToolInput, in.Cwd)
		if err != nil {
			return nil, err
		}
		_, err = st.LogPermit(store.PermitRequest{Client: client, SessionID: in.SessionID, Tool: in.ToolName, InputHash: hash,
			Summary: p.Summary, Category: p.Category, Tier: p.Tier, Predicted: p.Decision, Confidence: p.Confidence,
			Precedents: p.Precedents, Reason: p.Reason})
		return &p, err
	case "PostToolUse", "PostToolUseFailure":
		objective := "ok"
		if in.Event == "PostToolUseFailure" {
			objective = "failed"
		}
		_, err := st.ResolvePermitRan(client, in.SessionID, in.ToolName, hash, objective)
		_ = st.NotePermitActivity(client, in.SessionID)
		return nil, err
	}
	return nil, nil
}

// TierStats is agreement for one tier.
type TierStats struct {
	Tier            string  `json:"tier"`
	Requests        int     `json:"requests"`
	Resolved        int     `json:"resolved"`
	Approved        int     `json:"approved"`
	Denied          int     `json:"denied"`
	PredictedAllow  int     `json:"predictedAllow"`
	AllowApproved   int     `json:"allowApproved"`  // predicted allow and the user approved
	AllowDenied     int     `json:"allowDenied"`    // predicted allow but the user denied: the costly mistake
	AllowFailed     int     `json:"allowFailed"`    // predicted allow, approved, then failed
	Precision       float64 `json:"precision"`      // AllowApproved / (AllowApproved + AllowDenied)
	PromptsAvoided  float64 `json:"promptsAvoided"` // share of approved requests Engram would have answered
	ReadyForAuto    bool    `json:"readyForAuto"`
	ReadinessReason string  `json:"readinessReason"`
}

// Report is the shadow-mode agreement report.
type Report struct {
	Enabled   bool                  `json:"enabled"`
	Tiers     []TierStats           `json:"tiers"`
	Mistakes  []store.PermitRequest `json:"mistakes"` // predicted allow, user denied
	Recent    []store.PermitRequest `json:"recent"`
	Threshold string                `json:"threshold"`
}

// Readiness criteria before auto-approval could be considered for a tier (it is not
// implemented: this milestone is shadow mode only).
var (
	ReadyMinAllows    = 50
	ReadyMinPrecision = 0.98
)

// BuildReport computes agreement per tier from the log.
func BuildReport(st *store.Store, limit int) (Report, error) {
	_, _ = st.InferPermitDenials(DeniedAfterMoveOn, DeniedAfterAge)
	all, err := st.Permits(limit)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Enabled: Enabled(st), Mistakes: []store.PermitRequest{},
		Threshold: fmt.Sprintf("allow needs the eligible tier, %d precedents and confidence ≥ %.2f", MinPrecedents, MinConfidence)}
	byTier := map[string]*TierStats{}
	for _, tier := range []string{Eligible, Unknown, Never} {
		byTier[tier] = &TierStats{Tier: tier}
	}
	for _, r := range all {
		t := byTier[r.Tier]
		if t == nil {
			t = &TierStats{Tier: r.Tier}
			byTier[r.Tier] = t
		}
		t.Requests++
		if r.Predicted == "allow" {
			t.PredictedAllow++
		}
		if r.Outcome == "pending" {
			continue
		}
		t.Resolved++
		if r.Outcome == "approved" {
			t.Approved++
		} else {
			t.Denied++
		}
		if r.Predicted == "allow" {
			switch {
			case r.Outcome == "denied":
				t.AllowDenied++
				rep.Mistakes = append(rep.Mistakes, r)
			case r.Objective == "failed":
				t.AllowFailed++
				t.AllowApproved++
			default:
				t.AllowApproved++
			}
		}
	}
	for _, t := range byTier {
		if n := t.AllowApproved + t.AllowDenied; n > 0 {
			t.Precision = round3(float64(t.AllowApproved) / float64(n))
		}
		if t.Approved > 0 {
			t.PromptsAvoided = round3(float64(t.AllowApproved) / float64(t.Approved))
		}
		switch {
		case t.Tier != Eligible:
			t.ReadinessReason = "this tier always asks"
		case t.AllowApproved+t.AllowDenied < ReadyMinAllows:
			t.ReadinessReason = fmt.Sprintf("needs %d resolved allow predictions, has %d", ReadyMinAllows, t.AllowApproved+t.AllowDenied)
		case t.Precision < ReadyMinPrecision:
			t.ReadinessReason = fmt.Sprintf("precision %.3f is under %.2f", t.Precision, ReadyMinPrecision)
		default:
			t.ReadyForAuto = true
			t.ReadinessReason = "agreement is high enough to consider auto-approval (not enabled in this version)"
		}
		rep.Tiers = append(rep.Tiers, *t)
	}
	order := map[string]int{Eligible: 0, Unknown: 1, Never: 2}
	sort.Slice(rep.Tiers, func(i, j int) bool { return order[rep.Tiers[i].Tier] < order[rep.Tiers[j].Tier] })
	rep.Recent = all[:min(len(all), 20)]
	return rep, nil
}
