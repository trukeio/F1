#!/usr/bin/env bash
# rclone: copy, sync and check against F1; a second sync changes nothing.
. "$(dirname "$0")/common.sh"
export RCLONE_CONFIG_F1_TYPE=s3 RCLONE_CONFIG_F1_PROVIDER=Other RCLONE_CONFIG_F1_ENDPOINT=$S3_ENDPOINT \
	RCLONE_CONFIG_F1_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID RCLONE_CONFIG_F1_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY \
	RCLONE_CONFIG_F1_REGION=us-east-1 RCLONE_CONFIG_F1_FORCE_PATH_STYLE=true RCLONE_CONFIG=/dev/null

src=$WORK/rclone-src
make_tree "$src"
for b in "$BUCKET" "$TREE_BUCKET"; do
	rclone copy "$src" "f1:$b/copy"
	rclone check "$src" "f1:$b/copy" 2>"$WORK/check.log" || { cat "$WORK/check.log"; fail "$b: check after copy"; }
	rclone sync "$src" "f1:$b/sync"
	out=$(rclone sync -v "$src" "f1:$b/sync" 2>&1)
	echo "$out" | grep -qE 'Transferred:\s+0 B' || fail "$b: second sync transferred: $out"
	echo "$out" | grep -qE 'Copied|Deleted' && fail "$b: second sync changed files: $out"
	rm -rf "$WORK/rclone-back"
	rclone copy "f1:$b/sync" "$WORK/rclone-back"
	diff -r "$src" "$WORK/rclone-back" || fail "$b: copy back differs"
	# Deletions propagate.
	rm "$src/a/many-1"
	rclone sync "$src" "f1:$b/sync"
	rclone check "$src" "f1:$b/sync" 2>"$WORK/check.log" || { cat "$WORK/check.log"; fail "$b: check after delete"; }
	printf 'a/many-1\n' > "$src/a/many-1"
done
echo "rclone: ok"
