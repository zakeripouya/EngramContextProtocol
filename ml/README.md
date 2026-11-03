# Optional ML workers

Engram's engine is Go and needs no Python. These scripts train models that are
too heavy to write in Go; Engram then scores their output in Go and decides
whether to use them.

| Script | Trains | Then |
| --- | --- | --- |
| `train_xgboost.py` | A gradient-boosted ranker (XGBoost `rank:ndcg`) on the recall log | `engram model train ranker --algo xgboost --file ranker.json` |

```bash
pip install -r ml/requirements.txt
engram eval export data.jsonl            # time-split: the newest 30% is marked test
python3 ml/train_xgboost.py data.jsonl ranker.json
engram model train ranker --algo xgboost --file ranker.json --activate
```

`--activate` only takes effect if the model beats the hand-set formula on the
held-out queries with a 95% bootstrap interval above zero, on at least 30
queries. `engram model off ranker` goes back to the hand-set formula.
