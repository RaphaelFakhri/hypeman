#!/usr/bin/env bash
# Runs one shard of the Linux test suite. CI runs every shard in parallel.
#
# Usage: test-shard.sh SHARD COUNT
#
# The suite is split into COUNT shards. Shard "other" runs every package except
# lib/instances. Shards instances-1 through instances-<COUNT-1> split the
# top-level tests of lib/instances, the slowest package, by a hash of each test
# name. Tests are discovered with -test.list, so new tests are assigned
# automatically and existing tests keep their shard.
#
# Before running, an instances shard checks that the -run patterns of all
# instances shards together select every test exactly once.
set -euo pipefail

shard="${1:?usage: test-shard.sh SHARD COUNT}"
count="${2:?usage: test-shard.sh SHARD COUNT}"
tags=(-tags containers_image_openpgp)
test_flags=(-timeout="${TEST_TIMEOUT:-300s}")
if [ -n "${VERBOSE:-}" ]; then
  test_flags+=(-v)
fi
instances_pkg="$(go list "${tags[@]}" ./lib/instances)"

if [ "$shard" = other ]; then
  mapfile -t pkgs < <(go list "${tags[@]}" ./... | grep -vxF "$instances_pkg")
  exec go test "${tags[@]}" "${test_flags[@]}" "${pkgs[@]}"
fi

parts=$((count - 1))
index="${shard#instances-}"
if [[ "$shard" != instances-* || ! "$index" =~ ^[1-9][0-9]*$ ]] || ((index > parts)); then
  echo "unknown shard $shard (want other or instances-1..instances-$parts)" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
go test "${tags[@]}" -c -o "$tmp/instances.test" ./lib/instances
"$tmp/instances.test" -test.list '.*' | sort > "$tmp/all"
while read -r name; do
  sum="$(printf '%s' "$name" | cksum)"
  echo "$((${sum%% *} % parts + 1)) $name"
done < "$tmp/all" > "$tmp/assigned"

pattern() {
  echo "^($(awk -v shard="$1" '$1 == shard { print $2 }' "$tmp/assigned" | paste -sd'|'))\$"
}

for ((i = 1; i <= parts; i++)); do
  "$tmp/instances.test" -test.list "$(pattern "$i")"
done | sort > "$tmp/selected"
if ! diff -u "$tmp/all" "$tmp/selected"; then
  echo "lib/instances shards do not select every test exactly once" >&2
  exit 1
fi
echo "lib/instances: shard $index of $parts runs $(awk -v shard="$index" '$1 == shard' "$tmp/assigned" | wc -l) of $(wc -l < "$tmp/all") tests"

go test "${tags[@]}" "${test_flags[@]}" -run "$(pattern "$index")" ./lib/instances
