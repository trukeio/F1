# Shared setup for the S3 tool scripts. The Go test TestS3Tools runs each
# script with:
#   S3_ENDPOINT            http://127.0.0.1:<port> of an F1 S3 listener
#   AWS_ACCESS_KEY_ID      an F1 S3 key of a server administrator
#   AWS_SECRET_ACCESS_KEY
#   BUCKET                 an empty bucket (flat layout)
#   TREE_BUCKET            an empty bucket (tree layout)
#   WORK                   a scratch directory
set -euo pipefail

export AWS_DEFAULT_REGION=us-east-1 AWS_REGION=us-east-1
: "${S3_ENDPOINT:?}" "${BUCKET:?}" "${TREE_BUCKET:?}" "${WORK:?}"

# make_tree <dir>: test files of several sizes, nested directories, names
# with spaces and non-ASCII characters, and the "a.txt" / "a/b" ordering
# case (listings must be in byte order of the full key).
make_tree() {
	local d=$1
	mkdir -p "$d/a" "$d/dir with space/sub" "$d/ünï"
	printf 'a.txt\n' > "$d/a.txt"
	printf 'a/b\n' > "$d/a/b"
	printf 'empty follows\n' > "$d/dir with space/x y.txt"
	: > "$d/dir with space/sub/empty"
	head -c 300000 /dev/urandom > "$d/ünï/medium.bin"
	head -c 21000000 /dev/urandom > "$d/large.bin" # multipart for most tools
	for i in $(seq 1 30); do printf 'file %d\n' "$i" > "$d/a/many-$i"; done
}

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
