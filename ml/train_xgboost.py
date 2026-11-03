#!/usr/bin/env python3
"""Train a gradient-boosted ranker on Engram's recall log.

    engram eval export data.jsonl
    python3 ml/train_xgboost.py data.jsonl ranker.json
    engram model train ranker --algo xgboost --file ranker.json [--activate]

The first line of data.jsonl lists the feature names; every other line is one
result a recall showed: {"qid", "split", "grade", "features"}. Grades are 2
(accepted and it worked), 1 (accepted), 0 (overridden, failed, or shown and
not chosen). The model is trained on the "train" split only. Engram scores it
in Go (no Python at recall time) and only activates it if it beats the
hand-set formula on the held-out queries.
"""
import argparse
import json
import sys

import numpy as np
import xgboost as xgb


def load(path):
    names, rows = None, []
    with open(path) as f:
        for line in f:
            obj = json.loads(line)
            if "featureNames" in obj:
                names = obj["featureNames"]
                continue
            rows.append(obj)
    if names is None:
        sys.exit("not an Engram export: the first line should list featureNames")
    return names, rows


def matrix(rows):
    rows = sorted(rows, key=lambda r: (r["at"], r["qid"]))  # XGBoost wants each query's rows together
    X = np.array([r["features"] for r in rows], dtype=float)
    y = np.array([r["grade"] for r in rows], dtype=float)
    _, qid = np.unique([r["qid"] for r in rows], return_inverse=True)
    order = np.argsort(qid, kind="stable")
    return X[order], y[order], qid[order]


def ndcg_at(scores, y, qid, k):
    total, n = 0.0, 0
    for q in np.unique(qid):
        m = qid == q
        if y[m].max() <= 0:
            continue
        order = np.argsort(-scores[m], kind="stable")
        gains = (2 ** y[m][order] - 1)[:k]
        dcg = float(np.sum(gains / np.log2(np.arange(2, len(gains) + 2))))
        ideal = np.sort(2 ** y[m] - 1)[::-1][:k]
        idcg = float(np.sum(ideal / np.log2(np.arange(2, len(ideal) + 2))))
        total, n = total + dcg / idcg, n + 1
    return total / max(n, 1)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("data")
    ap.add_argument("out")
    ap.add_argument("--trees", type=int, default=200)
    ap.add_argument("--depth", type=int, default=4)
    ap.add_argument("--eta", type=float, default=0.05)
    ap.add_argument("--k", type=int, default=5)
    a = ap.parse_args()

    names, rows = load(a.data)
    train = [r for r in rows if r["split"] == "train"]
    test = [r for r in rows if r["split"] == "test"]
    if len({r["qid"] for r in train}) < 20:
        sys.exit(f"only {len({r['qid'] for r in train})} training queries; collect more feedback first")
    Xtr, ytr, qtr = matrix(train)
    model = xgb.XGBRanker(objective="rank:ndcg", n_estimators=a.trees, max_depth=a.depth, learning_rate=a.eta,
                          subsample=0.9, colsample_bytree=0.9, min_child_weight=2, random_state=1)
    model.fit(Xtr, ytr, qid=qtr)
    model.get_booster().feature_names = names
    model.save_model(a.out)
    print(f"trained {a.trees} trees on {len(np.unique(qtr))} queries ({len(train)} rows); saved {a.out}")
    if test:
        Xte, yte, qte = matrix(test)
        hand = Xte[:, names.index("hand")]
        print(f"held-out NDCG@{a.k}: model {ndcg_at(model.predict(Xte), yte, qte, a.k):.3f}, "
              f"hand-set formula {ndcg_at(hand, yte, qte, a.k):.3f}  (engram model train decides, with intervals)")


if __name__ == "__main__":
    main()
