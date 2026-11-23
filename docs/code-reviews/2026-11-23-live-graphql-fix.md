# Review: live dev-store proof of the 0.7.0 GraphQL, description read fix (ut-docs#6843, #6733, #6734), 0.7.1

Date: 2026-11-23. Branch `fix/6843-live-graphql`. Author: Tester agent (Sonnet).

Pre-flight: review record (this file) | README updated (import line, live-verified paragraph) | CLAUDE.md updated (description read as `descriptionHtml`) | manifest version 0.7.0 -> 0.7.1 | `scripts/validate.sh` pin unchanged (`push_fields` list is the same) | tests first (`desc_roundtrip_6843_test.go` red, then green) | full `go test ./...`, vet, golangci-lint, `scripts/build.sh`, `scripts/validate.sh` run.

## What was proven live

The real adapter handlers behind the conformance emulator, shop side proxied to the dev store (API 2026-10, client-credentials token held in memory only), on a disposable `UT-LIVE-TEST` product (deleted, verified gone). The harness is NOT committed (this repo never points code at a real store; evidence in ut-docs `temp/wt-guards/notes/live-roundtrip.md`).

Every GraphQL shape was accepted by Shopify: `productUpdate` (title, `descriptionHtml`), `productVariantsBulkUpdate` (price, `barcode`, `inventoryItem.sku`), `productCreateMedia`, `metafieldsSet`, `productReorderMedia`. Outbound and inbound matched for title, price, description, sku, first barcode and image; the image is idempotent by sha, a new sha goes to position 0, a FAILED media is repaired by the next push; a repeated identical `listing.upsert` sends no mutation.

## Defect found and fixed

Shopify's plain `Product.description` removes tags with nothing in their place: `<p>Inbound &amp; <em>desc</em></p><p>line two</p>` reads `Inbound & descline two`. The adapter read that field (import and the variant read), so a description pushed as two paragraphs came back as "Line oneLine two": the import delivered changed text to the cloud and the next push compared unequal and rewrote it (ping-pong). The fake had modelled plain text with newlines, hiding it.

- `queries.go`, `catalogue.go`, `outbox.go`: read `descriptionHtml` (NormalizeText turns tags into spaces).
- `textsync.go`: `descriptionHTML` normalises each line before escaping, so markup in item text is dropped exactly as the cloud's NormalizeText drops it; escaped markup would read back as literal `<b>` text and differ again. Contract says no tag from the item is emitted: still true. `TestDescriptionIsEscapedAndNeverCarriesItemMarkup` updated for this.
- `internal/fakeshop`: `htmlToPlain` now behaves like Shopify (glues), `descriptionHtml` served.
- Tests: `TestAMultiParagraphDescriptionRoundTripsAndIsNotRewritten` (red before the fix); the existing `TestContractShapedOpDrivesTheAdapterOnceAndSettles` was also red against the realistic fake.

## Accepted gaps

- A tag split across two lines of the item text is stripped by the cloud (whole text) but not per line here: absurd input, no loop for normal text.
- `productCreateMedia` is documented as deprecated by Shopify in favour of `productUpdate(media:)`; it still works on 2026-10. Move when Shopify removes it.
- Cloud side (the same `NormalizeText` compare) is unchanged and unverified here.
