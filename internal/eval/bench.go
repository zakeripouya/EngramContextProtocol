package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/llm"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Public long-term-memory benchmarks. Each sample is a conversation history
// (turns) and questions about it, with the turns that hold the evidence. Engram
// stores every turn as an episode in a fresh in-memory store, then each
// configuration recalls for each question: retrieval is scored against the
// evidence (Recall@k, NDCG@k, MRR), and with a chat model, answers are scored
// by an LLM judge.

// BenchTurn is one message of a conversation.
type BenchTurn struct {
	Key     string // dataset id: LoCoMo "D1:3"; LongMemEval "<session>:<n>"
	Session string
	Speaker string
	Text    string
	When    time.Time
}

// BenchQuestion is one question with its evidence turns.
type BenchQuestion struct {
	ID       string
	Question string
	Answer   string
	Evidence []string // turn keys
	Category string
	When     time.Time // when the question is asked
}

// BenchSample is one conversation history and its questions.
type BenchSample struct {
	ID        string
	Turns     []BenchTurn
	Questions []BenchQuestion
}

// LoCoMo's question categories, as numbered in its data.
var locomoCategory = map[int]string{1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop", 5: "adversarial"}

var evidenceSplit = regexp.MustCompile(`[;,\s]+`)

// LoadLoCoMo reads locomo10.json (github.com/snap-research/locomo). Adversarial
// questions (category 5), whose answer is not in the conversation, are skipped
// unless includeAdversarial is set.
func LoadLoCoMo(r io.Reader, includeAdversarial bool) ([]BenchSample, error) {
	var raw []struct {
		SampleID     string                     `json:"sample_id"`
		Conversation map[string]json.RawMessage `json:"conversation"`
		QA           []struct {
			Question string          `json:"question"`
			Answer   json.RawMessage `json:"answer"`
			Evidence []string        `json:"evidence"`
			Category int             `json:"category"`
		} `json:"qa"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("not a LoCoMo file: %w", err)
	}
	var out []BenchSample
	for si, s := range raw {
		sample := BenchSample{ID: s.SampleID}
		if sample.ID == "" {
			sample.ID = fmt.Sprintf("sample-%d", si)
		}
		var sessions []int
		for k := range s.Conversation {
			var n int
			if _, err := fmt.Sscanf(k, "session_%d", &n); err == nil && !strings.Contains(k, "date") {
				sessions = append(sessions, n)
			}
		}
		sort.Ints(sessions)
		var last time.Time
		for _, n := range sessions {
			var when string
			_ = json.Unmarshal(s.Conversation[fmt.Sprintf("session_%d_date_time", n)], &when)
			t := parseLoCoMoTime(when)
			if t.IsZero() {
				t = last.Add(24 * time.Hour)
			}
			last = t
			var turns []struct {
				Speaker     string `json:"speaker"`
				DiaID       string `json:"dia_id"`
				Text        string `json:"text"`
				BlipCaption string `json:"blip_caption"`
			}
			if err := json.Unmarshal(s.Conversation[fmt.Sprintf("session_%d", n)], &turns); err != nil {
				continue
			}
			for i, tr := range turns {
				text := tr.Text
				if tr.BlipCaption != "" {
					text += " [shares a photo: " + tr.BlipCaption + "]"
				}
				sample.Turns = append(sample.Turns, BenchTurn{Key: tr.DiaID, Session: fmt.Sprint(n), Speaker: tr.Speaker,
					Text: text, When: t.Add(time.Duration(i) * time.Minute)})
			}
		}
		for qi, q := range s.QA {
			if q.Category == 5 && !includeAdversarial {
				continue
			}
			var ev []string
			for _, e := range q.Evidence {
				for _, part := range evidenceSplit.Split(e, -1) {
					if part = strings.TrimSpace(part); part != "" {
						ev = append(ev, part)
					}
				}
			}
			if len(ev) == 0 {
				continue
			}
			sample.Questions = append(sample.Questions, BenchQuestion{ID: fmt.Sprintf("%s/q%d", sample.ID, qi), Question: q.Question,
				Answer: rawString(q.Answer), Evidence: ev, Category: locomoCategory[q.Category], When: last.Add(24 * time.Hour)})
		}
		out = append(out, sample)
	}
	return out, nil
}

func rawString(b json.RawMessage) string {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	return strings.Trim(string(b), `"`)
}

func parseLoCoMoTime(s string) time.Time {
	for _, layout := range []string{"3:04 pm on 2 January, 2006", "3:04 pm on 2 Jan, 2006", "3:04 PM on 2 January, 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// LoadLongMemEval reads a LongMemEval file (longmemeval_s.json, _m or _oracle;
// github.com/xiaowu0162/LongMemEval). Each question has its own history.
// Evidence is the turns marked has_answer; when none are marked, every turn of
// the answer sessions. Abstention questions (ids ending in _abs) are skipped.
func LoadLongMemEval(r io.Reader) ([]BenchSample, error) {
	var raw []struct {
		QuestionID   string          `json:"question_id"`
		QuestionType string          `json:"question_type"`
		Question     string          `json:"question"`
		Answer       json.RawMessage `json:"answer"`
		QuestionDate string          `json:"question_date"`
		SessionIDs   []string        `json:"haystack_session_ids"`
		Dates        []string        `json:"haystack_dates"`
		Sessions     [][]struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			HasAnswer bool   `json:"has_answer"`
		} `json:"haystack_sessions"`
		AnswerSessions []string `json:"answer_session_ids"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("not a LongMemEval file: %w", err)
	}
	var out []BenchSample
	for _, q := range raw {
		if strings.HasSuffix(q.QuestionID, "_abs") {
			continue
		}
		s := BenchSample{ID: q.QuestionID}
		answerSession := map[string]bool{}
		for _, id := range q.AnswerSessions {
			answerSession[id] = true
		}
		var marked, inAnswer []string
		for i, sess := range q.Sessions {
			sid := fmt.Sprint(i)
			if i < len(q.SessionIDs) {
				sid = q.SessionIDs[i]
			}
			var when time.Time
			if i < len(q.Dates) {
				when = parseLMETime(q.Dates[i])
			}
			for j, t := range sess {
				key := fmt.Sprintf("%s:%d", sid, j)
				s.Turns = append(s.Turns, BenchTurn{Key: key, Session: sid, Speaker: t.Role, Text: t.Content,
					When: when.Add(time.Duration(j) * time.Minute)})
				if t.HasAnswer {
					marked = append(marked, key)
				}
				if answerSession[sid] {
					inAnswer = append(inAnswer, key)
				}
			}
		}
		ev := marked
		if len(ev) == 0 {
			ev = inAnswer
		}
		if len(ev) == 0 {
			continue
		}
		s.Questions = []BenchQuestion{{ID: q.QuestionID, Question: q.Question, Answer: rawString(q.Answer), Evidence: ev,
			Category: q.QuestionType, When: parseLMETime(q.QuestionDate)}}
		out = append(out, s)
	}
	return out, nil
}

func parseLMETime(s string) time.Time {
	if i := strings.Index(s, " ("); i > 0 { // "2023/05/30 (Tue) 23:40"
		s = s[:i] + s[strings.Index(s, ")")+1:]
	}
	for _, layout := range []string{"2006/01/02 15:04", "2006/01/02", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// BenchOptions controls RunBench.
type BenchOptions struct {
	K            int      // results per question; default 10
	Configs      []string // configurations to compare; default bm25, embeddings, hybrid, full
	Embedder     embed.Embedder
	LinkTurns    bool     // also test an ingest that links consecutive turns as one episode (coactive 0.3)
	Chat         llm.Chat // answer each question from the recalled memories and judge it
	QAConfigs    []string // configurations to answer with (default: the first and last of Configs)
	MaxQuestions int      // 0 = all
	Progress     func(done, total int)
	Seed         int64
}

// BenchSystem is one configuration's scores.
type BenchSystem struct {
	Name          string             `json:"name"`
	Questions     int                `json:"questions"`
	RecallAtK     Estimate           `json:"recallAtK"`
	RecallAt5     Estimate           `json:"recallAt5"`
	NDCG          Estimate           `json:"ndcgAtK"`
	MRR           Estimate           `json:"mrr"`
	ContextTokens float64            `json:"contextTokens"` // mean tokens of recalled text per question (≈ words × 1.3)
	ByCategory    map[string]float64 `json:"recallAtKByCategory"`
	Accuracy      *Estimate          `json:"accuracy,omitempty"` // LLM-judged answers
	AccuracyByCat map[string]float64 `json:"accuracyByCategory,omitempty"`
	VsFirst       *Comparison        `json:"vsFirst,omitempty"` // Recall@k against the first configuration
	recall        []float64
	cats          map[string][]float64 // recall@k per category
	acc           []float64
	accCats       map[string][]float64
}

// BenchReport is the result of RunBench.
type BenchReport struct {
	Dataset   string        `json:"dataset"`
	Samples   int           `json:"samples"`
	Turns     int           `json:"turns"`
	Questions int           `json:"questions"`
	K         int           `json:"k"`
	Embedder  string        `json:"embedder"`
	Judge     string        `json:"judge,omitempty"`
	Systems   []BenchSystem `json:"systems"`
	Elapsed   string        `json:"elapsed"`
	Warnings  []string      `json:"warnings,omitempty"`
}

func approxTokens(s string) float64 { return float64(len(strings.Fields(s))) * 1.3 }

// RunBench ingests each sample into a fresh store and scores every configuration.
func RunBench(ctx context.Context, dataset string, samples []BenchSample, o BenchOptions) (BenchReport, error) {
	start := time.Now()
	if o.K <= 0 {
		o.K = 10
	}
	if len(o.Configs) == 0 {
		o.Configs = []string{"bm25", "embeddings", "hybrid", "full"}
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
	if o.Embedder == nil {
		o.Embedder = embed.NewHashing(embed.DefaultHashDims)
	}
	rep := BenchReport{Dataset: dataset, K: o.K, Embedder: o.Embedder.Model()}
	if _, hashing := o.Embedder.(*embed.Hashing); hashing {
		rep.Warnings = append(rep.Warnings, "the hashing embedder matches words, not meaning; run with a real embedding model (ENGRAM_EMBED_URL) before quoting these numbers")
	}
	type sys struct {
		name   string
		cfg    store.RecallConfig
		linked bool
	}
	var systems []sys
	for _, name := range o.Configs {
		cfg, err := store.NamedConfig(name)
		if err != nil {
			return rep, err
		}
		cfg.NoModels = true // public benchmarks measure the mechanisms, not models trained on your data
		systems = append(systems, sys{name, cfg, false})
	}
	if o.LinkTurns {
		cfg, _ := store.NamedConfig("full")
		cfg.NoModels = true
		systems = append(systems, sys{"full+episodes", cfg, true})
	}
	qaSet := map[string]bool{}
	if o.Chat != nil {
		if len(o.QAConfigs) == 0 {
			o.QAConfigs = []string{systems[0].name, systems[len(systems)-1].name}
		}
		for _, n := range o.QAConfigs {
			qaSet[n] = true
		}
		rep.Judge = "LLM judge (the configured sleep-cycle model)"
	}
	results := make([]BenchSystem, len(systems))
	for i, s := range systems {
		results[i] = BenchSystem{Name: s.name, cats: map[string][]float64{}, accCats: map[string][]float64{}}
	}
	total := 0
	for _, s := range samples {
		total += len(s.Questions)
	}
	if o.MaxQuestions > 0 && total > o.MaxQuestions {
		total = o.MaxQuestions
	}
	done := 0
	var tokens = make([]float64, len(systems))
	for _, sample := range samples {
		if done >= total {
			break
		}
		rep.Samples++
		rep.Turns += len(sample.Turns)
		stores := map[bool]*store.Store{}
		keyOf := map[bool]map[string]string{}
		build := func(linked bool) error {
			st, err := store.Open(":memory:")
			if err != nil {
				return err
			}
			st.SetEmbedder(o.Embedder)
			ids := map[string]string{}
			var prev, prevSession string
			for _, t := range sample.Turns {
				content := fmt.Sprintf("[%s] %s: %s", t.When.Format("2 Jan 2006"), t.Speaker, t.Text)
				id, _, err := st.InsertSystem(store.SystemMemory{Key: "bench:" + t.Key, Kind: "episode", Namespace: "bench",
					Service: "bench", Content: content, ValidFrom: t.When, Strength: 0.5})
				if err != nil {
					if strings.Contains(err.Error(), "looks like it contains") {
						continue // the secret filter caught something; it is simply not stored
					}
					st.Close()
					return err
				}
				ids[id] = t.Key
				if linked && prev != "" && prevSession == t.Session {
					if _, err := st.AddEdgeIfAbsent(prev, id, "coactive", 0.3); err != nil {
						st.Close()
						return err
					}
				}
				prev, prevSession = id, t.Session
			}
			stores[linked], keyOf[linked] = st, ids
			return nil
		}
		if err := build(false); err != nil {
			return rep, err
		}
		if o.LinkTurns {
			if err := build(true); err != nil {
				return rep, err
			}
		}
		for _, q := range sample.Questions {
			if done >= total {
				break
			}
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			done++
			grades := map[string]float64{}
			for _, e := range q.Evidence {
				grades[e] = 1
			}
			for i, s := range systems {
				st := stores[s.linked]
				cfg := s.cfg
				res, err := st.RecallSystem(store.RecallInput{Query: q.Question, K: o.K, Hops: 2, Namespaces: []string{"bench"}, Config: &cfg})
				if err != nil {
					res = nil // a query with no searchable words counts as a miss
				}
				r := Ranked{Grades: grades, Relevant: len(q.Evidence)}
				var context []string
				for _, x := range res {
					r.IDs = append(r.IDs, keyOf[s.linked][x.Memory.ID])
					context = append(context, x.Memory.Content)
					tokens[i] += approxTokens(x.Memory.Content)
				}
				rk := RecallAt(r, o.K)
				results[i].recall = append(results[i].recall, rk)
				results[i].cats[q.Category] = append(results[i].cats[q.Category], rk)
				results[i].RecallAt5.Mean += RecallAt(r, 5)
				results[i].NDCG.Mean += NDCGAt(r, o.K)
				results[i].MRR.Mean += MRR(r)
				if o.Chat != nil && qaSet[s.name] {
					ok, err := answerAndJudge(ctx, o.Chat, q, context)
					if err != nil {
						return rep, err
					}
					v := 0.0
					if ok {
						v = 1
					}
					results[i].acc = append(results[i].acc, v)
					results[i].accCats[q.Category] = append(results[i].accCats[q.Category], v)
				}
			}
			if o.Progress != nil {
				o.Progress(done, total)
			}
		}
		for _, st := range stores {
			st.Close()
		}
	}
	rep.Questions = done
	for i := range results {
		r := &results[i]
		r.Questions = done
		if done > 0 {
			r.RecallAt5.Mean /= float64(done)
			r.NDCG.Mean /= float64(done)
			r.MRR.Mean /= float64(done)
			r.ContextTokens = tokens[i] / float64(done)
		}
		r.RecallAtK = Bootstrap(r.recall, o.Seed+int64(i))
		r.RecallAt5.N, r.NDCG.N, r.MRR.N = done, done, done
		r.ByCategory = map[string]float64{}
		for c, v := range r.cats {
			r.ByCategory[c] = mean(v)
		}
		if len(r.acc) > 0 {
			e := Bootstrap(r.acc, o.Seed+100+int64(i))
			r.Accuracy = &e
			r.AccuracyByCat = map[string]float64{}
			for c, v := range r.accCats {
				r.AccuracyByCat[c] = mean(v)
			}
		}
		if i > 0 {
			c := Paired(r.recall, results[0].recall, o.Seed+200+int64(i))
			r.VsFirst = &c
		}
	}
	rep.Systems = results
	rep.Elapsed = time.Since(start).Round(time.Second).String()
	return rep, nil
}

const answerSystem = `You answer questions about a long conversation using only the memories given.
Answer in a few words. If the memories don't contain the answer, say "unknown".`

const judgeSystem = `You grade answers. Reply with exactly one word: CORRECT if the answer means the
same as the gold answer (dates in any format, paraphrases and extra detail are fine), or WRONG otherwise.`

func answerAndJudge(ctx context.Context, chat llm.Chat, q BenchQuestion, memories []string) (bool, error) {
	var b strings.Builder
	b.WriteString("Memories:\n")
	for i, m := range memories {
		fmt.Fprintf(&b, "%d. %s\n", i+1, m)
	}
	fmt.Fprintf(&b, "\nQuestion (asked %s): %s\nAnswer:", q.When.Format("2 Jan 2006"), q.Question)
	answer, err := chat.Complete(ctx, answerSystem, b.String())
	if err != nil {
		return false, fmt.Errorf("answering %s: %w", q.ID, err)
	}
	verdict, err := chat.Complete(ctx, judgeSystem,
		fmt.Sprintf("Question: %s\nGold answer: %s\nAnswer to grade: %s\nVerdict:", q.Question, q.Answer, strings.TrimSpace(answer)))
	if err != nil {
		return false, fmt.Errorf("judging %s: %w", q.ID, err)
	}
	v := strings.ToUpper(strings.TrimSpace(verdict))
	return strings.HasPrefix(v, "CORRECT"), nil
}
