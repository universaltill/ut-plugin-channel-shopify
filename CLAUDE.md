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
  is the one place that does, for the `contact` answer only. The `customer`
  object is not selected at all: it needs `read_customers`, which the app does
  not hold (ut-docs#4920).
- **Nothing is ever deleted.** No delete/destroy/remove operation text; the
  mutation allow-list in `security_test.go` is exact (`OrderCancel` and
  `RefundCreate` are the only ones that may name cancel/refund, and only they).
  `listing.unpublish` sets a single-variant product to DRAFT, never ARCHIVED. "Already
  done" (fulfilled, cancelled, refunded to the target) is success.
- **Cancel and refund are READ FIRST, never blind** (`writeback.go`): no
  mutation until the order shows the effect is not there; a refund is the
  delta to `refunded_to`, is refused BEFORE sending if Shopify's suggested
  amount exceeds `max_amount_minor`, refuses another currency, never notifies
  the buyer, and its note is a constant (no customer text). Extra payload
  fields are tolerated, not required.
- **No `order.cancel` / `refund.create` on a real shop until the `orderCancel`,
  `job`, `suggestedRefund`, `refundCreate` (+ `@idempotent`) README rows pass on
  a dev-store order** (money, names from memory). Cancel is acked only once the
  order shows cancelled: job polled once, still running = retryable
  `cancel_pending`, ended uncancelled = `cancel_failed`.
- **Images (code 0.7.0, ADR-0212, ut-docs#6843 S5; DORMANT since 0.7.2, "image" not in push_fields, ut-docs#6975).** `image.upsert` `{cloud_id, external_id, sha256,
  content_type, fetch_url, alt?}`: the plugin hands the cloud's signed `fetch_url` to
  `productCreateMedia(originalSource)` and never sees bytes; only Shopify fetches it, so no egress
  host is added. The URL is never logged or put in an ack. The sha256 of the last pushed image is
  kept in the product metafield `unitill.image_sha256`; same sha and a first media that has not
  FAILED = skip (acked with that media id). Otherwise create the media, store the sha, move it to
  position 0. Old media are never deleted. Bad payload = `invalid_payload`; a payload without any
  image key (old shape) or another kind = `unsupported_op`. Import reads `featuredMedia` as
  `images[0]` (https URL only); the cloud downloads and hashes it.
- **Product doorbell and webhook registration (code 0.8.0, ut-docs#7084, ADR-0217).** `products/create|update|delete` answer the neutral event `product_changed`; the ONLY body field read is the product id (`admin_graphql_api_id`, or the numeric `id` of a delete body turned into a typed gid; `productIDFromBody`). `channel.fetch.ask` routes by gid type: product gids are re-read in batches of 5 through ONE `nodes(ids:)` query (`qProductsByID`, same fields as the import, so a re-read row equals an import row), a null node is skipped, no stock is submitted, and ids over the budget come back as `deferred_ids`. `channel.webhook.register.ask` (`webhooks.go`) ensures the three product subscriptions through `webhookSubscriptionCreate/Update` for the host's callback address: list first, same address = `already`, OUR subscription (same host and same plugin segment under `/hooks/channel/`, `sameHookOwner`) at another address = repointed in place, never a foreign one, **never a delete** (mode `remove` is `unsupported`). The address and Shopify's userError text are never logged or answered; failures are stable codes. Manifest 0.8.0 declares `images: pull` (inbound only; `push_fields` still has no image) and keeps `permissions` and `scopes` unchanged.
- **`items` is `both`; `push_fields` is `["price","name","description","sku","barcodes"]`** (image is NOT declared: the till validator rejects it until ADR-0212 is Accepted, ut-docs#6975, 0.7.2; the image.upsert code stays dormant; description/sku/barcodes 0.6.0, ut-docs#6843, ADR-0202 amendment 2026-11-23) (owner requirement
  2026-11-20, ADR-0202, ut-docs#5014: the name syncs both ways, the later
  change wins; `scripts/validate.sh` pins the list). Beyond the price the adapter
  writes the product title (single-variant products only) and DRAFT/ACTIVE
  status, driven by the cloud's additive payload key `changed: {field: {was, at}}`
  (`listingUpsert`): absent = price only (older cloud); empty = ack with no
  request; only fields in `changed` are written; a field is skipped when Shopify
  already has it or Shopify's value differs from `was` and `updatedAt` is later
  than `at`. One read decides every field (a write bumps `updatedAt`). ARCHIVED is
  never touched; a merchant-drafted product is re-published only when `changed`
  has `active`. description (escaped `<p>` paragraphs via `descriptionHtml`, compared on
  `NormalizeText`; markup in the item text is dropped per line before escaping, and the description is READ as `descriptionHtml`, never `Product.description`, which glues paragraphs: live 2026-11-23, 0.7.1), the variant sku (`inventoryItem.sku`) and the FIRST barcode are
  written the same way (only when in `changed`, never cleared by an empty value,
  never over a later Shopify edit, no request when Shopify already holds the
  value); an op from an older cloud (no `changed`) writes none of them. The cloud queues
  `listing.upsert` only for push/both, so `pull` would leave the variant-price
  push unreachable (ADR-0169 criterion 6; `scripts/validate.sh` pins `both`).
  The op's external id is the Shopify VARIANT id (the cloud's listing id): the
  adapter reads the variant (and its product), pushes that one variant's price
  and acks the variant id unchanged. Price <= 0 is `invalid_price`; `tax_included: true` while the shop's prices exclude tax is `tax_basis_mismatch` (the opposite is a legitimate non-taxable variant); the rest of
  the record is ignored (logged once per ask, successes only, no PII); an upsert
  without an external id is a create: refused non-retryably with
  `create_not_supported`, nothing sent. Never add item creation here without
  ut-docs#4774.
- **Records are deterministic** (`external_version` = Shopify's `updatedAt`) so
  the host's idempotency gives "zero changes" on a second run. No clocks.
- **The orders record is the SDK's `channel.Order`**, which carries the
  contract's full record (universal-till#2146, ut-docs#4994). Never add an
  adapter-local copy; `TestSubmittedOrderBytesAreStable` pins the wire bytes.
- **Money** is integer minor units by string arithmetic (`money.go`); a
  mismatching currency is refused, never converted.
- **Mutations**: a write that is safe to repeat carries the op id as
  `Idempotency-Key` header (a signal to the HOST that a re-send is safe;
  Shopify ignores it) and (inventory) as the `@idempotent` variable; the
  fulfilment write carries no key and is made retry-safe by reading first.
- **Per-ask budgets** (`src/app/budget.go`) are call-count and point accounting
  inside the plugin that keep an ask small against the host's 30 s deadline
  (import 5 min) and caps (2,000 HTTP, 5,000 host calls, 20,000 records). The
  host does NOT pace by `cost_hint` yet (ut-cloud `egress.go` stores it; it only
  retries a throttle with back-off, then dead-letters); wall time is requests x
  latency. Stops fall between units of work, a started op is always acked, an
  ack failure ends the drain, a budget-limited ask resumes from its cursor. The
  order page constants are unmeasured (dev-store row in README). Count every host
  call (`hostLog`, `cursorSet`), never call `channel.Log` or `channel.CursorSet`
  directly.
- **Fulfilment**: one `fulfillmentCreate` per location (fulfilment orders of
  different locations cannot share one).
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
`shellcheck scripts/*.sh`, `scripts/check-workflows.sh`, `scripts/check-version-bump.test.sh`. Feature branch;
review record in `docs/code-reviews/<date>-<topic>.md`; then merge.

## Releases

Release = tag `v<version>` matching `manifest.json`, cut by `auto-tag-release.yml`
on merge. **Any PR that changes what ships — `manifest.json`, `README.md`,
`LICENSE`, anything under `src/`, `go.mod`/`go.sum`, `scripts/build.sh` or `scripts/package.sh` — must
bump `manifest.json`'s `version` in the same PR** (ut-docs#1940), enforced by
`scripts/check-version-bump.sh`. The marketplace signs the release at approval;
the plugin repo holds no signing key.
