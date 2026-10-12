#!/usr/bin/env bash
# Fail if total statement coverage is below COVER_MIN (percent).
set -euo pipefail
COVER_MIN="${COVER_MIN:-95}"
PROFILE="${1:-coverage.out}"
if [[ ! -f "$PROFILE" ]]; then
  echo "coverage profile missing: $PROFILE" >&2
  exit 1
fi
total="$(go tool cover -func="$PROFILE" | awk '/^total:/{gsub(/%/,"",$3); print $3}')"
if [[ -z "$total" ]]; then
  echo "could not parse total coverage from $PROFILE" >&2
  exit 1
fi
awk -v total="$total" -v min="$COVER_MIN" 'BEGIN {
  if (total+0 < min+0) {
    printf "coverage %.1f%% is below required %.1f%%\n", total, min > "/dev/stderr"
    exit 1
  }
  printf "coverage %.1f%% (min %.1f%%)\n", total, min
}'
