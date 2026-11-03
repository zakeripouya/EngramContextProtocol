package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/eval"
	"github.com/zakeripouya/EngramContextProtocol/internal/llm"
	"github.com/zakeripouya/EngramContextProtocol/internal/model"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// engram eval, bench, model and experiment: measuring Engram like an ML system.

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func ci(e eval.Estimate) string {
	if math.IsNaN(e.Mean) {
		return "      –         "
	}
	return fmt.Sprintf("%.3f [%.2f–%.2f]", e.Mean, e.Lo, e.Hi)
}

func diff(c *eval.Comparison) string {
	if c == nil {
		return ""
	}
	mark := ""
	if c.Significant {
		if c.Diff > 0 {
			mark = " better"
		} else {
			mark = " worse"
		}
	}
	return fmt.Sprintf("%+.3f [%+.2f, %+.2f]%s", c.Diff, c.Lo, c.Hi, mark)
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return store.ParseTime(s)
}

// openSnapshot copies a snapshot so opening it (and migrating it) never changes the original.
func openSnapshot(path string, st *store.Store) (*store.Store, func(), error) {
	dir, err := os.MkdirTemp("", "engram-eval-")
	if err != nil {
		return nil, nil, err
	}
	cp := filepath.Join(dir, "snapshot.db")
	if err := copyFile(path, cp); err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	snap, err := store.Open(cp)
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	snap.SetEmbedder(st.Embedder())
	return snap, func() { snap.Close(); os.RemoveAll(dir) }, nil
}

// snapshotBefore finds the newest snapshot taken before t.
func snapshotBefore(t time.Time) string {
	files, _ := filepath.Glob(filepath.Join(snapshotDir(), "engram-*.db"))
	sort.Strings(files)
	best := ""
	for _, f := range files {
		info, err := os.Stat(f)
		if err == nil && info.ModTime().Before(t) {
			best = f
		}
	}
	return best
}

func runEval(st *store.Store, args []string) error {
	sub := "ranking"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ranking":
		return evalRanking(st, args)
	case "permits":
		return evalPermits(st, args)
	case "runs":
		return evalRuns(st, args)
	case "show":
		return evalShow(st, args)
	case "compare":
		return evalCompare(st, args)
	case "export":
		return evalExport(st, args)
	case "experiment":
		return runExperiment(st, append([]string{"status"}, args...))
	}
	return fmt.Errorf("usage: engram eval [ranking] | permits | runs | show ID | compare A B | export FILE")
}

