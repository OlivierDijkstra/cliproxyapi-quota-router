#!/usr/bin/env bash
# Builds the plugin shared library for the current GOOS/GOARCH and checks that
# it exports the CLIProxyAPI C ABI entry points.
#
# Usage: scripts/build-plugin.sh OUT_DIR [VERSION]
set -euo pipefail

out_dir=${1:?usage: build-plugin.sh OUT_DIR [VERSION]}
version=${2:-}

goos=$(go env GOOS)
case "$goos" in
  darwin) ext=dylib ;;
  windows) ext=dll ;;
  *) ext=so ;;
esac

mkdir -p "$out_dir"
lib="$out_dir/cliproxyapi-quota-router.$ext"
ldflags=""
if [[ -n "$version" ]]; then
  ldflags="-X github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router.Version=$version"
fi

CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags "$ldflags" -o "$lib" ./plugin
rm -f "$out_dir/cliproxyapi-quota-router.h"

case "$goos" in
  darwin) symbols=$(nm -gU "$lib") ;;
  linux) symbols=$(nm -D --defined-only "$lib") ;;
  *) symbols=$(go tool nm "$lib") ;;
esac
for name in cliproxy_plugin_init cliproxyPluginCall cliproxyPluginFree cliproxyPluginShutdown; do
  # Here-string instead of a pipe: grep -q can SIGPIPE the producer under pipefail.
  if ! grep -Eq "[ _]${name}\$" <<<"$symbols"; then
    echo "missing export: $name" >&2
    exit 1
  fi
done
echo "$lib"
