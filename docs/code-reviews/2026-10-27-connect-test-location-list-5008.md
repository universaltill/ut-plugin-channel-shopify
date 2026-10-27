# Review: connect test answers `location_list` (ut-docs#5008)

**Version:** 0.4.2 → 0.4.3. **Contract:** sales-channel-v1 §4.1
(`location_list`, ut-docs#4921). **SDK:** `ConnectTestResult.LocationList`,
universal-till#2186.

## What shipped
- `qConnectTest` now reads `locations(first: 50) { nodes { id name isActive } }`
  instead of `first: 20 { name }`. 50 is the host's keep-50 cap.
  - `isActive` is a from-memory name: it is `fLocationActive` in the
    `queries.go` block, with a README verify row.
  - The cost hint goes from 40 to 70; the estimate is about 57.
- `connectTest` keeps `locations` (names, clipped to 100) and adds
  `location_list`:
  - only ids that pass `okGID(tLocation, …)`;
  - each name through `clip(name, 120)`, which is the host's limit;
  - `active` taken from `isActive`.
- `fakeshop` answers `id` and `isActive`. New helpers set a location's
  reported id or name, or deactivate it.

## Review
Independent review in a fresh context by a different model from the author.
The checks were re-run by the reviewer.

1. **should-fix, fixed.** Shopify's `locations` defaults to
   `includeInactive: false`, so deactivated locations never reach the
   adapter. The fake listed them with `isActive: false`, and a test pinned
   that path, which the real API never produces. The README row also called
   a missing deactivated location a failure.
   - Fix: kept the default. It matches the import's `qLocations` and adds no
     new from-memory argument.
   - The fake now leaves deactivated locations out, the test asserts they
     are in neither list, and the README row says absence is expected.
   - Accepted: with the real API `active` is always `true`. The mapping
     `Active: isActive` is a direct field copy and no test drives `false`.
2. **nit, accepted.** A wrong `isActive` name would fail the whole connect
   test, not just `location_list`. `Location.isActive` is a long-standing
   Admin API field, and the README row names the failure for the dev-store
   run.
3. **nit, noted.** The names-only `locations` grows from 20 to 50, so the
   connect test's stored location count is now min(n, 50). This is harmless.
   Beyond 50 locations the list is cut silently, matching the host cap.
4. **nit, accepted.** The cost hint 70 is plausible (about 57). The README
   asks for a comparison with `extensions.cost`.

**Conformance / demo:** suite check 1 asserts only the account name and
"writes nothing". `location_list` is optional, so neither the suite copy nor
`ut-plugin-channel-demo` changes in step.

**TDD re-verified by the orchestrator:** with the `app.go` hunk reverted,
the three new tests fail with `location_list []`; restored, they pass.

## Verified
`gofmt -l .`, `go vet ./...`, `GOOS=wasip1 GOARCH=wasm go vet ./...`,
`scripts/build.sh`, `go test ./...`, `scripts/validate.sh`,
`scripts/package.sh`, `scripts/check-workflows.sh` and
`scripts/check-version-bump.test.sh`. shellcheck was not run: it is not
installed in the cloud container, and no script changed.

**Safe to merge** once `go.mod` requires the merged SDK commit (no `replace`).
