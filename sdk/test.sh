#!/usr/bin/env bash
# Runs the TypeScript and Python SDK tests against a live Engram server with a
# temporary database. Usage: sdk/test.sh [ts|py]   (default: both)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'kill "${server:-0}" 2>/dev/null || true; rm -rf "$tmp"' EXIT

export ENGRAM_BIN="$tmp/engram" ENGRAM_DB="$tmp/engram.db" ENGRAM_EMBED=hash ENGRAM_LLM=off
(cd "$root" && go build -o "$ENGRAM_BIN" ./cmd/engram)
"$ENGRAM_BIN" pair sdk-test --read me --read flow/payouts --read flow/backup --write me >/dev/null
export ECP_TOKEN="$("$ENGRAM_BIN" token create sdk-test | sed -n 2p | tr -d ' ')"
port="${ECP_PORT:-$((20000 + RANDOM % 20000))}"
"$ENGRAM_BIN" serve --http "127.0.0.1:$port" 2>"$tmp/server.log" &
server=$!
export ECP_URL="http://127.0.0.1:$port/ecp"
for _ in $(seq 50); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

which="${1:-all}"
if [[ "$which" == all || "$which" == ts ]]; then
  (cd "$root/sdk/typescript" && { [[ -d node_modules ]] || npm install --no-audit --no-fund >/dev/null; } && npm test)
fi
if [[ "$which" == all || "$which" == py ]]; then
  (cd "$root/sdk/python" && python3 -m unittest discover -s tests -v)
fi
