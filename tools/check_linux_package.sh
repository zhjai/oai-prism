#!/usr/bin/env bash
set -euo pipefail

archive=${1:?Usage: check_linux_package.sh <native-archive> <version>}
version=${2:?Usage: check_linux_package.sh <native-archive> <version>}
work=$(mktemp -d)
pid=
cleanup() {
    if [[ -n "$pid" ]]; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    fi
    rm -rf -- "$work"
}
trap cleanup EXIT

# Use a path with spaces and invoke from outside the install directory.
root="$work/install directory"
mkdir -p "$root"
tar -xzf "$archive" -C "$root" --strip-components=1
test "$("$root/oaiprism" version)" = "oaiprism $version"
test -s "$root/web/dist/index.html"
test -s "$root/configs/config.example.yaml"
test -s "$root/deploy/oaiprism.service"
test ! -e "$root/configs/config.yaml"
test ! -e "$root/secrets"

port=$((20000 + RANDOM % 20000))
if (echo >"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "Smoke test port is already occupied: $port" >&2
    exit 1
fi
"$root/tools/start.sh" -port "$port" >"$work/gateway.log" 2>&1 &
pid=$!
url="http://127.0.0.1:$port"
healthy=false
for ((i = 0; i < 50; i++)); do
    if ! kill -0 "$pid" 2>/dev/null; then
        cat "$work/gateway.log" >&2
        exit 1
    fi
    if curl -fsS --max-time 2 "$url/healthz" >/dev/null 2>&1; then
        healthy=true
        break
    fi
    sleep 0.2
done
if [[ "$healthy" != true ]]; then
    cat "$work/gateway.log" >&2
    exit 1
fi
test "$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "$url/readyz")" = 503
curl -fsS --max-time 5 "$url/v1/models" >"$work/models.json"
# A fresh package has no credentials, so the dynamic catalog must be empty.
grep -Fq '"object":"list"' "$work/models.json"
grep -Fq '"data":[]' "$work/models.json"
curl -fsS --max-time 5 "$url/dashboard/" >"$work/dashboard.html"
cmp "$root/web/dist/index.html" "$work/dashboard.html"
for asset in "$root/web/dist/assets/"*.js "$root/web/dist/assets/"*.css; do
    curl -fsS --max-time 5 "$url/dashboard/assets/$(basename -- "$asset")" >"$work/asset"
    cmp "$asset" "$work/asset"
done
test -f "$root/configs/config.yaml"
test "$(stat -c %a "$root/configs/config.yaml")" = 600
kill -TERM "$pid"
wait "$pid"
pid=
echo "PASS: version, startup, health, empty-account readiness, models, Dashboard assets and shutdown"
