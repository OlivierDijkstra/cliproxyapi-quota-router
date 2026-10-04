#!/usr/bin/env bash
# Loads a built plugin library through CLIProxyAPI's own plugin host and drives
# register, usage.handle, scheduler.pick, reconfigure and shutdown through the
# C ABI. See scripts/testdata/quota_router_smoke_test.go.
#
# Usage: scripts/host-smoke.sh LIB CLIPROXYAPI_DIR
#   LIB              the built cliproxyapi-quota-router.<ext>
#   CLIPROXYAPI_DIR  a CLIProxyAPI checkout at the version in go.mod
set -euo pipefail

lib=${1:?usage: host-smoke.sh LIB CLIPROXYAPI_DIR}
upstream=${2:?usage: host-smoke.sh LIB CLIPROXYAPI_DIR}
here=$(cd "$(dirname "$0")" && pwd)
lib="$(cd "$(dirname "$lib")" && pwd)/$(basename "$lib")"

cp "$here/testdata/quota_router_smoke_test.go" "$upstream/internal/pluginhost/zz_quota_router_smoke_test.go"
cd "$upstream"
QUOTA_ROUTER_LIB="$lib" CGO_ENABLED=1 go test -tags quotarouter_smoke \
  -run '^TestQuotaRouterLoaderSmoke$' -count=1 -v ./internal/pluginhost
