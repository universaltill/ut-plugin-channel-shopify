# Review: Shopify product-webhook doorbell,  side (ut-docs#7084)

Cards ut-docs#7084 (parts of #5014, #6821), ADR-0217. Combined review of five worktrees: ut-cloud #1180, plugin 0.8.0 #25, SDK #2756, ut-my-shop #270, ADR/docs #7102. Independent reviewer: Fable (code was Sonnet-built, security-touching), read-only detached run, plus the reviewer's own read of the cloud ingress/worker, the plugin registration, ent drift and the focused suites. Tester report: temp/shopify-realtime/tester-report.md (full suites green, live dev-store drive).

Pre-flight: review record added; ADR-0217 matches the code after the M1 wording fix; ent generated code regenerated in a clean tree = zero diff, schema additive (5 nullable/default columns, one enum value); no real shop name in any diff (fixtures use shop-a); flags default off and verified off; plugin manifest 0.7.3 -> 0.8.0, scopes/permissions unchanged (pinned by test); en.json keys English only (cloud locales carry the 10 keys, Weblate does the rest); help topic and sales-channel-v1 contract text deferred (follow-up cards).

## Findings
1. M1 Medium, ACCEPTED (documented): product dedupe is salted with the unsigned webhook-id header, so a captured signed delivery replayed with fresh ids is not deduped. Cost-only (extra re-reads), bounded by ConnVerified and coalescing, no content trusted. ADR-0217 "What this exposes" and a code comment now say so.
2. M2 Medium, FIXED (plugin): "ours" was any subscription whose path starts /hooks/channel/, so a staging cloud and a prod cloud on one app would repoint each other's subscription. Now the same host AND the same plugin segment (`sameHookOwner`). Test `TestRegisterNeverRepointsAnotherCloudsHookSubscription` (fix reverted: fails, subscription repointed; restored: passes).
3. M3 Medium, FIXED (my.): the sync state refetched only on catalog_version change, so a quiet shop showed a false "Updates may be late". Added a 5-minute re-read of the sync state (paused while the tab is hidden; `channelImportTiming.syncRefreshMs`). Test added.
4. L4 Low, FIXED (cloud): doorbell flag turned off after events were recorded: the sweep would still fetch them. `fetchProducts` now ends them skipped `product_doorbell_off`. Test `TestFlagTurnedOffLaterStopsTheSweepFromFetchingWaitingEvents` (fix reverted: fails; restored: passes).
5. L2 Low, FIXED (cloud): the register ask ran on the main lane right after a re-read and timed out as `failed:busy` (retry after 1 h). It now runs on the webhook lane (short, no catalogue work); the dev's lane pin test updated.
6. Process, FIXED (cloud): five branch files were not gofmt-clean (scripts/ci/verify.sh would fail); formatted, no logic change.
7. L1 Low, DEFERRED: any guest exit on the register ask maps to `unsupported:ask_unsupported` until the digest changes. Follow-up: retry at the daily recheck, or a distinct status for no-handler.
8. L3 Low, DEFERRED: `deferred_ids` from the plugin is ignored; those products wait for the 15-minute re-read while the log says OK.
9. L5 Low, DEFERRED (confirm before deploy): a connection still pinned to plugin 0.7.0/0.7.1 loses the inbound image (gate moved to capabilities.images), flag on or off. Probably none live.
10. L6 Low, DEFERRED: sales-channel-v1.md lacks product_changed and channel.webhook.register.ask; help topic "how fast do changes arrive" absent.
11. Plugin go.mod still pins the old SDK: plugin PR CI `validate` is red until the SDK PR merges and the pin is bumped (builds locally only with the external go.work).

## Verified (personally, plus Fable)
Raw-body HMAC (constant time) runs before parse/record; every forged path is the same 401 with 0 parses/fetches/rows; dedupe by webhook id; nothing from the payload stored (resource ids cleared); no secret/token/ref/callback URL/body in new logs; the plugin has no delete op and no new scope; coalescing claims before the read, a late event leads its own read, failed read releases claims; channel_older strictly older only; LastImportAt written only by FinishImport; catalogueInterval keeps the schedule with product topics declared; unknown ask on 0.7.x => unsupported; old cloud without keys keeps the Last import row; RTL-safe template. Runs: cloud build/vet, channelhook/channelhost/channelsync/channelconn tests; SDK channel tests; plugin vet + tests (with the go.work); my. vitest ChannelDetail, tsc, lint.

## Gaps the Tester listed
my. webhook-state panel and catalogue-view refetch on catalog_version: per design these are follow-ups; they do not block merge (flags are off, the last-synced row and the sync-state refetch ship). Owner should know the panel and "Register again" button do not exist yet.

Verdict: merge after the fixes above; plugin only after the SDK merge and pin bump. Flags stay off at deploy.