func evalRanking(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	k := fs.Int("k", 5, "cutoff for Recall@k, precision@k and NDCG@k")
	split := fs.String("split", "", "test on queries from this date on (default: the newest 30%)")
	testFrac := fs.Float64("test", 0.3, "share of the newest labeled queries held out")
	models := fs.String("model", "", "model ids to evaluate, comma-separated (\"active\" for the active ranker)")
	calibrator := fs.String("calibrator", "", "a calibrator model id (or \"active\") to evaluate")
	ablations := fs.Bool("ablations", false, "replay the test queries with each mechanism switched off (needs --snapshot or --replay-current)")
	snapshot := fs.String("snapshot", "", "replay against this snapshot (default with --ablations: the newest one before the test period)")
	current := fs.Bool("replay-current", false, "replay against the current store (learning since the test period leaks into the result)")
	asJSON := fs.Bool("json", false, "print JSON")
	name := fs.String("name", "", "a name for this run in engram eval runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	at, err := parseDate(*split)
	if err != nil {
		return err
	}
	o := eval.RankingOptions{K: *k, Split: at, TestFraction: *testFrac, Calibrator: *calibrator}
	if *models != "" {
		o.Models = splitList(*models)
	} else if sc, _, _ := eval.LoadScorer(st, "active"); sc != nil {
		o.Models = []string{"active"}
	}
	if *ablations || *snapshot != "" || *current {
		o.Configs = append(append(append([]string{"hand"}, store.Baselines...), store.Ablations...), store.Additions...)
		switch {
		case *current:
			o.Replay, o.ReplayLabel = st, "current store: learning since the test period leaks in"
		default:
			path := *snapshot
			if path == "" {
				// The split is only known after loading; estimate it the same way.
				_, labeled, err := eval.LabeledQueries(st)
				if err != nil {
					return err
				}
				_, _, splitAt := eval.SplitByTime(labeled, at, *testFrac)
				if path = snapshotBefore(splitAt); path == "" {
					return fmt.Errorf("no snapshot from before the test period (%s); pass --snapshot FILE or --replay-current", splitAt.Format("2006-01-02"))
				}
			}
			snap, cleanup, err := openSnapshot(path, st)
			if err != nil {
				return err
			}
			defer cleanup()
			o.Replay, o.ReplayLabel = snap, "snapshot "+filepath.Base(path)
		}
	}
	rep, err := eval.EvaluateRanking(st, o)
	if err != nil {
		return err
	}
	id, err := st.RecordEvalRun("ranking", firstNonEmpty(*name, "ranking"), o, rep.Dataset, rep, strings.Join(rep.Warnings, "; "))
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rep)
	}
	printRanking(rep, id)
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func printRanking(rep eval.RankingReport, runID int64) {
	d := rep.Dataset
	fmt.Printf("Ranking evaluation (run %d)\n", runID)
	fmt.Printf("  %d recalls logged, %d with feedback (%d judgments, %d positive)\n", d.Logged, d.Labeled, d.Judgments, d.Positives)
	if d.Test > 0 {
		fmt.Printf("  train %d queries before %s, test %d from then on\n", d.Train, d.Split.Format("2006-01-02 15:04"), d.Test)
	}
	for _, w := range rep.Warnings {
		fmt.Println("  !", w)
	}
	if len(rep.Systems) == 0 {
		return
	}
	fmt.Printf("\n  %-22s %-7s %-20s %-20s %-20s %s\n", "SYSTEM", "MODE", fmt.Sprintf("RECALL@%d", rep.K), "MRR", fmt.Sprintf("NDCG@%d", rep.K), "NDCG vs HAND")
	for _, s := range rep.Systems {
		fmt.Printf("  %-22s %-7s %-20s %-20s %-20s %s\n", truncate(s.Name, 22), s.Mode, ci(s.RecallAtK), ci(s.MRR), ci(s.NDCG), diff(s.VsHand))
	}
	fmt.Println("\n  Brackets are 95% bootstrap intervals. \"better\" or \"worse\" means the whole interval is on one side of zero.")
	if rep.Systems[0].Mode == "rerank" {
		fmt.Println("  rerank = the logged candidates re-ordered by each system (no leakage, but limited to what recall returned).")
	}
	for _, s := range rep.Systems {
		if s.Mode == "replay" && s.Note != "" {
			fmt.Println("  replay =", s.Note)
			break
		}
	}
	if c := rep.Calibration; c != nil {
		fmt.Printf("\n  Confidence calibration on %d held-out judgments: ECE %.3f, Brier %.3f\n", c.Raw.N, c.Raw.ECE, c.Raw.Brier)
		if c.Calibrated != nil {
			fmt.Printf("  with calibrator %s: ECE %.3f, Brier %.3f\n", c.Model, c.Calibrated.ECE, c.Calibrated.Brier)
		}
		fmt.Println("    confidence   said   observed   n")
		for _, b := range c.Raw.Bins {
			if b.Count > 0 {
				fmt.Printf("    %.1f–%.1f      %.2f   %.2f       %d\n", b.Lo, b.Hi, b.Confidence, b.Accuracy, b.Count)
			}
		}
	}
}

func evalPermits(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("eval permits", flag.ContinueOnError)
	testFrac := fs.Float64("test", 0.3, "share of the newest resolved requests held out")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep, _, err := eval.EvaluatePermits(st, *testFrac, nil, 1)
	if err != nil {
		return err
	}
	id, err := st.RecordEvalRun("permits", "permits", map[string]any{"test": *testFrac}, map[string]any{"requests": rep.Requests, "train": rep.Train, "test": rep.Test}, rep, strings.Join(rep.Warnings, "; "))
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rep)
	}
	printPermits(rep, id)
	return nil
}

