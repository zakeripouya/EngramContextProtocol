# Evaluating Engram

How Engram is measured, the commands that do it, and the results so far. The
rule throughout: a mechanism or a model is only trusted when it beats a simple
baseline on data it has never seen, by more than noise.

## What gets logged

- **Every recall** a client makes is logged (`recall_log`) with the query, the
  configuration that served it, and each result's rank, score, confidence and
  features: keyword and meaning scores, prior, strength, hops, pathway weight,
  kind, age, use count, the hand-set score and confidence.
- **Feedback** (`accepted` or `overridden`, plus `succeeded` or `failed`) is
  linked to the recall it judged: the client's latest recall in the last two
  hours that returned the memory. Grades: 2 accepted and succeeded, 1 accepted,
  0 overridden or failed. Shown and not judged counts as 0 for training.
- **Permission prompts** are logged with the rule's prediction and what you did
  (milestone M9).

Unjudged recall-log entries older than 180 days are removed by the sleep cycle.

## Offline evaluation: `engram eval`

```bash
engram eval                       # ranking on held-out feedback
engram eval --ablations           # plus every mechanism switched off, replayed on a snapshot
engram eval permits               # the permission assist
engram eval runs | show ID | compare A B
```

- **Time split.** Labeled queries are ordered by time; the newest 30% (or
  everything after `--split DATE`) is the test set. Random splits would leak the
  future into training.
- **Metrics.** Recall@k, precision@k, MRR and NDCG@k (gains 2^grade − 1) per
  query; calibration as ECE over 10 bins, the Brier score and a reliability
  table; for permissions, precision, recall, F1, ROC-AUC and PR-AUC.
- **Intervals.** Every number is a mean with a 95% bootstrap interval over
  queries (2,000 resamples, fixed seed). Systems are compared with a paired
  bootstrap on the same queries; "better" means the whole interval of the
  difference is above zero.
- **Rerank vs replay.** *Rerank* re-orders the logged candidates by each
  system's score, using the features as they were at the time: no leakage, but
  limited to what recall returned. *Replay* re-runs recall with a configuration
  against a snapshot taken before the test period (the default), so mechanisms
  that change the candidate set (spreading, inhibition) can be measured too.
  `--replay-current` uses the live store and says that learning leaks in.
- **Baselines.** `bm25` (keywords only), `embeddings` (meaning only), `hybrid`
  (both, no prior or pathways), `prior` (strength and use only), and `hand` (the
  hand-set formula).
- **Ablations.** `no-keyword`, `no-semantic`, `no-prior`, `no-spreading`,
  `no-hebbian` (spread without learned `coactive` pathways), `no-inhibition`,
  `no-override`, and the candidate addition `with-about` (spread along `about`
  edges too).
- Every run is recorded in `eval_runs` with its configuration, dataset and
  metrics.

## Learned models: `engram model`

```bash
engram model train ranker [--activate]               # pairwise linear ranker (built in)
engram eval export data.jsonl && python3 ml/train_xgboost.py data.jsonl ranker.json
engram model train ranker --algo xgboost --file ranker.json [--activate]
engram model train calibrator [--activate]           # isotonic calibration of confidence
engram model train permit [--activate]               # logistic regression for permission prompts
engram model list | activate ID | off KIND
```

Models are trained on the older data and judged on the newer data. Activation is
gated:

| Model | Passes when |
| --- | --- |
| Ranker | NDCG@k gain over the hand-set formula with the whole 95% interval above zero, on at least 30 held-out queries |
| Calibrator | lower ECE and no worse Brier on at least 50 held-out judgments |
| Permission | precision ≥ 0.98 (lower bound ≥ 0.95), at least the rule's precision, and more auto-approvals than the rule, on at least 50 held-out requests; it still only allows the eligible tier with 8 precedents |

XGBoost models are trained in Python and scored in Go from XGBoost's JSON format
(numeric splits, comparisons in float32 as XGBoost does), so no Python runs at
recall time. A test checks the Go scores against XGBoost's own predictions.

## Online evaluation: `engram experiment`

```bash
engram experiment start spread-off --variant no-spreading --share 0.2
engram experiment status
engram experiment stop
```

A share of real recalls is served by the variant, assigned at random by recall
id, and logged with its arm. Status compares the arms' accept rate (recalls with
at least one accepted result) and MRR with an unpaired bootstrap.

## Public benchmarks: `engram bench`

```bash
curl -LO https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json
engram bench locomo locomo10.json [--ablations] [--link-turns] [--qa]
engram bench longmemeval longmemeval_s.json [--qa]
```

