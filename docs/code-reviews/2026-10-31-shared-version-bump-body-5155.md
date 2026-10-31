# Review: version-bump guard on the shared body (ut-docs#5155)

Date: 2026-10-31. Card: ut-docs#5155. Lane: lane:cloud-41.

## What shipped

- `ut-plugin-channel-demo` and `ut-plugin-channel-shopify` no longer carry an
  older inline copy of the version-bump guard. `scripts/check-version-bump.sh`
  is now a thin wrapper: the same `SHIPPED_PATTERNS` (unchanged), a
  `SHIPPED_NOTE`, and it sources `scripts/check-version-bump.body.sh`. The body
  is the byte-identical canonical copy from ut-docs
  `scripts/templates/check-version-bump.body.sh`, and it checks its own hash.
- `scripts/check-version-bump.test.sh` copies the body into each fixture and
  gains the hand-edited-body case (ut-docs#4489). The tests went from 26 to 27
  cases.
- ut-docs `architecture/plugin-architecture.md` §8 drops the "older inline copy"
  note and counts 19 repos.

## Verification

- TDD: the test failed before the body existed (`cp: cannot stat
  scripts/check-version-bump.body.sh`). It passed 27/27 after the sync, in both repos.
- `sync-version-bump-body.sh --check` on both repos printed `ok` for each.
  `shellcheck scripts/*.sh` was clean, and `scripts/check-version-bump.sh` against
  origin/main printed ok. In shopify, `scripts/check-workflows.sh` also passed.
- No shipped file changed, so no version bump is needed and no release follows.

## Independent review (different model, Sonnet; author Opus)

- No blockers or majors. The reviewer diffed the inline logic against the shared
  body. The first-release block, the already-tagged check and `read_version` are
  identical. The only text change is the failure message, which reads the same
  through `SHIPPED_NOTE`. The rename, stale-fork and first-release cases still pass.
- Nit (fixed): say "19 repos" in the architecture doc.
- Minor (accepted): the wrapper comment names this card as historical context.
- `code-reviews/2026-10-29-version-bump-package-sh-5147.md` still mentions the
  inline copy. It is a dated record, so it was left as is.

Verdict: safe to merge.