func printPermits(rep eval.PermitReport, runID int64) {
	fmt.Printf("Permission assist evaluation (run %d)\n", runID)
	fmt.Printf("  %d resolved requests: train %d, test %d", rep.Requests, rep.Train, rep.Test)
	if rep.Test > 0 {
		fmt.Printf(" (from %s), %d approvals in test", rep.Split.Format("2006-01-02"), rep.Positives)
	}
	fmt.Println()
	for _, w := range rep.Warnings {
		fmt.Println("  !", w)
	}
	if len(rep.Systems) == 0 {
		return
	}
	fmt.Printf("\n  %-8s %8s  %-22s %-22s %6s %7s %7s\n", "SYSTEM", "ALLOWED", "PRECISION", "RECALL", "F1", "ROC-AUC", "PR-AUC")
	for _, s := range rep.Systems {
		fmt.Printf("  %-8s %8d  %-22s %-22s %6.3f %7.3f %7.3f\n", s.Name, s.Allowed, ci(s.Precision), ci(s.Recall), s.F1, s.ROCAUC, s.PRAUC)
	}
	if rep.Weights != "" {
		fmt.Println("\n  Model's strongest features:", rep.Weights)
	}
	fmt.Println("  Auto-approval needs precision ≥ 0.98; recall is the share of your approvals it would have saved you.")
}

func evalRuns(st *store.Store, args []string) error {
	kind := ""
	if len(args) > 0 {
		kind = args[0]
	}
	runs, err := st.EvalRuns(kind, 30)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("No evaluations yet: engram eval, engram eval permits, engram bench locomo FILE")
		return nil
	}
	fmt.Printf("%-5s %-20s %-8s %-24s %s\n", "ID", "WHEN", "KIND", "NAME", "HEADLINE")
	for _, r := range runs {
		fmt.Printf("%-5d %-20s %-8s %-24s %s\n", r.ID, r.At, r.Kind, truncate(r.Name, 24), headline(r))
	}
	return nil
}

// headline is the one number that matters for a run.
func headline(r store.EvalRun) string {
	switch r.Kind {
	case "ranking":
		var rep eval.RankingReport
		if json.Unmarshal(r.Metrics, &rep) == nil {
			for _, s := range rep.Systems {
				if s.Name == "served" {
					return fmt.Sprintf("served NDCG@%d %.3f on %d queries", rep.K, s.NDCG.Mean, s.Queries)
				}
			}
			if len(rep.Warnings) > 0 {
				return rep.Warnings[0]
			}
		}
	case "permits":
		var rep eval.PermitReport
		if json.Unmarshal(r.Metrics, &rep) == nil && len(rep.Systems) > 0 {
			return fmt.Sprintf("rule precision %.3f, %d test requests", rep.Systems[0].Precision.Mean, rep.Test)
		}
	case "bench":
		var rep eval.BenchReport
		if json.Unmarshal(r.Metrics, &rep) == nil && len(rep.Systems) > 0 {
			best := rep.Systems[0]
			for _, s := range rep.Systems {
				if s.RecallAtK.Mean > best.RecallAtK.Mean {
					best = s
				}
			}
			return fmt.Sprintf("%s best Recall@%d %.3f (%s), %d questions", rep.Dataset, rep.K, best.RecallAtK.Mean, best.Name, rep.Questions)
		}
	case "model":
		return string(r.Metrics)
	}
	return ""
}

func evalShow(st *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: engram eval show RUN_ID")
	}
	var id int64
	if _, err := fmt.Sscan(args[0], &id); err != nil {
		return err
	}
	r, err := st.EvalRunByID(id)
	if err != nil {
		return err
	}
	switch r.Kind {
	case "ranking":
		var rep eval.RankingReport
		if json.Unmarshal(r.Metrics, &rep) == nil {
			printRanking(rep, r.ID)
			return nil
		}
	case "permits":
		var rep eval.PermitReport
		if json.Unmarshal(r.Metrics, &rep) == nil {
			printPermits(rep, r.ID)
			return nil
		}
	case "bench":
		var rep eval.BenchReport
		if json.Unmarshal(r.Metrics, &rep) == nil {
			printBench(rep, r.ID)
			return nil
		}
	}
	return printJSON(r)
}

