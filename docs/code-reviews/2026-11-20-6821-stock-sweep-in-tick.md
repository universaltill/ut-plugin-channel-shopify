# Review: stock levels move from the import to the tick (ut-docs#6821), 0.4.5

Date: 2026-11-20. Branch `fix/6821-stock-sweep-in-tick`. Author: Dev agent
(Sonnet). Independent reviewer: Opus subagent. Tester: separate Sonnet agent.
Design: BA + Architect (Opus). Lane `lane:cloud-41`.

Pre-flight: review record — this file · README updated (import and tick rows,
budgets, locations) · manifest version 0.4.4 → 0.4.5 · contract updated
(ut-docs `reference/contracts/sales-channel-v1.md`, branch
`docs/6821-stock-in-tick`) · ADR n/a (ADR-0169 §1.1 adoption stays one-off) ·
mirrors n/a (no manifest field, no host function) · cloud code n/a (the host
already accepts `stock_observed` in a tick, ModeApply) · follow-up → ut-docs#6825.

## The bug

`channel.import.ask` had a second stage that read every inventory level and
submitted `stock_observed`. The cloud runs the import in ModeCollect and refuses
stock there (`sync.stock_outside_run`; contract section 7). `submit()` only
counts rejections, so the pull did not fail, but every Shopify stock level was
dropped: ut-cloud's "use the channel's stock as the starting count"
(`AdoptChannelStock`) had nothing to count. The stage also spent up to 40 pages
and extra asks against the cloud's ask limit for nothing.

## The fix

- The import submits listings only. A legacy `import.stage` cursor (an install
  upgraded mid-stock-stage) is cleared, and the import restarts from products.
- `src/app/stock.go`: `sweepStock` runs in `channel.tick` after orders, only with
  ask cost budget left, at most 3 pages (150 levels) per tick, resuming through
  `tick.stock.loc` / `tick.stock.after` (saved after every page) and starting a
  new pass after a full one. Records are byte-identical across passes.
- A failed stock page is logged (`stock.sweep_failed`) and does not fail the
  tick: orders are already submitted and their cursors moved.

## Findings (review round 1)

| # | Sev | Finding | Outcome |
|---|---|---|---|
| 1 | MEDIUM | A sweep error failed the whole tick, and the cursor was saved only on a budget stop, so a bad page wedged the pass forever and made the sync log show a healthy channel as failing. | Fixed: cursor after every page; error logged, tick answers. `TestTickSurvivesAStockPageErrorAndResumesPastTheLastGoodPage` (seen red first). |
| 2 | MEDIUM | Unmapped multi-location shop: all locations collapse onto one ut-cloud state key, so LastKnown flaps per pass and feeds a wrong CAS `expect`. Cloud-side, pre-existing, amplified. | Deferred → ut-docs#6825. |
| 3 | MEDIUM | Adopt can be partial while the first pass runs (~150 levels per tick); nothing tells the owner. | Deferred → ut-docs#6825. |
| 4 | LOW | `maxTickStockPages = 4` was unreachable inside the 1,500 cost budget. | Fixed: 3, README, `TestTickStockPageCapIsReachedWhenNoOrdersAreRead`. |
| 5 | LOW | Permanent Shopify load (~930 requested points per idle tick). | Deferred → ut-docs#6825 (measure on the dev store). |
| 6 | LOW | Legacy cursors cleared on every import ask (3 host calls). | Fixed: only when `import.stage` is set. |
| 7 | LOW | `stock.locations_truncated` logged every tick. | Fixed: once per pass, `TestLocationsTruncatedIsLoggedOncePerPass`. |
| 8 | LOW | README said a tick's observation "corrects the count"; it only records the last-known level. | Fixed. |

Fixes re-verified by the orchestrator (gate re-run, `stock.go`/`orders.go` read);
one review round per MODEL-ROUTING (medium).

## Verification

- Tester: nine new tests go red with the fix reverted; full gate green.
- Driven run: the built `bin/plugin.wasm` through ut-cloud's real channel host
  (`channelhost.DemoRigForTest`) and the real `channelsync.Sink`, 20 products,
  2 locations. 0.4.4: import answered `observed:82` and raised
  `sync.stock_outside_run` twice. 0.4.5: import 20 listings, 0 stock, no event;
  four ticks accepted 246 stock records, no refusal.
- Real Postgres: no cloud code changed; the existing `internal/channelalloc`
  observation tests already run on Postgres in ut-cloud CI.
- Known limit (README): orders needing many line pages can use a tick's budget,
  and that tick reads no stock.
