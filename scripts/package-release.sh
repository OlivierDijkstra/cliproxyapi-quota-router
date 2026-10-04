#!/usr/bin/env bash
# Packages built libraries into release assets.
#
# Input layout: IN_DIR/<goos>-<goarch>/cliproxyapi-quota-router.<ext>
# Output, per platform:
#   cliproxyapi-quota-router_<version>_<goos>_<goarch>.zip  plugin-store archive, library at the zip root
#   cliproxyapi-quota-router-<goos>-<goarch>.<ext>          bare library for manual installs
# plus checksums.txt (sha256) covering every asset.
#
# Usage: scripts/package-release.sh IN_DIR OUT_DIR VERSION
set -euo pipefail

in_dir=${1:?usage: package-release.sh IN_DIR OUT_DIR VERSION}
out_dir=${2:?usage: package-release.sh IN_DIR OUT_DIR VERSION}
version=${3:?usage: package-release.sh IN_DIR OUT_DIR VERSION}
id=cliproxyapi-quota-router

mkdir -p "$out_dir"
out_dir=$(cd "$out_dir" && pwd)
count=0
for platform_dir in "$in_dir"/*-*/; do
  platform=$(basename "$platform_dir")
  goos=${platform%-*}
  goarch=${platform#*-}
  case "$goos" in
    darwin) ext=dylib ;;
    windows) ext=dll ;;
    linux) ext=so ;;
    *) echo "unexpected platform $platform" >&2; exit 1 ;;
  esac
  lib="$id.$ext"
  if [[ ! -f "$platform_dir/$lib" ]]; then
    echo "missing $platform_dir$lib" >&2
    exit 1
  fi
  archive="${id}_${version}_${goos}_${goarch}.zip"
  (cd "$platform_dir" && zip -q -X "$out_dir/$archive" "$lib")
  if [[ "$(unzip -Z1 "$out_dir/$archive")" != "$lib" ]]; then
    echo "$archive must contain only $lib at its root" >&2
    exit 1
  fi
  cp "$platform_dir/$lib" "$out_dir/$id-$goos-$goarch.$ext"
  count=$((count + 1))
done
if [[ $count -eq 0 ]]; then
  echo "no platforms found in $in_dir" >&2
  exit 1
fi
(cd "$out_dir" && sha256sum -- *.zip "$id"-*-* > checksums.txt && sha256sum -c --quiet checksums.txt)
echo "packaged $count platforms into $out_dir"