// evalCompare lines up two runs system by system.
func evalCompare(st *store.Store, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: engram eval compare RUN_A RUN_B")
	}
	var a, b int64
	if _, err := fmt.Sscan(args[0], &a); err != nil {
		return err
	}
	if _, err := fmt.Sscan(args[1], &b); err != nil {
		return err
	}
	ra, err := st.EvalRunByID(a)
	if err != nil {
		return err
	}
	rb, err := st.EvalRunByID(b)
	if err != nil {
		return err
	}
	if ra.Kind != rb.Kind {
		return fmt.Errorf("run %d is %s and run %d is %s", a, ra.Kind, b, rb.Kind)
	}
	type row struct{ a, b float64 }
	rows := map[string]*row{}
	var order []string
	add := func(name string, v float64, first bool) {
		r := rows[name]
		if r == nil {
			r = &row{math.NaN(), math.NaN()}
			rows[name] = r
			order = append(order, name)
		}
		if first {
			r.a = v
		} else {
			r.b = v
		}
	}
	metric := ""
	for i, run := range []store.EvalRun{ra, rb} {
		switch run.Kind {
		case "ranking":
			var rep eval.RankingReport
			_ = json.Unmarshal(run.Metrics, &rep)
			metric = fmt.Sprintf("NDCG@%d", rep.K)
			for _, s := range rep.Systems {
				add(s.Name, s.NDCG.Mean, i == 0)
			}
		case "bench":
			var rep eval.BenchReport
			_ = json.Unmarshal(run.Metrics, &rep)
			metric = fmt.Sprintf("Recall@%d", rep.K)
			for _, s := range rep.Systems {
				add(s.Name, s.RecallAtK.Mean, i == 0)
			}
		case "permits":
			var rep eval.PermitReport
			_ = json.Unmarshal(run.Metrics, &rep)
			metric = "precision"
			for _, s := range rep.Systems {
				add(s.Name, s.Precision.Mean, i == 0)
			}
		}
	}
	fmt.Printf("%s, run %d vs run %d\n\n  %-24s %8s %8s %8s\n", metric, a, b, "SYSTEM", fmt.Sprint(a), fmt.Sprint(b), "CHANGE")
	for _, n := range order {
		r := rows[n]
		fmt.Printf("  %-24s %8.3f %8.3f %+8.3f\n", truncate(n, 24), r.a, r.b, r.b-r.a)
	}
	return nil
}

func evalExport(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("eval export", flag.ContinueOnError)
	testFrac := fs.Float64("test", 0.3, "share of the newest labeled queries marked as test")
	if err := fs.Parse(reorderFlags(args, "test")); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: engram eval export FILE.jsonl [--test 0.3]")
	}
	_, labeled, err := eval.LabeledQueries(st)
	if err != nil {
		return err
	}
	train, test, at := eval.SplitByTime(labeled, time.Time{}, *testFrac)
	f, err := os.Create(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := eval.ExportRows(f, train, test)
	if err != nil {
		return err
	}
	fmt.Printf("Wrote %d rows (%d train and %d test queries, split at %s) to %s\n", n, len(train), len(test), at.Format("2006-01-02"), fs.Arg(0))
	fmt.Println("Train a gradient-boosted ranker with: python3 ml/train_xgboost.py", fs.Arg(0), "ranker.json")
	fmt.Println("Then evaluate and store it: engram model train ranker --algo xgboost --file ranker.json")
	return nil
}

// ---- benchmarks ----

