#!/usr/bin/env bash
# Installs the pinned external tools of the tests (AWS CLI v2, rclone,
# MinIO client, Git LFS) into <dir>/bin, verifying each download against
# its pinned SHA-256.
#
# Usage: test/s3tools/install.sh <dir>
set -euo pipefail

AWSCLI_VERSION=2.37.4
AWSCLI_SHA256=0c59444563f4df735eeb5481f6165f95dae546c33761760d8be9855d5cfe2d12
RCLONE_VERSION=v1.75.1
RCLONE_SHA256=982b5aa772841168f8e380f139e9e787b2a105403e32b94da8676a0e1c0a13ab
MC_VERSION=RELEASE.2025-08-13T08-35-41Z
MC_SHA256=01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891
GITLFS_VERSION=v3.8.0
GITLFS_SHA256=e455e00f15d9b95661b8d53498ffb0c3367962cf1ec73c31ab7369516cd6ab8d

dir=$(mkdir -p "$1" && realpath "$1")
mkdir -p "$dir/bin"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch() { # url sha256 file
	curl -fsSL -o "$3" "$1"
	echo "$2  $3" | sha256sum -c --quiet -
}

fetch "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-$AWSCLI_VERSION.zip" "$AWSCLI_SHA256" "$tmp/awscli.zip"
mkdir "$tmp/awscli"
unzip -q "$tmp/awscli.zip" -d "$tmp/awscli"
"$tmp/awscli/aws/install" -i "$dir/aws-cli" -b "$dir/bin" --update >/dev/null

fetch "https://github.com/rclone/rclone/releases/download/$RCLONE_VERSION/rclone-$RCLONE_VERSION-linux-amd64.zip" "$RCLONE_SHA256" "$tmp/rclone.zip"
mkdir "$tmp/rclone"
unzip -q "$tmp/rclone.zip" -d "$tmp/rclone"
install -m 755 "$tmp/rclone/rclone-$RCLONE_VERSION-linux-amd64/rclone" "$dir/bin/rclone"

fetch "https://github.com/minio/mc/releases/download/$MC_VERSION/mc.linux-amd64.$MC_VERSION" "$MC_SHA256" "$tmp/mc"
install -m 755 "$tmp/mc" "$dir/bin/mc"

fetch "https://github.com/git-lfs/git-lfs/releases/download/$GITLFS_VERSION/git-lfs-linux-amd64-$GITLFS_VERSION.tar.gz" "$GITLFS_SHA256" "$tmp/git-lfs.tar.gz"
mkdir "$tmp/git-lfs"
tar -xzf "$tmp/git-lfs.tar.gz" -C "$tmp/git-lfs"
find "$tmp/git-lfs" -name git-lfs -type f -exec install -m 755 {} "$dir/bin/git-lfs" \;

"$dir/bin/aws" --version
"$dir/bin/rclone" version | sed -n 1p
"$dir/bin/mc" --version | sed -n 1p
"$dir/bin/git-lfs" version
