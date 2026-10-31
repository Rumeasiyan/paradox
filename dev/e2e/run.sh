#!/bin/sh
# Go-live rehearsal (backlog story 37): runs PARADOX from the local image
# against a real Sablier, a real Uptime Kuma and PostgreSQL behind
# PgBouncer (session mode, pool of 2), with paradox testsystem as the
# sleeping system. Keys and settings go to $E2E_DIR, outside the
# repository. Needs Docker, Go, and Python 3 with uptime-kuma-api for the
# monitors (pip install uptime-kuma-api; without it the monitors are
# skipped and failures go to the log).
#
#   dev/e2e/run.sh          start and run the checks
#   dev/e2e/run.sh down     remove the stack
set -eu
cd "$(dirname "$0")"
REPO=$(cd ../.. && pwd)
export E2E_DIR="${E2E_DIR:-$HOME/.cache/paradox-e2e}"   # must be a folder Docker can mount
dc() { docker compose "$@"; }

if [ "${1:-}" = down ]; then dc down -v; exit 0; fi

mkdir -p "$E2E_DIR/systems/jobtest" "$E2E_DIR/keys"
(cd "$REPO" && docker build -q --build-arg VERSION="$(cat VERSION)" -t paradox:local . >/dev/null)
paradox() { (cd "$REPO" && go run ./cmd/paradox "$@"); }
[ -f "$E2E_DIR/keys/paradox.key" ] || paradox keygen -out "$E2E_DIR/keys/paradox.key" >/dev/null
[ -f "$E2E_DIR/keys/jobtest.key" ] || paradox keygen -out "$E2E_DIR/keys/jobtest.key" > "$E2E_DIR/systems/jobtest/system.pub"
chmod 644 "$E2E_DIR"/keys/*.key   # the containers run as another user; throwaway keys only
cat > "$E2E_DIR/systems/jobtest/schedules.yaml" <<YAML
system: jobtest
wake: sablier
jobs:
  - name: ping
    every: "0 7 * * *"
  - name: fail
    retries: 1
YAML
printf 'TESTSYSTEM_PARADOX_PUBLIC_KEY=%s\nTESTSYSTEM_FAIL_JOBS=fail\n' "$(paradox pubkey -in "$E2E_DIR/keys/paradox.key")" > "$E2E_DIR/jobtest.env"

: > "$E2E_DIR/paradox.env"
dc up -d postgres pgbouncer sablier kuma jobtest
sleep 3
dc stop jobtest

HB=""; FL=""
if python3 -c "import uptime_kuma_api" 2>/dev/null; then
  [ -f "$E2E_DIR/kuma.json" ] || python3 kuma_setup.py "$E2E_DIR/kuma.json"
  HB=$(python3 -c "import json;d=json.load(open('$E2E_DIR/kuma.json'));print('http://kuma:3001/api/push/'+d['paradox-heartbeat']['token'])")
  FL=$(python3 -c "import json;d=json.load(open('$E2E_DIR/kuma.json'));print('http://kuma:3001/api/push/'+d['paradox-failures']['token'])")
fi
umask 077
cat > "$E2E_DIR/paradox.env" <<ENV
PARADOX_DATABASE_URL=postgres://paradox:paradox@pgbouncer:5432/paradox
PARADOX_PRIVATE_KEY=$(cat "$E2E_DIR/keys/paradox.key")
PARADOX_SYSTEMS_DIR=/systems
PARADOX_SABLIER_URL=http://sablier:10000
PARADOX_HEARTBEAT_URL=$HB
PARADOX_FAILURE_URL=$FL
PARADOX_RETRY_DELAYS=5s
PARADOX_KEEP_AWAKE_EVERY=20s
PARADOX_WAKE_TIMEOUT=1m
PARADOX_HEARTBEAT_EVERY=15s
GOMEMLIMIT=200MiB
ENV
dc up -d --force-recreate paradox
for i in $(seq 1 45); do
  [ "$(docker inspect -f '{{.State.Health.Status}}' paradox-e2e-paradox-1 2>/dev/null)" = healthy ] && break
  sleep 1
done

echo "jobtest before:  $(docker ps -a --filter name=paradox-e2e-jobtest --format '{{.Status}}')"
dc exec -T paradox /paradox enqueue -url http://127.0.0.1:8080 -system jobtest -key /keys/jobtest.key -job ping >/dev/null
dc exec -T paradox /paradox enqueue -url http://127.0.0.1:8080 -system jobtest -key /keys/jobtest.key -job fail >/dev/null
dc exec -T postgres psql -U admin -d paradox -qc "UPDATE job SET next_due_at = now() - interval '1 second' WHERE name = 'ping'"
sleep 25
echo "jobtest after:   $(docker ps -a --filter name=paradox-e2e-jobtest --format '{{.Status}}')"
dc exec -T paradox /paradox runs
echo "Expect: a queued ping and a scheduled ping succeeded, fail failed after 2 attempts;"
echo "the failure monitor got DOWN then UP; jobtest stops again about 80 s after its last job."
