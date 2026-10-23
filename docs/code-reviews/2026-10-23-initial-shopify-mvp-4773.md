# Review — initial Shopify channel plugin MVP (ut-docs#4773)

Date: 2026-10-23. Built by Sonnet 5.5 (Builder). Independent review: pending (Reviewer).

## What shipped

The Shopify channel WASM plugin written to `sales-channel-v1`, against a fake
Shopify only (`internal/fakeshop`, test-only, never in the wasm):

- connect test; OAuth callback parse; webhook parse (no tenant context);
- paged import of products + variants, then stock per location; deterministic
  records (second import byte-identical); refusal with a reason code for a
  product over `max_variants` / `max_options`;
- order sweep (`tick`) and `fetch` into the contract's `order` record, no
  customer name, address, email or phone, resumable cursors, line paging;
- outbox: `stock.set` (absolute, compare-and-set, stale re-read, activate
  inactive level), `fulfilment.create` (tracking), `listing.upsert` (variant
  prices); everything else, `listing.unpublish` included, dead-lettered with a logged code;
- conformance suite (copy of the demo's, extended for GraphQL): checks 1, 2
  (adapter side), 7, 9 implemented with broken-adapter self-tests; 3, 4, 5, 6, 8
  are explicit `t.Skip` TODOs naming #4762, #4767, #4771.

## Decisions

1. Paged import, not bulk operations (the JSONL result is on a host the egress
   rule forbids). ADR-0169 section 10 says bulk: an ADR amendment is a
   follow-up card; the ADR is not edited here.
2. Variables only; ids typed and validated (`okGID`) before any request; one
   URL; no `DELETE`; mutations on an allow-list.
3. `listing.upsert` payload is not defined by the contract: the adapter accepts
   `{"variants":[{"external_id","price_minor","currency"}]}` for an existing
   product and nothing else. Needs a contract note (see gaps).
4. Fulfilment is made retry-safe by reading first and carries no
   Idempotency-Key; inventory and price writes carry the op id.
5. `notifyCustomer: false` on fulfilment (a setting later).
6. `qty` = Shopify `quantity`; edit-removed units go into `refunded_quantity`.
7. `conformance/` is a copy of the demo repo's suite (extended), not an import,
   because the demo's check 1 treats every POST as a write.

## Known gaps

- Nothing verified against a real Shopify: GraphQL field/input names
  (`changeFromQuantity`, `ignoreCompareQuantity`, `totalDiscountSet`, ...), the
  requested costs, `@idempotent` placement and the protected-customer-data
  access for `customer { id }` / `shippingAddress { countryCodeV2 }` are written
  from the 2026-10 reference as remembered. Owner action: development-store run.
- Orders older than 60 days are invisible without `read_all_orders` (not in the
  owner's scope list).
- An order edit that removes units may break the contract's total formula
  (`totalPriceSet` is the original total; `currentTotalPriceSet` is unread):
  check on the dev store. More than 5 refunds, 20 refund lines per refund or 200
  lines per order (since round 2: 60) are DETECTED (the query asks for one more than the cap) and the
  order is refused with a logged reason code, never under-counted.
- **Blocker B1 (cloud side, ut-docs#4890):** the cloud's personal-data scan
  refuses the JSON key `name` anywhere in an order, including `lines[].name`,
  which the contract requires. Real orders pass only once #4890 is deployed.
  `TestTheOnlyKeyTheHostsPersonalDataScanWouldRefuseIsLinesName` mirrors the
  host's key list (copied from `channelhost/host.go`) and fails on any key other
  than `lines[].name`.
- Locations beyond 20 are logged (`import.locations_truncated`), not imported.
- Capabilities are `items: pull`, `variants: both`: the adapter cannot create or
  edit an item, so `listing.unpublish` (a draft is an item push) is not done
  (round 2); ut-docs#4774 adds it with `items: both`.
- Every ask has a request, host-call and requested-cost budget sized for the
  host's 30 s deadline (round 2, `src/app/budget.go`); the outbox pulls orders
  at most once per ask after stale ops.
- Multi-location: a `stock.set` without a location is refused when the item is
  stocked at more than one.
- Out of scope (ut-docs#4774): collections, compare-at, images, customers,
  refunds, cancels, product creation / `productSet`, categories.

## Verify first on the dev store

Every Shopify name below was written from memory and has only run against the
fake. All live in `src/app/queries.go`; README has the same table.

| Name / assumption | What failing looks like |
|---|---|
| `changeFromQuantity` vs `compareQuantity`, `ignoreCompareQuantity` | every `stock.set` with `expect` dead-letters (`rejected_by_channel`), or a stale write is not detected |
| `@idempotent(key:)` on `inventorySetQuantities` | GraphQL "directive not defined" on every `stock.set`. The `Idempotency-Key` header is a host-only signal, Shopify ignores it |
| `discountAllocations` is the complete discount list; `totalTaxSet`; `shippingLines`; no duties/tips in the total; `totalPriceSet` is the original total | `order.refused.total_mismatch` / `bad_line` (README has the per-item rows) |
| edit-removed unit of an unpaid order is never restocked here | UT stock stays below Shopify's by the removed units |
| `currentQuantity` excludes refunded units? | `refunded_quantity` off by the refunded units on an unpaid edit-removal plus refund |
| `customer { id }` protected customer data | orders without `customer`, or ACCESS_DENIED on the order query |
| `read_all_orders` absent | orders older than 60 days are invisible |

## Changes after the second independent review (Builder, Sonnet 5.5)

Fixes for the round-2 findings; the findings table below stays the human
reviewer's. Each fix has a test; mutation checks were reported in the Builder's
hand-off, which is not part of this repository, so the tests are the evidence
here.

- **Line title over the cloud's 256-byte limit (HIGH).** `clip` cut at a byte,
  mid-character, and `json.Marshal` turned the stray byte into a 3-byte U+FFFD
  (257 bytes decoded). `clip` now drops invalid UTF-8 and cuts on a rune
  boundary; titles are capped at 250 bytes (`maxTitleBytes`), sku 100, order
  number 40. Tests: `TestClipCutsOnARuneBoundary...` (accented, emoji at the
  boundary, CJK, invalid bytes), `TestLongNonASCIITitlesPassTheHostsScan`, and
  the personal-key mirror now implements the host's 256-byte string rule and the
  "root `lines` only" rule (`TestTheHostScanMirrorHasTheTitleLengthAndRootOnlyRules`).
- **Deadlines (M1).** Only import has 5 minutes; every other ask has 30 s. The
  drain was sized to 600 HTTP calls and the order pull to 20 pages of 800
  requested points: not finishable. New `budget.go`: the drain stops at 40
  requests / 150 host calls (log and cursor writes now counted) / 600 points;
  a tick reads 2 pages of 2 orders; line completion and fetch have a cost cap;
  import stops at 9,000 points. Stops fall between units: nothing performed is
  un-acked, nothing unstarted is lost (a page that does not fit is read again, a
  fetch overflow is left to the tick sweep). Over-stated comment rewritten.
- **`listing.unpublish` (M2).** Removed (op, query, fake handler, tests). An
  unknown or undeclared op is dead-lettered `unsupported_op` and logs
  `outbox.unsupported_op`.
- **Unpaid edit-removed units (M3).** Mapping made explicit and documented next
  to the code; today's behaviour (refunded, not restocked) is pinned by
  `TestOrderEditAndRestockedRefund...` (e4); README verify row added.
- **Total formula (M4).** `mapOrder` recomputes the contract formula and
  refuses with `order.refused.total_mismatch` (counts only); discounts are the
  sum of `discountAllocations`; all shipping lines are summed (mixed rates and
  more than 10 refused); `tax_added_minor` is the order's `totalTaxSet`.
- **LOW.** README row for `totalDiscountSet` replaced; the uncapped
  `RestockedQuantity` mutation is killed by
  `TestRestockedIsNeverMoreThanRefunded...`; an ack failure ends the drain
  (`TestAnAckFailureStopsTheDrain...`).
- Also: order lines over 60 are refused (was 220) to keep the first page of any
  ask inside the cost cap; `qOrders` cost hint 560 for 2 orders.

## Findings

| # | Severity | Finding | Resolution |
|---|---|---|---|
| R1-B1 | Blocker (cloud) | The cloud's personal-key scan refused `lines[].name`, so every order would be rejected | Fixed in ut-cloud#601 (ut-docs#4890); `personal_keys_test.go` mirrors the host scan |
| R1-H1/H2 | High | `changeFromQuantity` (vs `compareQuantity`) and `@idempotent(key:)` are from memory | Cannot be settled without a real shop; isolated in one block, listed under "Verify first on the dev store" |
| R1-H3..H5 | High | Order-level discount codes, truncated refunds, double-counted edit removals | Fixed, with tests |
| R1-M1..M4, L | Medium/Low | No outbox budget, over-declared capabilities, README vs ADR wording, location cap, cursor/regexp/status mapping | Fixed or documented |
| R2-H1 | High | A long non-ASCII line title is cut mid-character and the cloud rejects the order | Fixed (rune-boundary `clip`, 250-byte cap, mirrored in the host-scan test) |
| R2-M1 | Medium | Outbox/tick budgets larger than the 30 s deadline | Fixed (`budget.go`, a few tens of calls per ask) |
| R2-M2 | Medium | `listing.unpublish` contradicts `items: pull` | Removed from the MVP; unknown op dead-letters (#4774 adds it) |
| R2-M3/M4 | Medium | Edit-removed units on unpaid orders; total formula assumptions | Documented, pinned by tests, total mismatch refused and logged |
| R3 | - | The third round of fixes (budget.go, total check, clip) was verified by the builder's tests and the parent's re-run of every gate, NOT by a third independent review | Run a third independent review before this release is ever approved in the marketplace |

## Verdict

Safe to commit as the first commit of a public repo: nothing is proven against real Shopify, no release can be approved without the owner, and the verify-first list is in the README. Not approved for production use until the dev-store run passes.
