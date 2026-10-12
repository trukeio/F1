#!/usr/bin/env bash
# Smoke test for phase 0: "f1d serve --config <file>" starts, answers
# /healthz, and stops cleanly on SIGTERM.
#
# Usage: scripts/smoke-serve.sh <f1d binary>
set -euo pipefail

bin=$(realpath "$1")
work=$(mktemp -d)
trap 'kill "$pid" 2>/dev/null || true; rm -rf "$work"' EXIT

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
sed "s/^listen = .*/listen = \"127.0.0.1:$port\"/" example.toml > "$work/f1d.toml"

"$bin" serve --config "$work/f1d.toml" 2> "$work/log" &
pid=$!

for _ in $(seq 50); do
	if curl -fsS "http://127.0.0.1:$port/healthz" > /dev/null 2>&1; then
		break
	fi
	sleep 0.1
done
curl -fsS "http://127.0.0.1:$port/healthz"

kill -TERM "$pid"
status=0
wait "$pid" || status=$?
cat "$work/log"
if [ "$status" -ne 0 ]; then
	echo "f1d exited with status $status" >&2
	exit 1
fi
grep -q "server stopped" "$work/log"
test -d "$work/data"
echo "smoke test passed"