func runBench(st *store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: engram bench locomo FILE | longmemeval FILE [--k 10] [--configs a,b] [--link-turns] [--qa] [--max N] [--json]")
	}
	dataset, args := args[0], args[1:]
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	k := fs.Int("k", 10, "memories recalled per question")
	configs := fs.String("configs", "bm25,embeddings,hybrid,full", "configurations to compare (first is the baseline for the paired test)")
	ablations := fs.Bool("ablations", false, "also run every ablation and addition")
	link := fs.Bool("link-turns", false, "also test linking consecutive turns as episodes")
	qa := fs.Bool("qa", false, "answer each question with the sleep-cycle chat model and grade it with an LLM judge")
	maxQ := fs.Int("max", 0, "stop after this many questions")
	asJSON := fs.Bool("json", false, "print JSON")
	adversarial := fs.Bool("adversarial", false, "LoCoMo: include adversarial questions (no answer in the conversation)")
	part := fs.String("part", "all", "tune (even-numbered conversations), report (odd-numbered) or all: tune on one half, report the other")
	if err := fs.Parse(reorderFlags(args, "k", "configs", "max", "part")); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: engram bench %s FILE", dataset)
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	var samples []eval.BenchSample
	switch dataset {
	case "locomo":
		samples, err = eval.LoadLoCoMo(f, *adversarial)
	case "longmemeval":
		samples, err = eval.LoadLongMemEval(f)
	default:
		return fmt.Errorf("unknown benchmark %q (locomo or longmemeval)", dataset)
	}
	if err != nil {
		return err
	}
	samples, err = eval.Part(samples, *part)
	if err != nil {
		return err
	}
	if *part != "all" {
		dataset += " (" + *part + ")"
	}
	o := eval.BenchOptions{K: *k, Configs: splitList(*configs), LinkTurns: *link, MaxQuestions: *maxQ, Embedder: st.Embedder()}
	if *ablations {
		o.Configs = append(append(o.Configs, store.Ablations...), store.Additions...)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *qa {
		chat, note := llm.New(ctx, llm.FromEnv())
		if chat == nil {
			return fmt.Errorf("--qa needs a chat model: %s", note)
		}
		o.Chat = chat
	}
	tty := isTerminal(os.Stderr)
	o.Progress = func(done, total int) {
		if tty && !*asJSON && (done%10 == 0 || done == total) {
			fmt.Fprintf(os.Stderr, "\r\033[K%s: %d/%d questions", dataset, done, total)
		}
	}
	rep, err := eval.RunBench(ctx, dataset, samples, o)
	if tty && !*asJSON {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if err != nil {
		return err
	}
	id, err := st.RecordEvalRun("bench", dataset, o.Configs, map[string]any{"file": filepath.Base(fs.Arg(0)), "samples": rep.Samples,
		"questions": rep.Questions, "embedder": rep.Embedder}, rep, strings.Join(rep.Warnings, "; "))
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rep)
	}
	printBench(rep, id)
	return nil
}

func printBench(rep eval.BenchReport, runID int64) {
	fmt.Printf("%s (run %d): %d conversations, %d turns, %d questions, k=%d, embeddings: %s, %s\n", rep.Dataset, runID, rep.Samples,
		rep.Turns, rep.Questions, rep.K, rep.Embedder, rep.Elapsed)
	for _, w := range rep.Warnings {
		fmt.Println("  !", w)
	}
	fmt.Printf("\n  %-16s %-20s %9s %8s %8s %8s  %s\n", "SYSTEM", fmt.Sprintf("RECALL@%d", rep.K), "RECALL@5", "NDCG", "MRR", "TOKENS", "VS FIRST")
	for _, s := range rep.Systems {
		fmt.Printf("  %-16s %-20s %9.3f %8.3f %8.3f %8.0f  %s\n", truncate(s.Name, 16), ci(s.RecallAtK), s.RecallAt5.Mean, s.NDCG.Mean,
			s.MRR.Mean, s.ContextTokens, diff(s.VsFirst))
	}
	var cats []string
	for c := range rep.Systems[0].ByCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	if len(cats) > 1 {
		fmt.Printf("\n  Recall@%d by question type\n  %-16s", rep.K, "")
		for _, c := range cats {
			fmt.Printf(" %12s", truncate(c, 12))
		}
		fmt.Println()
		for _, s := range rep.Systems {
			fmt.Printf("  %-16s", truncate(s.Name, 16))
			for _, c := range cats {
				fmt.Printf(" %12.3f", s.ByCategory[c])
			}
			fmt.Println()
		}
	}
	judged := false
	for _, s := range rep.Systems {
		if s.Accuracy != nil {
			if !judged {
				fmt.Printf("\n  Answer accuracy (%s)\n", rep.Judge)
				judged = true
			}
			fmt.Printf("  %-16s %s\n", s.Name, ci(*s.Accuracy))
		}
	}
}

