#!/usr/bin/env bash
# Runs the selected Ceph s3-tests (test/s3tests.txt) against a fresh f1d.
#
# Usage: test/s3tests.sh <f1d binary> <s3-tests checkout> [pytest args...]
#
# The checkout is github.com/ceph/s3-tests at the commit in S3TESTS_COMMIT,
# with its Python requirements installed in the active environment (see the
# s3tests job in .github/workflows/ci.yml). With S3TESTS_ALL=1, every test
# in test_s3.py runs instead of the selection, to review the selection.
set -euo pipefail

S3TESTS_COMMIT=5522d1c351f75bc00ae0f64f742f3f095f5939d9

bin=$(realpath "$1")
suite=$(realpath "$2")
shift 2
here=$(dirname "$(realpath "$0")")
work=$(mktemp -d)
trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; rm -rf "$work"' EXIT

if [ "$(git -C "$suite" rev-parse HEAD)" != "$S3TESTS_COMMIT" ]; then
	echo "s3-tests checkout is not at $S3TESTS_COMMIT" >&2
	exit 1
fi

free_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'
}
port=$(free_port)
s3port=$(free_port)
cat > "$work/f1d.toml" <<EOF
[server]
data_dir = "data"
listen = "127.0.0.1:$port"
[server.s3]
listen = "127.0.0.1:$s3port"
[log]
level = "warn"
EOF
cfg=$work/f1d.toml

# Users: "main" administers the server (it creates buckets); "alt" and
# "tenant" are other users.
declare -A key secret
for u in main alt tenant; do
	"$bin" user add --config "$cfg" "$u" >/dev/null
	out=$("$bin" s3key create --config "$cfg" "$u")
	key[$u]=$(sed -n 's/^access_key = //p' <<<"$out")
	secret[$u]=$(sed -n 's/^secret_key = //p' <<<"$out")
done
"$bin" acl set --config "$cfg" '*' user:main "" admin

user_section() {
	cat <<EOF
[$1]
display_name = $2
user_id = user:$2
email = $2@example.com
access_key = ${key[$2]}
secret_key = ${secret[$2]}
api_name =
EOF
}
{
	cat <<EOF
[DEFAULT]
host = 127.0.0.1
port = $s3port
is_secure = False
ssl_verify = False

[fixtures]
bucket prefix = f1test-{random}-
iam name prefix = s3-tests-
iam path prefix = /s3-tests/

EOF
	user_section "s3 main" main
	echo
	user_section "s3 alt" alt
	echo
	user_section "s3 tenant" tenant
	echo "tenant = tenant"
	echo
	user_section "iam" main
	echo
	user_section "iam root" main
	echo
	user_section "iam alt root" alt
} > "$work/s3tests.conf"

"$bin" serve --config "$cfg" 2> "$work/f1d.log" &
pid=$!
for _ in $(seq 50); do
	curl -fsS "http://127.0.0.1:$port/healthz" > /dev/null 2>&1 && break
	sleep 0.1
done

if [ "${S3TESTS_ALL:-}" = 1 ]; then
	tests=(s3tests/functional/test_s3.py)
else
	mapfile -t tests < <(grep -v '^\s*#' "$here/s3tests.txt" | grep -v '^\s*$' | sed 's|^|s3tests/functional/test_s3.py::|')
fi
cd "$suite"
status=0
S3TEST_CONF=$work/s3tests.conf python3 -m pytest -q -p no:cacheprovider "${tests[@]}" "$@" || status=$?
if [ "$status" -ne 0 ]; then
	echo "--- f1d log (last 50 lines)" >&2
	tail -50 "$work/f1d.log" >&2
fi
exit "$status"
