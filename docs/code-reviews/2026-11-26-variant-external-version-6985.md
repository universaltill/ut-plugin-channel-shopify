# Review: Shopify inbound sync, outcomes and resolve results (plugin)

Cards ut-docs#6985 and #6734, ADR-0202 amendment 2026-11-26. Combined review of the four worktrees (ut-cloud, plugin 0.7.3, ut-my-shop, ut-docs). Independent reviewer: Opus 5.5 subagent (code was Sonnet-built), plus the reviewer's own read of every diff. Tester report: tester-report.md (all suites green, live dev-store drive).

Pre-flight: review record added; ADR-0202 amended (ADR-0007; adr index unchanged, no new ADR); contract note sales-channel-v1 s7 `variants[].external_version` additive; help topic deferred -> follow-up card (owner-visible Review/Import screens changed); i18n: ut-cloud has no locales/en.json and no new cloud keys (cloud sends English error text, my-shop strings are in src/i18n/channelStrings.ts, Weblate does the rest, ADR-0160); ent column `channel_sync_logs.outcomes` nullable JSON (safe on existing rows, read as []); plugin manifest 0.7.2 -> 0.7.3, marketplace publish is the owner's step.

## Findings
1. Medium, FIXED (ut-cloud): a new row skipped for `tax_basis_unknown` (createChannel) and a `channel_link_taken` row were counted but never listed in the outcomes. Added `noteReview` on both paths; test `TestNewRowSkippedForTaxBasisIsListed` (reverted fix: fails with empty outcome; restored: passes).
2. Medium, DEFERRED (card): the tick/webhook path (`channelsync/sink.go` applyNow) discards `res.Outcomes`, so a channel-only change applied there is listed nowhere (it was equally unlisted before this change; the import/drift-check path is now complete). Backlog card to give tick runs the same outcomes on their sync-log row.
3. Medium, ACCEPTED (design): a channel-only price/active change now applies whatever the times (ADR-0202 amendment ii). A reconcile that read the channel before an owner edit was acknowledged could write the older channel price back. Gating on item time would bring back the original bug (stamp() = now at every in-sync reconcile). Window is narrow; the overwrite is listed as `applied` with before/after.
4. Low, DEFERRED: extra barcodes beyond the first and image Review entries carry no outcome line. Same card as 2.
5. Process: ADR amendment states the owner approved item (ii) on 2026-11-26; artefacts only show "recommend, mark owner OK". The orchestrator must confirm before merge.
6. Note: outcomes list is the first 100 in apply order, so on a large first import applied rows can crowd out review rows (counts line still shows the review total).

## Verified
Price minor units (94995 -> 95195), 100-entry cap counted once across chunks, outcomes only for committed chunks, resolve `already_equal` / `channel_price` moves the listing only, `catalog_master_till` 409 and `review_value_refused` 422, cloud JSON matches my-shop parsing (channels.ts, channelStock.ts), no secret or PII beyond the shop's own item names (30-day sync-log retention). go build, go vet, go test catalogimport, channelsync, api: green.

Verdict: green. Plugin side: variants[].external_version added additively via outVariant (no custom MarshalJSON on the SDK type, so Variants are not shadowed away; test app_test.go:499), "image" not added to push_fields. Marketplace publish of 0.7.3 is the owner step. Cloud-side findings are in the ut-cloud record.
