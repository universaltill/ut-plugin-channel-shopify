#!/usr/bin/env bash
# Every workflow file must parse as YAML. A step name with an unquoted
# "word: word" is a YAML error, and GitHub then runs no job at all and only
# says "likely failed because of a workflow file issue" (it hid CI on the
# first push). Uses ruby, which ships on ubuntu-latest and macOS.
set -euo pipefail
cd "$(dirname "$0")/.."
status=0
for f in .github/workflows/*.yml; do
  if ! ruby -ryaml -e 'YAML.load_file(ARGV[0])' "$f" 2>/tmp/check-workflows.err; then
    echo "FAIL: $f is not valid YAML: $(head -c 200 /tmp/check-workflows.err)" >&2
    status=1
  fi
done
[ "$status" -eq 0 ] && echo "ok workflows parse"
exit "$status"
