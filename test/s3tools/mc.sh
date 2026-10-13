#!/usr/bin/env bash
# MinIO client: mc mirror, ls, cat and diff against F1.
. "$(dirname "$0")/common.sh"
export MC_CONFIG_DIR=$WORK/mc-config
mc() { command mc --quiet --no-color "$@"; }
mc alias set f1 "$S3_ENDPOINT" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" --api S3v4 --path on >/dev/null

src=$WORK/mc-src
make_tree "$src"
for b in "$BUCKET" "$TREE_BUCKET"; do
	mc mirror "$src" "f1/$b/mirror" >/dev/null
	out=$(mc diff "$src" "f1/$b/mirror")
	[ -z "$out" ] || fail "$b: diff after mirror: $out"
	# A second mirror copies nothing.
	out=$(mc mirror --json "$src" "f1/$b/mirror" | grep -c '"status":"success","source"' || true)
	[ "$out" -eq 0 ] || fail "$b: second mirror copied $out objects"
	[ "$(mc cat "f1/$b/mirror/a.txt")" = "a.txt" ] || fail "$b: cat"
	n=$(mc ls --recursive "f1/$b/mirror" | wc -l)
	[ "$n" -eq "$(find "$src" -type f | wc -l)" ] || fail "$b: ls shows $n objects"
	rm -rf "$WORK/mc-back"
	mc mirror "f1/$b/mirror" "$WORK/mc-back" >/dev/null
	diff -r "$src" "$WORK/mc-back" || fail "$b: mirror back differs"
	mc rm --recursive --force "f1/$b/mirror/a" >/dev/null
	[ "$(mc ls --recursive "f1/$b/mirror/a/" | wc -l)" -eq 0 ] || fail "$b: rm --recursive"
done
echo "mc: ok"
