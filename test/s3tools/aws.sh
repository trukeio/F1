#!/usr/bin/env bash
# AWS CLI: aws s3 cp/sync/ls and aws s3api calls against F1.
. "$(dirname "$0")/common.sh"
aws() { command aws --endpoint-url "$S3_ENDPOINT" "$@"; }

src=$WORK/aws-src
make_tree "$src"
for b in "$BUCKET" "$TREE_BUCKET"; do
	aws s3 cp "$src/large.bin" "s3://$b/single/large.bin" >/dev/null
	aws s3 cp "s3://$b/single/large.bin" "$WORK/large.back" >/dev/null
	cmp "$src/large.bin" "$WORK/large.back" || fail "$b: cp round trip"

	aws s3 sync "$src" "s3://$b/tree" >/dev/null
	# A second sync finds nothing to do.
	out=$(aws s3 sync "$src" "s3://$b/tree")
	[ -z "$out" ] || fail "$b: second sync transferred: $out"
	rm -rf "$WORK/aws-back"
	aws s3 sync "s3://$b/tree" "$WORK/aws-back" >/dev/null
	diff -r "$src" "$WORK/aws-back" || fail "$b: sync back differs"
	out=$(aws s3 sync "s3://$b/tree" "$WORK/aws-back")
	[ -z "$out" ] || fail "$b: second sync down transferred: $out"

	n=$(aws s3 ls --recursive "s3://$b/tree/" | wc -l)
	[ "$n" -eq "$(find "$src" -type f | wc -l)" ] || fail "$b: ls shows $n objects"

	aws s3api put-object --bucket "$b" --key api/obj --body "$src/a.txt" --content-type text/plain --metadata k=v >/dev/null
	aws s3api head-object --bucket "$b" --key api/obj | grep -q '"k": "v"' || fail "$b: head-object metadata"
	aws s3api copy-object --bucket "$b" --key api/copy --copy-source "$b/api/obj" >/dev/null
	aws s3api get-object --bucket "$b" --key api/copy "$WORK/copy" >/dev/null
	cmp "$src/a.txt" "$WORK/copy" || fail "$b: copy-object"
	n=$(aws s3api list-objects-v2 --bucket "$b" --prefix api/ --query 'length(Contents)')
	[ "$n" = 2 ] || fail "$b: list-objects-v2 lists $n objects"
	aws s3api delete-object --bucket "$b" --key api/copy
	aws s3 rm --recursive "s3://$b/tree/a/" >/dev/null
	[ "$(aws s3 ls --recursive "s3://$b/tree/a/" | wc -l)" -eq 0 ] || fail "$b: rm --recursive"
done
echo "aws: ok"
