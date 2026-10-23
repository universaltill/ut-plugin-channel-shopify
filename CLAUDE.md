# ut-plugin-channel-shopify — rules for working in this repo

The **Shopify sales-channel** adapter (ADR-0169, ut-docs#4773; contract
`ut-docs/reference/contracts/sales-channel-v1.md`). WASM `channel` plugin
(ADR-0009 one-repo-per-plugin, ADR-0001) in Go on the official guest SDK
`github.com/universaltill/universal-till/sdk/plugin/channel` — never
hand-rolled `//go:wasmimport` lines. It runs only in the ut-cloud channel host
(`hosts: ["cloud"]`); a till lists it and refuses to run it. The sibling
`ut-plugin-channel-demo` is the reference adapter; keep the layout in step.

- **No secret, no client id, no real shop data**, ever (ADR-0169 section 5; the
  repo is public). Tokens and secrets in `internal/fakeshop` are invented
  constants. **Never point a test or code at a real store** (not even the
  owner's development store); no credential exists in this repo or its CI.
- **The plugin never holds a credential or its own account.** Network is
  `channel.HTTP` only, one URL: `https://{account}/admin/api/<APIVersion>/graphql.json`;
  the host fills the account in, adds the access token and strips it from
  answers. Never add a second network path, a `DELETE`, a Shopify storage/CDN
  URL, or anything that reads a secret. `.golangci.yml` denies `net/http`, `os`,
  `time` and friends in `src/`.
- **GraphQL variables only.** Query texts are constants in `queries.go`; an id,
  cursor, amount or filter is a variable, never interpolated. Ids pass `okGID`
  (typed `gid://shopify/<Type>/<digits>`) before any request. The tests pin
  both (`security_test.go`).
- **No customer name, address, email or phone** in a submitted `order`, a
  cursor, a log field or an ack. Order queries do not select them; `qContact`
  is the one place that does, for the `contact` answer only. The customer is
  decoded for id and shipping country only.
- **Nothing is ever deleted.** No delete/destroy/remove/cancel/refund operation
  text (allow-list of mutations in `security_test.go`). `listing.unpublish` is not
  done (items is `pull`; ut-docs#4774).
  "Already done" (already fulfilled) is success.
- **Records are deterministic** (`external_version` = Shopify's `updatedAt`) so
  the host's idempotency gives "zero changes" on a second run. No clocks.
- **Money** is integer minor units by string arithmetic (`money.go`); a
  mismatching currency is refused, never converted.
- **Mutations**: a write that is safe to repeat carries the op id as
  `Idempotency-Key` header (a signal to the HOST that a re-send is safe;
  Shopify ignores it) and (inventory) as the `@idempotent` variable; the
  fulfilment write carries no key and is made retry-safe by reading first.
- **Per-ask budgets** (`src/app/budget.go`) keep an ask inside the host's 30 s
  deadline (import 5 min) and caps (2,000 HTTP, 5,000 host calls, 20,000
  records); every request carries a `cost_hint` under Shopify's 1,000 points.
  Stops fall between units of work, a started op is always acked, an ack
  failure ends the drain, a budget-limited ask resumes from its cursor. Count
  every host call (`hostLog`, `cursorSet`), never call `channel.Log` or
  `channel.CursorSet` directly.
- **Text in a record goes through `clip`** (rune boundary, invalid bytes
  dropped): the cloud refuses a line title over 256 bytes after JSON decoding.
- **An order is refused, logged, if the contract's total formula does not
  hold** (`order.refused.total_mismatch`), never submitted for the cloud to drop.
- **`AccountSuffix` in `src/app/api.go` equals `channel.account_suffix`** and the
  scopes equal ADR-0169's list (`scripts/validate.sh` checks both).
- **The suite must be able to fail:** every check in `conformance/suite.go` has a
  deliberately broken adapter in `conformance/selftest_test.go`. TODO checks
  name the card they wait for and `t.Skip`; never delete one or make it pass
  vacuously. `conformance/` is a copy of the demo repo's suite plus GraphQL
  extensions: change the two together.
- **Every from-memory Shopify name lives in `queries.go`** (one block, pinned by
  `TestFromMemoryShopifyNamesLiveInOnePlace`); README "Verify first on the dev
  store" lists each with what failing looks like.
- **Field and input names are unverified against Shopify** until the owner's
  development-store run; do not "fix" them from memory without checking the
  Admin API 2026-10 reference.
- **Tests run natively on the SDK `FakeHost`** behind `conformance.Emulator`
  (`*_test.go` are `//go:build !wasip1`). TDD: failing test first.

## Before committing

`gofmt -l .` (no output), `go vet ./...`, `GOOS=wasip1 GOARCH=wasm go vet ./...`,
`scripts/build.sh`, `go test ./...`, `scripts/validate.sh`, `scripts/package.sh`,
`shellcheck scripts/*.sh`, `scripts/check-version-bump.test.sh`. Feature branch;
review record in `docs/code-reviews/<date>-<topic>.md`; then merge.

## Releases

Release = tag `v<version>` matching `manifest.json`, cut by `auto-tag-release.yml`
on merge. **Any PR that changes what ships — `manifest.json`, `README.md`,
`LICENSE`, anything under `src/`, `go.mod`/`go.sum` or `scripts/build.sh` — must
bump `manifest.json`'s `version` in the same PR** (ut-docs#1940), enforced by
`scripts/check-version-bump.sh`. The marketplace signs the release at approval;
the plugin repo holds no signing key.
