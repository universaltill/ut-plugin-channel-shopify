# Review — CI workflow YAML fix

Date: 2026-10-23. CI-only change (not a shipped file, no version bump).

## What shipped

The first push to `main` ran no CI: `ci.yml` had a step name with an unquoted `word: word` (`Lint (golangci-lint: import guard, ...)`), which is invalid YAML, so GitHub reported "likely failed because of a workflow file issue" and ran no job. The name is fixed, and `scripts/check-workflows.sh` (ruby YAML parse of every workflow) now runs first in CI.

## Verified

The guard fails (exit 1, with the YAML error) on the old file and passes on the fixed one; shellcheck clean. No code or permission change. A trivial CI fix, reviewed by the builder only.

## Verdict

Safe to merge.