// ---- models ----

func runModel(st *store.Store, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		ms, err := st.Models("")
		if err != nil {
			return err
		}
		if len(ms) == 0 {
			fmt.Println("No models yet. Recall uses the hand-set formula. Train one with: engram model train ranker | calibrator | permit")
			return nil
		}
		fmt.Printf("%-24s %-11s %-16s %-9s %-20s %s\n", "ID", "KIND", "ALGORITHM", "STATUS", "CREATED", "HELD-OUT")
		for _, m := range ms {
			fmt.Printf("%-24s %-11s %-16s %-9s %-20s %s\n", m.ID, m.Kind, m.Algo, m.Status, m.CreatedAt, truncate(string(m.Metrics), 60))
		}
		return nil
	case "train":
		if len(args) < 2 {
			return fmt.Errorf("usage: engram model train ranker | calibrator | permit [--activate]")
		}
		return trainModel(st, args[1], args[2:])
	case "activate":
		if len(args) != 2 {
			return fmt.Errorf("usage: engram model activate ID")
		}
		if err := st.ActivateModel(args[1]); err != nil {
			return err
		}
		fmt.Println("Activated", args[1], "(engram model off KIND goes back to the hand-set rules)")
		return nil
	case "off":
		if len(args) != 2 {
			return fmt.Errorf("usage: engram model off ranker | calibrator | permit")
		}
		n, err := st.DeactivateModels(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Retired %d active %s model(s); the hand-set rules are back.\n", n, args[1])
		return nil
	}
	return fmt.Errorf("usage: engram model list | train KIND | activate ID | off KIND")
}

