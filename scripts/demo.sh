#!/usr/bin/env bash
# Runs a demo: an F1 server and the web viewer (f1web), with Git
# repositories pushed into Git namespaces. It prints the viewer's URL and
# a token to sign in with, and runs until Ctrl-C.
#
# Usage: scripts/demo.sh [--port N] [--keep DIR] [repository...]
#
# A repository is a local Git directory, a Git URL, or owner/name on
# GitHub (cloned with gh if it is signed in, so private repositories work,
# and with git otherwise). Without any, the demo uses
# this repository and a few small trukeio ones. Each becomes a namespace
# named after it, with all its branches and tags.
#
#   --port N    the viewer's port (default 8090); F1 listens on the next two
#   --keep DIR  keep the data in DIR (created if missing) instead of a
#               temporary directory that is removed at exit; running again
#               with the same DIR reuses it without pushing again
set -euo pipefail

port=8090
keep=""
repos=()
while [ $# -gt 0 ]; do
	case "$1" in
	--port) port=$2; shift 2 ;;
	--keep) keep=$2; shift 2 ;;
	-h|--help) sed -n '2,/^set -e/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
	-*) echo "demo: unknown flag $1" >&2; exit 2 ;;
	*) repos+=("$1"); shift ;;
	esac
done

root=$(cd "$(dirname "$0")/.." && pwd)
if [ ${#repos[@]} -eq 0 ]; then
	repos=("$root" trukeio/ogdl trukeio/gstore trukeio/ktest trukeio/folio)
fi
f1port=$((port + 1))
s3port=$((port + 2))

if [ -n "$keep" ]; then
	mkdir -p "$keep"
	work=$(cd "$keep" && pwd)
else
	work=$(mktemp -d)
fi
pids=()
cleanup() {
	for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	if [ -z "$keep" ]; then rm -rf "$work"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

echo "Building f1d and f1web..."
make -C "$root" --no-print-directory build > /dev/null
f1d="$root/bin/f1d"
f1web="$root/bin/f1web"

cat > "$work/f1d.toml" <<EOF
[server]
data_dir = "$work/data"
listen = "127.0.0.1:$f1port"
[server.s3]
listen = "127.0.0.1:$s3port"
[log]
level = "warn"
EOF
cat > "$work/f1web.toml" <<EOF
listen = "127.0.0.1:$port"
public_url = "http://127.0.0.1:$port"
f1_url = "http://127.0.0.1:$f1port"
[log]
level = "warn"
EOF

fresh=false
if [ ! -f "$work/token" ]; then
	fresh=true
	"$f1d" user add --config "$work/f1d.toml" demo > /dev/null
	"$f1d" acl set --config "$work/f1d.toml" '*' user:demo "" admin
	"$f1d" token create --config "$work/f1d.toml" demo 2> /dev/null | head -n 1 > "$work/token"
fi
token=$(cat "$work/token")

wait_for() {
	for _ in $(seq 100); do
		if curl -fsS "$1" > /dev/null 2>&1; then return 0; fi
		sleep 0.1
	done
	echo "demo: $1 does not answer" >&2
	exit 1
}

"$f1d" serve --config "$work/f1d.toml" 2> "$work/f1d.log" &
pids+=($!)
wait_for "http://127.0.0.1:$f1port/healthz"

# namespace turns a repository name into a namespace name: lowercase
# letters, digits, '.', '_' and '-', starting and ending with a letter or
# digit.
namespace() {
	local n
	n=$(basename "$1" .git | tr 'A-Z' 'a-z' | tr -c 'a-z0-9._\n-' '-' | sed 's/^[^a-z0-9]*//; s/[^a-z0-9]*$//')
	echo "${n:0:63}"
}

if $fresh; then
	mkdir -p "$work/src"
	for r in "${repos[@]}"; do
		ns=$(namespace "$r")
		src=$r
		if [ ! -d "$r" ]; then
			src="$work/src/$ns.git"
			case "$r" in
			*://*|*@*:*) clone=(git clone --quiet --bare "$r" "$src") ;;
			*)
				# gh, if signed in, also clones private repositories.
				if command -v gh > /dev/null && gh auth status > /dev/null 2>&1; then
					clone=(gh repo clone "$r" "$src" -- --quiet --bare)
				else
					clone=(git clone --quiet --bare "https://github.com/$r.git" "$src")
				fi
				;;
			esac
			echo "Cloning $r..."
			if ! "${clone[@]}" 2> "$work/clone.log"; then
				echo "  skipped: $(tail -n 1 "$work/clone.log")" >&2
				continue
			fi
		fi
		echo "Pushing $r to namespace $ns..."
		curl -fsS -H "Authorization: Bearer $token" -d "{\"name\":\"$ns\",\"preset\":\"git\"}" \
			"http://127.0.0.1:$f1port/api/v1/ns" > /dev/null
		if ! git -C "$src" push --quiet "http://demo:$token@127.0.0.1:$f1port/git/$ns.git" \
			'refs/heads/*:refs/heads/*' 'refs/tags/*:refs/tags/*' 2> "$work/push.log"; then
			echo "  push failed: $(tail -n 1 "$work/push.log")" >&2
		fi
	done
fi

"$f1web" --config "$work/f1web.toml" 2> "$work/f1web.log" &
pids+=($!)
wait_for "http://127.0.0.1:$port/-/healthz"

cat <<EOF

F1 demo running:
  viewer   http://127.0.0.1:$port/
  token    $token   (paste it at "Sign in with a token")
  F1 API   http://127.0.0.1:$f1port/api/v1/   Git: http://127.0.0.1:$f1port/git/<namespace>.git
  data     $work$([ -z "$keep" ] && echo " (removed at exit)")
  logs     $work/f1d.log, $work/f1web.log

Press Ctrl-C to stop.
EOF
wait
