# Review: orders use the SDK's channel.Order (ut-docs#4994)

**Branch:** `refactor/4994-sdk-channel-order` · **Card:** universaltill/ut-docs#4994 (complexity:easy)
**Author model:** Opus 5.5 · **Reviewer:** Sonnet 5.5 subagent, fresh context (a different model from the author)

## What shipped

- `src/app/orders.go`: removed the adapter-local `orderRecord` / `orderLineRecord`
  and their stale comment ("the SDK's channel.Order is thinner"). `mapOrder`,
  `fetch` and `pullOrders` now use `channel.Order` / `channel.OrderLine`. The
  pinned SDK (`v1.1.1-0.20261027011746-4f5504db0242`) already has the full
  record from universal-till#2146, so `go.mod` does not change.
- `src/app/order_wire_test.go` + `testdata/orders.golden.jsonl`: a golden test
  of the exact bytes of the submitted order records (3 order records per tick, for a shop whose prices include tax and one whose prices exclude it).
- `CLAUDE.md`: the orders-record rule now names `channel.Order` and the golden test.
- `manifest.json`: version 0.4.3 → 0.4.4 (src/ changed).

## Verification

- The golden was written by the **old** types on `main` before the swap. It passes
  unchanged after the swap, so the bytes on the wire are identical. The author and the
  reviewer each checked this separately; the reviewer restored origin/main's `orders.go`
  in a scratch worktree.
- The test can fail: a changed `clip` length on `order_number` makes it fail
  (author and reviewer each ran this).
- Field-by-field: names, order, tags and omitempty are identical, including the embedded
  `Identity`. The SDK type adds `Customer *CustomerRef omitempty`. In Shopify nothing
  sets it, so it is always nil and never on the wire (Shopify does not select the
  customer, ut-docs#4920). The demo's local type already had it.
- Gate: `gofmt -l .`, `go vet ./...`, `GOOS=wasip1 GOARCH=wasm go vet ./...`,
  `scripts/build.sh`, `go test ./...` (incl. conformance), `scripts/validate.sh`,
  `scripts/package.sh`, `scripts/check-version-bump.test.sh`, golangci-lint. All green
  (demo lint: one pre-existing SA4000 in `internal/fakeshop/shop_test.go:155`, on
  `main` too, deliberate double-post). shellcheck is not installed in the cloud
  container; no shell script changed, and CI runs it.

## Findings

| Sev | Finding | Outcome |
|---|---|---|
| minor | No review record in the diff | Fixed (this file) |
| minor | Golden failure prints whole blobs, no per-line diff | Accepted: 3–8 short lines |
| nit | Test comment named the removed `orderRecord` as if current | Fixed ("former local … type") |
| nit | WIP commit title | Fixed (squashed with a real message) |
| nit | `UPDATE_GOLDEN=1` fails loudly without `testdata/` | Accepted |

**Verdict:** safe to merge. No user-visible change and no change on the wire.
