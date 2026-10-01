#!/usr/bin/env bash
# Runs govulncheck and fails on any reachable vulnerability that is not listed,
# unexpired, in security/vuln-exceptions.txt.
set -uo pipefail
cd "$(dirname "$0")/.."
out=$(.tools/bin/govulncheck ./... 2>&1)
status=$?
echo "$out"
[ $status -eq 0 ] && exit 0

today=$(date +%Y-%m-%d)
fail=0
for id in $(echo "$out" | grep -oE '^Vulnerability #[0-9]+: GO-[0-9]+-[0-9]+' | grep -oE 'GO-[0-9]+-[0-9]+' | sort -u); do
  line=$(grep -E "^$id " security/vuln-exceptions.txt || true)
  if [ -z "$line" ]; then
    echo "FAIL: $id is reachable and has no exception"; fail=1; continue
  fi
  expires=$(echo "$line" | awk '{print $2}')
  if [[ "$today" > "$expires" ]]; then
    echo "FAIL: the exception for $id expired on $expires"; fail=1
  else
    echo "accepted until $expires: $id"
  fi
done
exit $fail
