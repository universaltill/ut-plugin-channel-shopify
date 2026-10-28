# Review — connect-test location_list matches stock.set locations (ut-docs#5009)

Date: 2026-10-28 · Card: universaltill/ut-docs#5009 (split from #4921) ·
Built by Sonnet (Dev subagent), reviewed by Opus (different model), lane `lane:cloud-41`.

## What shipped

- **Conformance check 10** (`Check10ConnectLocationsAreStockSetLocations`),
  byte-identical in `ut-plugin-channel-demo` and `ut-plugin-channel-shopify`
  `conformance/suite.go`: every active `location_list` id from the connect
  test (contract section 4.1) is pushed a `stock.set` at
  `<StockVariant>@<id>` with `payload.location` (section 8.3) on a fresh
  connection and must be acknowledged and change the channel; an id the list
  does not name must not be accepted and must change nothing. A target opts in
  with the new `Target.StockVariant`; without it the check skips with a TODO.
- **Selftests** (both repos): a listed location stock.set refuses, a channel /
  adapter that ignores `payload.location`, a connect test with no
  `location_list` — each makes check 10 report.
- **Demo adapter 0.2.3:** SDK bumped to the pseudo-version carrying
  `ConnectTestResult.LocationList` (universal-till#2186); the connect test
  answers `location_list` (okID ids, name clipped to 120, `active` defaulting
  to true); `stock.set` takes `<variant>` or `<variant>@<location>` plus an
  optional `payload.location`, refuses two differing locations
  (`invalid_payload`), sends the location in the body and maps the fake
  shop's `unknown_location` 404 to `not_found` with nothing written; the
  stale-expect report uses the variant id, not the composite id. README row
  updated.
- **Shopify:** `conformance/` only; `src/` unchanged — the 0.4.3 adapter
  already passes check 10.

## Findings

| # | Severity | Finding | Outcome |
|---|---|---|---|
| 1 | low | Shopify's "ignores payload.location" selftest models a channel that files every location under its first one, not a broken adapter (the adapter's stock path is unexported and fed by the host). It still proves step 3 can fail. | Accepted; comment on the type says so. |
| 2 | info | The unlisted id is `first + "9"`, extended once more if listed — enough for one or two seeded locations. | Accepted. |

No security-relevant change: ids still pass `okID`/`okGID`, the location
travels in a JSON body, never in a URL; no new network path, no credential.

## Verified

- TDD re-verified by the reviewer: with the old `src/app/outbox.go`,
  `TestStockSetAtAnUnknownLocationIsNotFoundAndChangesNothing` and suite check
  10 fail; restored, both pass. Dev showed each selftest fails with its branch
  of check 10 disabled, in both repos.
- Gates (both repos): `gofmt -l .` empty, `go vet` native and wasip1,
  `scripts/build.sh`, `go test ./...`, `scripts/validate.sh`,
  `scripts/package.sh`, `shellcheck scripts/*.sh`,
  `scripts/check-version-bump.test.sh` (+ `check-workflows.sh` in Shopify).

Verdict: safe to merge.
