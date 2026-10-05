#!/bin/sh
# Fetches Lemonade's ACE-Step vulkan backend (acestep.cpp release) into the cache volume.
# Why: acestep.vulkan_bin points Lemonade at ace-server-lowvram.sh; Lemonade then reports the backend
# as "installed" and `backends install` does nothing, so after a wiped model volume the real binary
# the wrapper execs is missing. Version and sha256 come from Lemonade's own backend_versions.json,
# so a Lemonade upgrade moves this along.
# usage: fetch-acestep.sh <cache-dir>     (e.g. /opt/lemonade/.cache/lemonade)
set -eu
cache=${1:?cache dir}
res=${BACKEND_VERSIONS:-/opt/lemonade/resources/backend_versions.json}
file=acestep-vulkan-linux-x64.tar.gz
dest=$cache/bin/acestep/vulkan

set -- $(awk -v f="$file" '
  /"pwilkin\/acestep.cpp"/ {blk=1; next}
  blk && /"v[0-9.]+": *\{/ {match($0,/v[0-9.]+/); v=substr($0,RSTART,RLENGTH)}
  blk && index($0,f) {match($0,/sha256:[0-9a-f]+/); print v, substr($0,RSTART+7,RLENGTH-7); exit}' "$res")
ver=${1:?no acestep version in $res}; sha=${2:?no sha256 in $res}

if [ -x "$dest/ace-server" ] && [ "$(cat "$dest/.fetched-version" 2>/dev/null)" = "$ver" ]; then
  echo "acestep $ver already present"; exit 0
fi
echo "fetching acestep $ver"
tmp=$cache/.acestep-tmp; rm -rf "$tmp"; mkdir -p "$tmp"
url=https://github.com/pwilkin/acestep.cpp/releases/download/$ver/$file
n=0
until curl -fsSL -m 600 -o "$tmp/$file" "$url"; do
  n=$((n+1)); [ $n -ge 3 ] && { echo "download failed: $url" >&2; rm -rf "$tmp"; exit 1; }
  sleep 5
done
echo "$sha  $tmp/$file" | sha256sum -c - >/dev/null || { echo "sha256 mismatch" >&2; rm -rf "$tmp"; exit 1; }
mkdir -p "$tmp/x" && tar xzf "$tmp/$file" -C "$tmp/x"
[ -x "$tmp/x/ace-server" ] || { echo "ace-server missing in archive" >&2; rm -rf "$tmp"; exit 1; }
echo "$ver" > "$tmp/x/.fetched-version"
rm -rf "$dest"; mkdir -p "$(dirname "$dest")"; mv "$tmp/x" "$dest"; rm -rf "$tmp"
echo "acestep $ver installed to $dest"
