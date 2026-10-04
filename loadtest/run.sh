#!/usr/bin/env bash
# usage: ./loadtest/run.sh <rate> <label>     e.g. ./loadtest/run.sh 2500 baseline
set -euo pipefail
RATE=$1; LABEL=$2
grep -q constant-arrival-rate loadtest/k6.js || { echo "ERROR: k6.js is not the step test"; exit 1; }

mkdir -p results
docker compose --progress plain up -d --build > results/build.log 2>&1 \
  || { echo "ERROR: build failed, see results/build.log"; exit 1; }
sleep 5                                                      # always test the code you just saved
OUT="results/${LABEL}-${RATE}"

docker run --rm -i --network pulse_default -e BASE_URL=http://lb:8000 -e RATE="$RATE" \
  grafana/k6 run --quiet - < loadtest/k6.js > "$OUT.k6.txt" 2>&1 &
K6=$!
sleep 45                                                     # 30 s into the measured phase
docker stats --no-stream --format "table {{.Name}}\t{{.CPUPerc}}" > "$OUT.stats.txt"
wait $K6 || true                                             # k6 exits non-zero when a threshold fails

echo "===== $LABEL @ $RATE req/s ====="
cat "$OUT.stats.txt"
grep -E "scenario:measure|http_reqs|dropped_iterations" "$OUT.k6.txt"