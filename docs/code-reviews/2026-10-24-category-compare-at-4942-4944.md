# Review: category, compare-at and a safe productType fill (v0.3.0) - ut-docs#4942, #4944

## What shipped
- Import fills `category_path` with ONE category (#4944): `productType`, else the standard category's name, else the first collection, else none; and `compare_at_minor` from `compareAtPrice`. Text is cut to 60 runes (the cloud's category-name limit).
- `listing.upsert` fills `productType` (leaf of `category_path`) via `productUpdate(product:)` ONLY on a product with no category at all in Shopify. Never a collection, never the standard category, never an overwrite.
- `internal/fakeshop/realapi.go` refuses the real 2026-10 shapes learned read-only: bare `category`, `collections` without `first`, `productUpdate(input:)`, `userErrors { code }`, non-`productType` input fields.
- Manifest 0.3.0, README rows, mutation allow-list gains `ProductType`.
- Contract changes (cost, compare-at push, category-on-change, customers) proposed as comments on ut-docs#4942 and #4923; nothing invented in the plugin.

## Verified against the real Admin API 2026-10 (read-only, development store, nothing written)
Query texts for `productType`, `category { name }`, `collections(first: 1)`, `compareAtPrice`, `inventoryItem.unitCost` run clean; `ProductUpdateInput` has `id`, `productType`, `category` (an ID); `productUpdate` takes `product:`; `UserError` has `field`, `message` only. The `productUpdate` mutation itself was NOT run (README row says so).

## Independent review (Opus) findings
| # | Severity | Finding | Outcome |
|---|---|---|---|
| H1 | high | Category over 60 chars fails the whole cloud import row (clip was 250 bytes) | Fixed: `categoryName` cuts to 60 runes, trims after; test `TestCategoryTextIsCutToTheCloudsLimit` |
| H2 | high | Cloud sends `category_path` on every upsert; a write-on-difference rule undoes a merchant's own category change at the next price edit | Fixed by design: fill only a product with no category; proposal to send category only on change filed on #4942 |
| M1 | medium | A refused type write dead-lettered an already-written price | Fixed: logged `listing.category_rejected`, price ack stands; test |
| M2 | medium | Case-sensitive comparison vs the cloud's case-insensitive names | Moot: no comparison any more (fill-only) |
| M3 | medium | Sibling variants share one productType | Accepted, documented in README |
| L1 | low | README cost claim | Fixed wording |
| L4 | low | README named a real prospect; compare-at missing from the ignored list | Fixed |
| L5/L6 | low | JSON tag spelled outside queries.go; fake skips input check when variable is absent | Accepted (same as existing tags) |

## TDD
Tests written first and seen red (empty category, compare-at 0, productType unchanged); fix disabled again after the review and 5 tests failed as claimed, restored green.

## Not done
Cost (`unitCost`), customers, discounts (#4945), images, compare-at push: the contract has no field or the decision is open.

## Verdict
Safe to merge. Pre-commit list green (gofmt, vet incl. wasip1, build.sh, go test, validate, package, shellcheck, check-workflows, check-version-bump.test).
