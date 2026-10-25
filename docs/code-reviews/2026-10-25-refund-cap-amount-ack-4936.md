# Review: refund cap against admin refunds and refunded_amount_minor ack (v0.3.1) - ut-docs#4936

Built by a Sonnet subagent; reviewed independently by Opus 5.5 (fresh context,
detached worktree); review fixes by the orchestrator, re-verified.

## What shipped

- **R2 cap** (contract sales-channel-v1 8.4, adapter's duty 2): `refund.create`
  reads the order's `totalRefundedSet` in the same `RefundState` read that
  decides what is owed, before `suggestedRefund` and any mutation, and caps the
  refund at `max(0, max_amount_minor - max(0, refunded_now - refunded_before_minor))`.
  An absent `refunded_before_minor` (op queued before the cloud sent it) is a
  baseline of 0 (conservative: can only refuse a refund that was fine). Out of
  range is `invalid_payload`; an unreadable or foreign-currency refunded total
  is `rejected_by_channel` before any request that moves money.
- **R1 amount ack** (8.5): after a successful `refundCreate`, the refund's own
  `totalRefundedSet` is acked as `refunded_amount_minor` (0..999999999999,
  inside the host's 2^50 bound, never beside an error). Unreadable answer:
  acked without it, `refund.amount_unread` logged (line count only). The
  already-refunded, user-error-then-reread and field-error-then-reread acks
  carry no amount (the cloud counts the ceiling).
- **R7**: fixture with Shopify's tax remainder on the last refunded units, 1
  minor unit above the rounded-up ceiling: refused `refund_exceeds_ceiling`,
  no `refundCreate` sent.
- `fTotalRefunded` lives in `queries.go` (from-memory name, pinned by
  `TestFromMemoryShopifyNamesLiveInOnePlace`) with a README "Verify first" row.
  SDK `sdk/plugin` bumped to pseudo-version `v1.1.1-0.20261025013710-64927b5442b1`
  (has `Ack.RefundedAmountMinor`; its FakeHost refuses what the host refuses).
  Manifest 0.3.0 -> 0.3.1. The cloud host accepting the field is ut-cloud
  818b758 (ut-docs#4904), on `main` and built by `build-and-push` since 2026-10-24.

## Findings

| # | Severity | Finding | Outcome |
|---|---|---|---|
| 1 | minor | `TestRefundCapFloorsAtZero` could not fail: a 1-unit refund is refused with or without the floor. The floor only matters for a 0-amount refund. | **Fixed**: `TestRefundOfNothingIsAllowedAtACapFlooredAtZero` (zero-value unit, cap would be -1000). Verified: fails with the floor removed, passes with it. |
| 2 | minor | The new `refund { totalRefundedSet }` selection adds a way for a refund that was MADE to come back with a field-level GraphQL error, which `failFor` dead-lettered as `rejected_by_channel` (money moved, staff see a failure; no double refund since retries read first). | **Fixed**: on a `gqlError` from `refundCreate`, re-read (`settled`, shared with the userErrors path); nothing owed is success without an amount; still owed stays the failure. Fake gained `PartialAnswer` (effect + data + error) and `FailGraphQL` (error, nothing performed). `TestRefundThatTookEffectButAnsweredWithAFieldErrorIsAckedWithoutAnAmount` failed before the fix (`ack {... Error:...}, shop refunded 1620`), passes after. |
| 3 | nit | A gateway refund still pending may read `totalRefundedSet` 0 and be acked as 0. Safe: the next op's cap re-reads the order's total. | Accepted; added to the README "Verify first" row for the dev-store run. |

Checked and sound (reviewer): no int64 overflow (all three inputs bounded to
999,999,999,999); cap math matches 8.4 incl. `now < before` and absent
baseline; read-before-mutation ordering; no PII in logs/acks; nothing deleted;
version bumped.

## TDD re-verification

Reviewer reverted `src/app/writeback.go` alone (compiles: `fTotalRefunded` is
also used by `orderFields`): all 8 new behaviour tests failed for the expected
reason; the R7 test passed (pins existing behaviour, as stated). Restored: all
pass. Fix 1 and 2 re-verified by the orchestrator as above.

## Gate

`gofmt -l .` empty, `go vet ./...`, `GOOS=wasip1 GOARCH=wasm go vet ./...`,
`scripts/build.sh`, `go test ./...`, `scripts/validate.sh`, `scripts/package.sh`,
`scripts/check-workflows.sh`, `scripts/check-version-bump.test.sh`,
`golangci-lint run ./...` (0 issues): all pass. `shellcheck` is not installed in
the cloud container; no shell script changed in this PR (CI runs it).

## Not verified here

No real Shopify call (repo rule). `Refund.totalRefundedSet` is from memory:
real-shop use of `refund.create` stays gated on the README dev-store rows.

**Verdict:** safe to merge.
