# 2026-10-25 — `channel.push_fields: ["price"]` (ut-docs#5014)

## What shipped (v0.4.1)

- `manifest.json` declares `"push_fields": ["price"]`; version 0.4.0 → 0.4.1.
  The cloud (ut-docs#4911) now hashes this channel's listings over price and
  `active` only, so a rename / description / SKU / barcode / compare-at /
  category change no longer re-queues a no-op `listing.upsert` after every
  24-hourly re-read.
- `scripts/validate.sh` pins `push_fields == ["price"]` with the reason.
- README: capabilities paragraph explains the field, why `category_path` is
  not declared, and the one-time re-hash on the first sweep; a "Verify first"
  row for the dev-store check; stale "cloud sends the category on every
  upsert" wording corrected (also in `src/app/outbox.go`'s comment).

## Design decision: `category_path` not declared

The adapter's `productType` write is fill-only (a product with no category in
Shopify). Declaring `category_path` would make the cloud hash UT's category on
the sweep and Shopify's on the re-read; wherever they differ the adapter never
converges, so an upsert would be queued after every re-read — the loop this
card removes. Consequence (documented): a category-only UT edit reaches
Shopify only at the item's next price push, and only for a product with no
category there; pushes do not move the link's category baseline
(`pushedBaseline`), the next re-read does.

## Compatibility

No till or ut-cloud validator decodes strictly (no `DisallowUnknownFields` in
`ut-cloud/pkg/manifest`, `internal/signing`, `universal-till/internal/plugins`);
both `main`s already know `push_fields` and accept `price`, and the signing
canonical manifest carries the field. A cloud without ut-docs#4911 ignores it
(old behaviour, no failure). The channel never runs on a till.

## TDD

`scripts/validate.sh` with the new check failed on the unchanged manifest
(`FAIL: channel.push_fields must be ["price"] …`) and passed after the edit.

## Independent review (Sonnet; author Opus)

Verdict: safe to merge. Ran build, `go test ./...`, validate, shellcheck.
Confirmed the category argument against `catalogimport/channel_apply.go`
re-read hashing and `channelsync/outbound.go` `categoryToSend` /
`pushedBaseline`, and the compatibility claim.

| # | Sev | Finding | Outcome |
|---|---|---|---|
| 1 | minor | `outbox.go` comment / README said the cloud sends `category_path` on every upsert; it sends it when it differs from the link baseline, which a price-only ack never moves. README also omitted that a category never reaches an already-categorised product. | Fixed |
| 2 | minor | Verify-first row checked renames only; add the category fill + baseline step. | Fixed |
| 3 | nit | `active` is always hashed; a UT deactivation queues an `unpublish` that this adapter answers `unsupported_op`. Pre-existing, unchanged by this card (ut-docs#4774). | Accepted |

## Verified beyond automated tests

Nothing live: the dev-store check (README row) is the owner's step after
marketplace approval of v0.4.1, as the card's acceptance states.

## Gate

gofmt, `go vet` (native + wasip1), `scripts/build.sh`, `go test ./...`,
`scripts/validate.sh`, `scripts/package.sh`, shellcheck, `check-workflows.sh`,
`check-version-bump.test.sh` — all clean.