func trainModel(st *store.Store, kind string, args []string) error {
	fs := flag.NewFlagSet("model train", flag.ContinueOnError)
	testFrac := fs.Float64("test", 0.3, "share of the newest data held out")
	algo := fs.String("algo", "pairwise", "ranker algorithm: pairwise (built in) or xgboost (with --file)")
	file := fs.String("file", "", "an XGBoost JSON model trained by ml/train_xgboost.py")
	activate := fs.Bool("activate", false, "activate it if it passes the held-out gate")
	force := fs.Bool("force", false, "with --activate: activate even if it fails the gate")
	k := fs.Int("k", 5, "ranking cutoff")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var id string
	var gate eval.GateResult
	switch kind {
	case "ranker":
		_, labeled, err := eval.LabeledQueries(st)
		if err != nil {
			return err
		}
		train, test, at := eval.SplitByTime(labeled, time.Time{}, *testFrac)
		trainInfo := map[string]any{"trainQueries": len(train), "testQueries": len(test), "split": at}
		rec := store.ModelRecord{Kind: model.KindRanker, Features: model.Names}
		switch *algo {
		case "pairwise":
			groups := eval.Groups(train)
			m, err := model.TrainPairwise(model.Names, groups, model.TrainOptions{})
			if err != nil {
				return fmt.Errorf("training on %d queries: %w", len(train), err)
			}
			rec.Algo = model.AlgoPairwise
			rec.Params, _ = json.Marshal(m)
			trainInfo["explain"] = m.Explain(6)
		case "xgboost":
			if *file == "" {
				return fmt.Errorf("--algo xgboost needs --file MODEL.json (see ml/train_xgboost.py)")
			}
			b, err := os.ReadFile(*file)
			if err != nil {
				return err
			}
			xm, err := model.ParseXGBoost(b)
			if err != nil {
				return err
			}
			rec.Algo, rec.Params = model.AlgoXGBoost, b
			trainInfo["trees"] = xm.Trees()
		default:
			return fmt.Errorf("unknown ranker algorithm %q (pairwise or xgboost)", *algo)
		}
		rec.Train, _ = json.Marshal(trainInfo)
		if id, err = st.SaveModel(rec); err != nil {
			return err
		}
		rep, err := eval.EvaluateRanking(st, eval.RankingOptions{K: *k, Split: at, Models: []string{id}})
		if err != nil {
			return err
		}
		gate = eval.RankerGate(rep, "model:"+id)
		_ = st.SetModelMetrics(id, map[string]any{"gate": gate, "systems": rep.Systems})
		runID, _ := st.RecordEvalRun("ranking", "train "+id, map[string]any{"model": id, "algo": rec.Algo}, rep.Dataset, rep, gate.Reason)
		printRanking(rep, runID)
		if e, ok := trainInfo["explain"]; ok {
			fmt.Println("\n  Strongest features:", e)
		}
	case "calibrator":
		_, labeled, err := eval.LabeledQueries(st)
		if err != nil {
			return err
		}
		train, test, at := eval.SplitByTime(labeled, time.Time{}, *testFrac)
		p, y := eval.CalibrationPairs(train)
		m, err := model.FitIsotonic(p, y)
		if err != nil {
			return fmt.Errorf("fitting on %d judgments: %w", len(p), err)
		}
		params, _ := json.Marshal(m)
		trainInfo, _ := json.Marshal(map[string]any{"trainJudgments": len(p), "testQueries": len(test), "split": at})
		if id, err = st.SaveModel(store.ModelRecord{Kind: model.KindCalibrator, Algo: model.AlgoIsotonic, Features: []string{"conf"},
			Params: params, Train: trainInfo}); err != nil {
			return err
		}
		rep, err := eval.EvaluateRanking(st, eval.RankingOptions{K: *k, Split: at, Calibrator: id})
		if err != nil {
			return err
		}
		gate = eval.CalibratorGate(rep.Calibration)
		_ = st.SetModelMetrics(id, map[string]any{"gate": gate, "calibration": rep.Calibration})
		if c := rep.Calibration; c != nil {
			fmt.Printf("Calibration on %d held-out judgments: ECE %.3f → %.3f, Brier %.3f → %.3f\n", c.Raw.N, c.Raw.ECE,
				valueOr(c.Calibrated, func(x *eval.Calibration) float64 { return x.ECE }), c.Raw.Brier,
				valueOr(c.Calibrated, func(x *eval.Calibration) float64 { return x.Brier }))
		}
	case "permit":
		d, err := eval.BuildPermitDataset(st)
		if err != nil {
			return err
		}
		train, _, _ := d.Split(*testFrac)
		m, err := eval.TrainPermitModel(train)
		if err != nil {
			return fmt.Errorf("training on %d requests: %w", len(train.Requests), err)
		}
		rep, _, err := eval.EvaluatePermits(st, *testFrac, m, 1)
		if err != nil {
			return err
		}
		params, _ := json.Marshal(m)
		trainInfo, _ := json.Marshal(map[string]any{"trainRequests": rep.Train, "testRequests": rep.Test})
		if id, err = st.SaveModel(store.ModelRecord{Kind: model.KindPermit, Algo: model.AlgoLogReg, Features: m.Features,
			Params: params, Train: trainInfo}); err != nil {
			return err
		}
		gate = eval.PermitGate(rep)
		_ = st.SetModelMetrics(id, map[string]any{"gate": gate, "systems": rep.Systems})
		runID, _ := st.RecordEvalRun("permits", "train "+id, map[string]any{"model": id}, map[string]any{"train": rep.Train, "test": rep.Test}, rep, gate.Reason)
		printPermits(rep, runID)
	default:
		return fmt.Errorf("unknown model kind %q (ranker, calibrator or permit)", kind)
	}
	fmt.Printf("\nStored %s as a candidate. Gate: %v (%s)\n", id, passWord(gate.Pass), gate.Reason)
	if *activate {
		if !gate.Pass && !*force {
			fmt.Println("Not activated: it did not pass the gate. The hand-set rules stay in charge (--force overrides).")
			return nil
		}
		if err := st.ActivateModel(id); err != nil {
			return err
		}
		fmt.Println("Activated. engram model off", kind, "goes back to the hand-set rules.")
	}
	return nil
}

