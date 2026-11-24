# Review: Shopify manifest rejected by the till validator (ut-docs#6975, root cause 1), 0.7.2

Cause: 0.7.0/0.7.1 declared `channel.push_fields` "image"; the till's `channelPushFields` (internal/plugins/manifest_channel.go) allows only name, description, sku, barcodes, price, compare_at, category_path, so the till refused the whole manifest. ADR-0212 (images) is only Proposed; the till is not widened.

Change: "image" removed from `push_fields`, version 0.7.1 -> 0.7.2, `scripts/validate.sh` pin updated, new `scripts/check-push-fields.sh` (subset-of-till-list check, run by validate.sh and therefore CI). Image code (`image.upsert`) is untouched and dormant: the cloud queues it only for a declared field. Re-adding "image" needs ADR-0212 Accepted, the till widened and released, then `check-push-fields.sh` widened.

Pre-flight: review record (this file) | README + CLAUDE.md updated | manifest 0.7.1 -> 0.7.2 | validate.sh pin updated | test first (check-push-fields.sh red on 0.7.1) | go test, vet, build, validate, shellcheck run | independent Opus review: approve, nits fixed.

Tests: check-push-fields.sh failed on the 0.7.1 manifest (red) and passes on 0.7.2; `go test ./...`, vet, build.sh, validate.sh, shellcheck run.
