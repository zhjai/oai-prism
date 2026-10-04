#!/usr/bin/env bash
set -euo pipefail
umask 022

version=${1:-}
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][a-zA-Z0-9.-]+)?$ ]]; then
    echo "Usage: $0 <vX.Y.Z[-suffix]> [output-directory]" >&2
    exit 2
fi

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd -- "$repo"
output=${2:-"$repo/dist"}
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
stage=$(mktemp -d)
trap 'rm -rf -- "$stage"' EXIT

for arch in amd64 arm64; do
    name="oaiprism-${version}-linux-${arch}"
    root="$stage/$name"
    mkdir -p "$root/configs" "$root/web" "$root/tools" "$root/deploy"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X main.version=$version" -o "$root/oaiprism" ./cmd/oaiprism
    # Copy runtime assets explicitly; local configs, accounts and databases never enter a release.
    cp -- configs/config.example.yaml "$root/configs/"
    cp -- tools/start.sh "$root/tools/"
    cp -- deploy/oaiprism.service "$root/deploy/"
    cp -- README.md LICENSE "$root/"
    cp -R -- docs "$root/docs"
    cp -R -- web/dist "$root/web/dist"
    chmod 755 "$root/oaiprism" "$root/tools/start.sh"
    tar --owner=0 --group=0 --numeric-owner -czf "$output/$name.tar.gz" -C "$stage" "$name"
done

cd -- "$output"
sha256sum "oaiprism-${version}-linux-amd64.tar.gz" \
    "oaiprism-${version}-linux-arm64.tar.gz" > SHA256SUMS
echo "Linux packages and SHA256SUMS: $output"