func valueOr(c *eval.Calibration, f func(*eval.Calibration) float64) float64 {
	if c == nil {
		return math.NaN()
	}
	return f(c)
}

func passWord(b bool) string {
	if b {
		return "pass"
	}
	return "fail"
}

// ---- online experiments ----

func runExperiment(st *store.Store, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "start":
		fs := flag.NewFlagSet("experiment start", flag.ContinueOnError)
		variant := fs.String("variant", "", "a named configuration (no-spreading, with-about, ...) or a JSON RecallConfig")
		share := fs.Float64("share", 0.2, "share of recalls that get the variant")
		if err := fs.Parse(reorderFlags(args[1:], "variant", "share")); err != nil {
			return err
		}
		if fs.NArg() != 1 || *variant == "" {
			return fmt.Errorf("usage: engram experiment start NAME --variant CONFIG [--share 0.2]")
		}
		var cfg store.RecallConfig
		if strings.HasPrefix(strings.TrimSpace(*variant), "{") {
			if err := json.Unmarshal([]byte(*variant), &cfg); err != nil {
				return fmt.Errorf("--variant JSON: %w", err)
			}
		} else {
			var err error
			if cfg, err = store.NamedConfig(*variant); err != nil {
				return err
			}
			cfg.NoModels = false // the variant changes one thing; active models stay on in both arms
		}
		if cfg.Ranker == "" && !cfg.NoModels {
			cfg.Ranker, cfg.Calibrator = "active", "active"
		}
		if err := st.StartExperiment(store.Experiment{Name: fs.Arg(0), Variant: cfg, Share: *share}); err != nil {
			return err
		}
		fmt.Printf("Experiment %s started: %.0f%% of recalls get %s. engram experiment status shows how it is going.\n", fs.Arg(0), *share*100, *variant)
		return nil
	case "stop":
		e, err := st.CurrentExperiment()
		if err != nil {
			return err
		}
		if err := st.StopExperiment(); err != nil {
			return err
		}
		if e != nil {
			fmt.Printf("Experiment %s stopped; its results stay available: engram experiment status %s\n", e.Name, e.Name)
		}
		return nil
	case "status":
		fs := flag.NewFlagSet("experiment status", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(reorderFlags(args[1:], "-")); err != nil {
			return err
		}
		name := fs.Arg(0)
		e, err := st.CurrentExperiment()
		if err != nil {
			return err
		}
		if name == "" {
			if e == nil {
				fmt.Println("No experiment is running. Start one: engram experiment start NAME --variant no-spreading --share 0.2")
				return nil
			}
			name = e.Name
		}
		rep, err := eval.AnalyzeExperiment(st, name, 1)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(rep)
		}
		state := "stopped"
		if e != nil && e.Name == name {
			state = fmt.Sprintf("running since %s, %.0f%% to the variant", e.Started, e.Share*100)
		}
		fmt.Printf("Experiment %s (%s)\n\n  %-8s %8s %8s  %-22s %s\n", name, state, "ARM", "RECALLS", "JUDGED", "ACCEPT RATE", "MRR")
		for _, a := range rep.Arms {
			fmt.Printf("  %-8s %8d %8d  %-22s %s\n", a.Arm, a.Recalls, a.Judged, ci(a.AcceptRate), ci(a.MRR))
		}
		orNone := func(c *eval.Comparison) string {
			if c == nil {
				return "not enough data"
			}
			return diff(c)
		}
		fmt.Printf("\n  variant − control: accept rate %s, MRR %s\n", orNone(rep.AcceptDiff), orNone(rep.MRRDiff))
		for _, w := range rep.Warnings {
			fmt.Println("  !", w)
		}
		return nil
	}
	return fmt.Errorf("usage: engram experiment start NAME --variant CONFIG [--share 0.2] | status [NAME] | stop")
}