Each conversation is stored as episodes in a fresh in-memory store, and every
configuration recalls for every question. Retrieval is scored against the
dataset's evidence turns. With `--qa`, the configured chat model answers from
the recalled memories and an LLM judge grades the answer against the gold one.
LoCoMo's adversarial questions (category 5) and LongMemEval's abstention
questions are skipped by default because they have no evidence to retrieve.
Models trained on your data are never used on public benchmarks.

### First results: LoCoMo, hashing embedder

Engram 0.13.0, all 10 conversations (5,882 turns), 1,536 questions, k = 10, no
embedding server (the offline hashing embedder), no chat model. Run in about 4.5
minutes on 2 CPU cores.

| System | Recall@10 [95% CI] | Recall@5 | NDCG@10 | MRR | Tokens |
| --- | --- | --- | --- | --- | --- |
| bm25 | 0.560 [0.54–0.58] | 0.487 | 0.428 | 0.408 | 405 |
| embeddings (hashing) | 0.095 [0.08–0.11] | 0.091 | 0.086 | 0.090 | 33 |
| hybrid | 0.558 [0.53–0.58] | 0.481 | 0.415 | 0.391 | 402 |
| full Engram | 0.558 [0.53–0.58] | 0.481 | 0.415 | 0.391 | 402 |
| full + turns linked as episodes | 0.557 [0.53–0.58] | 0.481 | 0.415 | 0.390 | 402 |

By question type, Recall@10 for full Engram: single-hop 0.655, temporal 0.623,
multi-hop 0.282, open-domain 0.285.

What this says:

- On a cold store, Engram is no better than plain keyword search. The prior and
  Hebbian learning have nothing to work with on a benchmark nobody has used,
  and every ablation of them scores the same.
- Spreading activation does not help here: what it reaches is always ranked
  below the direct matches, so it never enters the top 10. Multi-hop questions,
  the case spreading is meant for, are the weakest category. This is a design
  problem to fix, tracked in the backlog.
- The hashing embedder matches words, not meaning, so "embeddings" is close to
  useless. These numbers must be rerun with a real embedding model, and with a
  chat model for answer accuracy, before they are compared with published
  results for Mem0, Zep or others.

### Tuning attempts: fusion and spreading (0.13.1)

To make spreading reach the top 10 and to fuse keyword and meaning better, four
switchable changes were added, tuned on the even-numbered LoCoMo conversations
(`--part tune`, 786 questions) and checked on the odd-numbered ones
(`--part report`, 750 questions). Same setup: hashing embedder, k = 10.

| Change | Name | Recall@10 change vs full (tune) | Report half |
| --- | --- | --- | --- |
| Reciprocal rank fusion of keyword and meaning ranks | `full+rrf` | −0.017 [−0.03, −0.01] | −0.018 |
| Spreading adds activation from every source (ACT-R style) | `full+additive` | −0.292 [−0.33, −0.26] | not run |
| Pathways only boost direct matches, by at most 10% | `full+boost10` | −0.128 [−0.15, −0.10] | −0.098 |
| One of the 10 places reserved for a pathway result | `full+slots1` | −0.013 [−0.02, −0.01] | −0.011 |
| Two places reserved | `full+slots2` | −0.020 [−0.03, −0.01] | not run |
| Additive spreading over turns linked as episodes | `full+additive@episodes` | −0.193 [−0.22, −0.16] | not run |

Every change was significantly worse, so none is on by default; they stay
available as modifiers (`+rrf`, `+additive`, `+boostN`, `+slotsN`, `@episodes`)
to retest. Two reasons, both about this setup rather than final verdicts:

- With the hashing embedder, the "meaning" ranking is close to noise, so giving
  it an equal vote (RRF) hurts. RRF needs a rerun with a real embedding model.
- The pathway graph here is built by the same hashing embedder, so `similar`
  edges link turns that share words. Being well connected to the top hits is
  then not evidence of relevance, and anything that rewards connectivity
  reorders results by the wrong thing. Spreading needs meaning-based edges or
  pathways learned from use, which a cold benchmark does not have. The current
  design, where pathway results rank below direct matches, is the least harmful
  choice on this data.

Where Engram is meant to win is learning from use: a store that has seen your
feedback, measured with `engram eval` on your own queries. On a simulated user
(the tests in `internal/eval`), a ranker trained on 21 days of feedback beat the
hand-set formula on the following 9 days (NDCG@5 +0.36, interval +0.34 to +0.37),
and isotonic calibration cut ECE from 0.53 to 0.06. The simulated world is built
so that a learnable pattern exists, so those numbers show the machinery works,
not that it will help you. Your own `engram eval` after a few weeks of use is
the result that counts.
