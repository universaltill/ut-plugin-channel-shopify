# Shopify plugin v0.7.0: description/SKU/barcode push, image.upsert, plain-text description import (ut-docs#6843, #5014)

Pre-flight: walked PREFLIGHT.md: build.sh (wasm), validate.sh, go test, conformance, golangci-lint (tester: 277 pass / 5 skip / 0 fail, lint 0 issues), no secret in logs.

Combined review with ut-cloud: see `ut-cloud/docs/code-reviews/2026-11-23-shopify-finish-6843-6734-6729.md` for findings, revert-and-see results and verdict.

Plugin specifics checked: `image.upsert` takes only a https `fetch_url` (no userinfo, <= 2048), never fetches or logs it, never puts it in an ack; sha/content-type/alt validated before any request; metafield sha idempotency, media reorder best-effort and logged by fixed code; description written as escaped `<p>` only; `NormalizeText` byte-for-byte equal to the cloud's (shared vector tables, cross-check test); SKU via `inventoryItem.sku` and barcode in `productVariantsBulkUpdate`; validate.sh pin moved to the six fields with manifest 0.7.0. No defect found. Accepted: tie/unknown-time writes use the product `updatedAt` (safe direction). Deferred: real dev-store run (media deprecation, extension-less URL), release is an owner/marketplace step.
Verdict: approve.
