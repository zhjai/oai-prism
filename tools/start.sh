#!/usr/bin/env bash
set -euo pipefail
umask 077

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd -- "$repo"
binary=${OAIPRISM_BINARY:-"$repo/oaiprism"}
config=${OAIPRISM_CONFIG:-"$repo/configs/config.yaml"}
[[ "$binary" = /* ]] || binary="$repo/$binary"
[[ "$config" = /* ]] || config="$repo/$config"

if [[ ! -x "$binary" ]]; then
    if [[ -n ${OAIPRISM_BINARY:-} ]]; then
        echo "Binary is not executable: $binary" >&2
        exit 1
    fi
    if [[ ! -f go.mod ]] || ! command -v go >/dev/null 2>&1; then
        echo "Install a Linux release package or Go 1.26+ to build from source." >&2
        exit 1
    fi
    echo "Building oaiprism..."
    go build -trimpath -o "$binary" ./cmd/oaiprism
fi

if [[ ! -f "$config" ]]; then
    if [[ -n ${OAIPRISM_CONFIG:-} ]]; then
        echo "Configuration does not exist: $config" >&2
        exit 1
    fi
    cp -- configs/config.example.yaml "$config"
    echo "Created configs/config.yaml"
fi

# Keep the gateway in the foreground so Ctrl+C and systemd reach it directly.
exec "$binary" serve -config "$config" "$@"
