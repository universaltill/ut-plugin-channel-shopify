# ut-plugin-channel-shopify

The **Shopify sales channel** for Universal Till (`com.universaltill.channel-shopify`).
It connects a Shopify store to Universal Till: it imports the store's products
and variants, sends price changes and stock to the store, brings its orders in
and reports shipments with tracking, cancels and refunds back to Shopify.
Nothing in the store is ever deleted.

It is a signed **WASM plugin of type `channel`** that runs only in the ut-cloud
channel host, never on a till (ADR-0169, contract
`ut-docs/reference/contracts/sales-channel-v1.md`, card ut-docs#4773). It is the
second adapter after the demo channel (`ut-plugin-channel-demo`) and uses the
same layout, the same SDK (`github.com/universaltill/universal-till/sdk/plugin/channel`)
and the same conformance approach.

**Status: MVP. First run against the owner's development store on 2026-10-24** (ut-docs#4920): the plugin's query texts were sent straight to the real Admin API 2026-10 (not through the cloud host) and connect, variant price push, stock set, fulfil, refund and cancel were run on test data. Rows below marked *verified* passed there; the rest still ran only against `internal/fakeshop`, which now refuses what the real API refused (`internal/fakeshop/realapi.go`).
Everything in this repository is invented data.

## What it does

| Ask | Behaviour |
|---|---|
| `channel.connect.test.ask` | Reads the shop name, granted access scopes, locations and counts of products and orders. Writes nothing. |
| `channel.oauth.callback.ask` | Parses the install parameters the host already verified; checks the shop domain; returns account and scopes. The host does the code exchange and the HMAC check; the plugin never sees a token or a secret. |
| `channel.webhook.ask` | Parses a webhook the host already verified: topic, shop domain and webhook id from the headers, the order's global id from an order webhook. GDPR topics: the body is not read. No tenant context. |
| `channel.import.ask` | Pages through products with variants (Admin GraphQL, cursor paging), then through every location's inventory levels. Submits `listing` and `stock_observed` records. Resumable through cursors; records are deterministic (`external_version` is Shopify's `updatedAt`) so a second import is byte-identical and the host reports zero changes. |
| `channel.fetch.ask` / `channel.tick` | Re-fetch named orders / sweep orders updated since the `orders.since` cursor and submit contract `order` records with **no customer name, address, email or phone**. |
| `channel.contact.ask` | Fetches an order's contact details live for display; never stored, submitted or logged. |
| `channel.outbox.ask` | `stock.set` (absolute, compare-and-set on `expect`, inactive levels activated, a stale `expect` reports the shop's value and pulls recent orders), `fulfilment.create` (shipped with carrier and tracking), `order.cancel` and `refund.create` (below), `listing.upsert` (one variant's price and the product's `productType` from the category: the op's external id is the Shopify VARIANT id, as the cloud sends it; the variant is read first, then its own product is updated; a price that already matches is acked without a write; a zero or negative price is refused `invalid_price`, another currency `currency_mismatch`, an unknown variant `not_found`, all non-retryable; other fields are ignored without error and logged once per ask, on success only, as `listing.fields_ignored`; the ack keeps the variant id; an upsert without an external id is a create and is refused with the non-retryable `create_not_supported`, nothing sent). Anything else is dead-lettered with `unsupported_op` and a logged `outbox.unsupported_op`: a delete, and `listing.unpublish` (a product draft is an item push: card ut-docs#4774). Nothing is ever deleted. |

The plugin has **no account name, no credential and no network of its own**.
Its only network path is `channel_http` to one URL,
`https://{account}/admin/api/2026-10/graphql.json`; the host fills in the
connection's verified account and adds the access token.

## Design decisions

- **GraphQL variables only.** Every query text is a compile-time constant; ids,
  cursors, prices and filter strings travel in `variables`. Ids are validated by
  resource type (`gid://shopify/Order/<digits>`) before they are used.
- **Paged import, not Shopify bulk operations.** ADR-0169 section 10 asks for
  bulk operations on a large import. The plugin does not use them because a bulk
  operation's result is a JSONL file on Shopify's storage host, which the
  contract's egress rule (`https` to the account host only) forbids the plugin
  to fetch. This differs from the accepted ADR on purpose; an ADR-0169
  amendment (bulk result fetched by the host, or paging accepted as the MVP
  path) is a **follow-up card**, and the ADR is not edited here. Page sizes keep
  the requested query cost under Shopify's 1,000 point limit; every call passes
  a cost hint for the host's pacing.
- **Money** is integer minor units by string arithmetic (no float), per currency
  exponent. A price with more decimals than the currency has is refused, not
  rounded. A price pushed in another currency than the shop's is refused.
- **Orders**: `qty` is the quantity as ordered; a unit removed by an order edit or
  refunded shows only in `refunded_quantity` (and `restocked_quantity` when it
  went back to stock). `total_minor` is Shopify's `totalPriceSet`, and the
  adapter recomputes the contract's formula for every order (lines, all
  shipping lines, `tax_added_minor`): an order whose numbers it does not
  explain (a duty, a tip, an edit that changed the original total, a rounding
  gap) is refused with the logged `order.refused.total_mismatch` (counts only),
  never submitted for the cloud to reject silently. A line's discount is the
  sum of its `discountAllocations`, so an order-level discount code (allocated
  across lines) is inside `discount_line_minor` and `order_discount_minor` is 0.
  Shipping is the sum of all shipping lines (differing tax rates:
  `order.refused.mixed_shipping_tax`; more than 10: `too_many_shipping_lines`).
  On a tax-excluded shop `tax_added_minor` is the order's own `totalTaxSet`,
  not the sum of per-line taxes. A line list longer than 60 lines is refused
  (`too_many_lines`), never truncated. Text is cut on a rune boundary: a line
  title is at most 250 bytes of valid UTF-8 (the cloud refuses over 256 bytes
  after decoding), invalid bytes are dropped. An edit-removed unit of an unpaid
  order is `refunded_quantity` but never `restocked_quantity` (no refund line
  says it went back; the stock observation of the next import or tick corrects
  the count).
  A unit is counted once in `refunded_quantity` (the larger of the refund lines
  and the edit-removed units, never the sum). The query reads at most 5 refunds
  and 20 refund lines each; it asks for one more, and an order that overflows is
  refused with the logged reason `too_many_refunds`, never under-counted. A
  payment state that is not paid (`PARTIALLY_PAID`) is `pending`; `EXPIRED` is
  `voided`.
- **Capabilities**: `items: both`, `variants: both`, `stock: both`,
  `orders: pull`, `fulfilment: push`, `cancel: push`, `refunds: push`. `items`
  is `both` because the cloud queues `listing.upsert` only for push/both: with
  `pull` a Universal Till price edit would never be queued and could not reach
  Shopify. What the adapter then does with it: the cloud's external id for an
  imported item is the Shopify variant id, so an upsert reads that variant
  (and its product), then pushes the one variant's price (another currency is
  refused, never converted; a zero or negative price is refused as
  `invalid_price`; a variant that no longer exists is `not_found`). The item's
  category fills the product's `productType` (the leaf of `category_path`, cut
  to 60 characters) ONLY when the product has no category at all in Shopify (no
  type, no standard category, no collection): the cloud sends the category on
  every upsert, so overwriting would undo the merchant's own change at the next
  price edit. Never a collection, never the standard category, never an
  overwrite; an empty path writes nothing. A refused type write is logged
  (`listing.category_rejected`) and does not fail the price push. Sibling
  variants share the product's type (the first push fills it). Changing the
  category of an already-categorised product waits for the cloud to send the
  category only when it changed (proposal on ut-docs#4942). Title,
  description, status, sku, barcodes, compare-at and image are ignored (acknowledged as success;
  `listing.fields_ignored` is logged once per ask, for successes only).
  `listing.unpublish` is not done (`unsupported_op`, no mutation). Only the
  rows of "Verify first on the dev store" marked verified have run against a real store.
- **Known limits**: an item the owner published from Universal Till (no
  external id yet) is a create, which the adapter cannot do (ut-docs#4774): it
  is refused with the non-retryable `create_not_supported`, nothing is sent to
  Shopify, and today it shows only as a failed operation.
- **Per-ask budgets** (`src/app/budget.go`). The host gives every ask but import
  a 30 second deadline (import 5 minutes) and kills a run at it. The budgets are
  call-count and Shopify-point accounting **inside the plugin**; the host does
  **not** pace requests by `cost_hint` yet (ut-cloud `channelhost/egress.go`
  stores the hint and never reads it; it only retries a throttled request with
  back-off, 500 ms doubling, six attempts, 7.75 to 15.5 s, then dead-letters
  it). So wall time is bounded by the request count times each request's
  latency, and a throttle the back-off cannot clear ends the ask with an error
  that leaves the cursor or the outbox untouched; the next ask resumes. A
  ut-cloud card tracks host pacing by cost hint. Every stop is between units of
  work: the outbox drain stops starting operations after 40 requests, 150 host
  calls (logs and cursor writes included) or 600 points; the order pull that
  follows a stale `stock.set` (once per ask) adds at most 2 pages of 2 orders,
  and no ask requests more than about 1,800 points. A started operation is
  always acknowledged; one not started stays pending. An ack failure ends the
  drain (a repeat would perform the same op again). A tick reads at most 2
  pages of 2 orders per ask and resumes from `orders.after`: a page that would
  not fit the cost cap is read again next ask, never skipped. **Until the dev
  store run raises those numbers (below), a big shop's first sweep is slow**:
  4 orders per 15 minute tick, about 62 hours for 1,000 orders. Nothing is
  lost, it only lags. A fetch takes up to 250 ids (the host's cap), reads what
  its budget allows and leaves the rest to the next tick sweep
  (`fetch.deferred_to_tick`). Import stops near 9,000 points per ask and
  resumes from its cursor.
- **Locations**: an import reads the first 20; a shop with more logs
  `import.locations_truncated`.
- **Fulfilment** reads the order's fulfilment orders first and fulfils the open
  ones with the tracking details, one `fulfillmentCreate` per location (Shopify
  refuses a fulfillment spanning locations). If one location is refused the
  others are still fulfilled, the op is acked failed, and a retry re-reads and
  sends only what is still open. An order Shopify already shows as fulfilled
  (by us earlier, or by the shop in its own admin) is success. The customer is
  not notified by this call (a setting is future work).
- **Cancel** (`order.cancel`) reads the order first. An already-cancelled order
  (by us earlier or by the shop) is success; one whose fulfilment has begun
  (`FULFILLED`, `PARTIALLY_FULFILLED`, `IN_PROGRESS`) is refused for good as
  `order_fulfilled`, with no mutation sent. The contract's reason maps to
  Shopify's (`customer_request` CUSTOMER, `out_of_stock` INVENTORY, `fraud`
  FRAUD, `other` OTHER); `restock` is the op's flag; the buyer is **not**
  notified; the money is refunded (`refund: true`, which is what the cloud's
  confirmation assumes: every unit refunded); the staff note is a constant.
  Shopify runs a cancel as a job, so success is acknowledged only when the order
  **shows cancelled**: a job not done in the answer is polled once (`job(id:)`);
  still running is a retryable `cancel_pending` (the retry reads first and finds
  it cancelled), a job that ended with the order not cancelled is
  `cancel_failed` (non-retryable), and a user error saying a cancel is already
  in progress is `cancel_pending` too, never a dead letter. No
  `Idempotency-Key` is sent: a re-sent cancel would be a user error hiding the
  first one's success; a lost answer is retryable and the retry finds the order
  cancelled or pending.
- **Refund** (`refund.create`) is by quantity. It reads the order first (its
  currency, cancellation, fulfilment, location and what each line already shows
  refunded): if every named line is at least at its `refunded_to` it is success
  (our earlier attempt, or the shop's own refund); otherwise it refunds only
  the delta still owed (never more than the line's `quantity`), then asks
  Shopify what those units come to (`suggestedRefund`) and **refuses, before
  sending anything, an amount above the cap** (`refund_exceeds_ceiling`,
  non-retryable). The cap is `max_amount_minor` less any money refunded in the
  Shopify admin since the op was queued: `max_amount_minor - max(0,
  totalRefundedSet now - refunded_before_minor)`, never below 0 (contract
  section 8.4). An op without `refunded_before_minor` (queued before the cloud
  sent it) uses a baseline of 0, the conservative choice; a value outside
  0..999999999999 is `invalid_payload`; an unreadable or foreign-currency
  refunded total is `rejected_by_channel`, before any mutation. After a
  successful `refundCreate` the money the refund moved (`refund.totalRefundedSet`)
  is acked as `refunded_amount_minor`; if the answer cannot be read the refund
  is acked without it (the cloud then counts the ceiling) and `refund.amount_unread`
  is logged. The "already refunded" and race-then-reread successes carry no
  amount; so does a `refundCreate` answered with a GraphQL error whose re-read
  shows nothing owed (the refund was made, a field of the answer failed), which
  is never dead-lettered. A currency other than the shop's is `currency_mismatch`. A
  line is restocked only when every owed unit of it is to go back: an
  unfulfilled order restocks as `CANCEL`, a fulfilled one as `RETURN` to the
  one location it shipped from (`location_required` if there are several, never
  a guess), a part-fulfilled one is refused `restock_not_supported`. The op id
  is sent as Shopify's own `@idempotent` key and as the host's
  `Idempotency-Key`; exactly-once across attempts rests on the read. Extra
  payload fields (`unit_price_minor`, `discount_line_minor`, a reported-refund
  baseline) are tolerated, not required; Shopify works the money out. No
  customer data is read, sent or logged; the note is a constant.
- **Out of scope here** (ut-docs#4774): editing collections or the standard category, pushing compare-at
  (the contract's upsert does not carry it), cost (`unitCost`, no contract field), images, customers, product creation, `productSet`.

## Verify first on the dev store

These Shopify names and behaviours were written from the Admin API 2026-10
reference as remembered and have only run against `internal/fakeshop`. The
names are defined once, in `src/app/queries.go` (a correction is one line),
and a test keeps them there. First run on the development store, in this order:

> **Do not queue real stock ops on a shop until the first two rows pass:** a
> schema error there dead-letters every `stock.set` non-retryably (log code
> `outbox.graphql_error`).
>
> **Do not queue `order.cancel` or `refund.create` on a real shop until the
> `orderCancel`, `job`, `suggestedRefund` and `refundCreate` (+ `@idempotent`)
> rows pass on a dev-store order:** they move customer money, and every name in
> them is from memory.

| Name / assumption | Where | What failing looks like |
|---|---|---|
| *Verified 2026-10-24.* `productVariant(id:) { id product { id productType category { name } collections(first: 1) { nodes { title } } } price }` returns the variant's own product, the product's category fields and the price as a decimal string (the price push reads it first, because the cloud's external id is the variant id) | `mVariantLookup`, `qVariantPrice` | Edit the price of an imported item in Universal Till and wait for the sweep: failing looks like the op dead-lettering `not_found` for a variant that exists, or every price op dead-lettering `rejected_by_channel` with a logged `outbox.graphql_error`. Also check a second identical push makes no change (the read sees the same price and sends no write). |
| *Verified 2026-10-24.* `productVariantsBulkUpdate(productId:, variants: [{id, price}])` takes a bare decimal `price` and changes only that field (compare-at is untouched) | `qVariantPrices` | Edit the Universal Till price of an imported product (one that came from Shopify), wait for the sweep, then read the variant price in Shopify (admin or `productVariant(id:) { price compareAtPrice }`). Failing looks like the op dead-lettering (`rejected_by_channel` with a logged `outbox.graphql_error`, or `currency_mismatch` when the shop currency differs from the till's) or the price not changing. Also check the compare-at price: the adapter does NOT refuse a pushed price above the variant's compare-at, so look in the admin for a product that now shows a nonsensical "sale" (compare-at below the price) and decide whether the shop should clear it, and that every real push logs `listing.fields_ignored` once per ask (the cloud always sends name, description, status, sku and barcodes; the count is of those content fields only, never of structural keys) while the price still changes. A payload that says `tax_included: true` while the shop's prices exclude tax dead-letters `tax_basis_mismatch` and writes nothing. An item published from Universal Till (no Shopify id) must show a `create_not_supported` dead letter and nothing new in Shopify. |
| *Verified 2026-10-24.* `changeFromQuantity` is REQUIRED on every quantity of `inventorySetQuantities` (an integer is a compare-and-set, an explicit `null` is no compare); there is no `ignoreCompareQuantity` field; `@idempotent(key:)` is accepted; `inventoryActivate` also needs `@idempotent`. A key is remembered per mutation: the same key with different input is refused (`IDEMPOTENCY_KEY_PARAMETER_MISMATCH`), so an activation uses `<op>:activate` and the set of a level that was not held uses `<op>:new` | `fCompareField`, `keyActivate`, `keyNew` | Failing looks like every `stock.set` rejected (INVALID_VARIABLE / INVALID_FIELD_ARGUMENTS), an activation refused with BAD_REQUEST, or a redelivered op dead-lettered `rejected_by_channel`. A stale value gives `CHANGE_FROM_QUANTITY_STALE`. |
| *Verified 2026-10-24.* `@idempotent(key:)` on `inventorySetQuantities` | `dIdempotent` | A GraphQL error "directive not defined" on every `stock.set`. The `Idempotency-Key` header is **not** a Shopify feature: it only tells the host the POST may be re-sent. |
| *Verified 2026-10-24 (read-only).* `Product.productType` (String), `Product.category { name }` (TaxonomyCategory, an object: a bare `category` is refused) and `collections(first: 1) { nodes { title } }` (needs `first`) are read on import and by the price push; the ONE category is `productType`, else the standard category's name, else the first collection, else none (ut-docs#4944). The dev store's page of 4 products (few variants) had a requested cost of 59; the 700 hint stays the upper bound for 50 variants each | `fCategory`, `gqlCategorised.single` | Import with an empty Category column for products that have a type, a standard category or a collection, or every import failing with a schema error. Check one product with a type, one with only a standard category, one with only collections, one with nothing; and that a product in two collections imports the first only. |
| *Verified 2026-10-24 (read-only).* `ProductVariant.compareAtPrice` is a decimal string or null | `fCompareAt` | The compare-at price missing on an item that has one in Shopify. |
| `productUpdate(product: ProductUpdateInput!)` with `{id, productType}`; its `userErrors` are `UserError` (`field`, `message`, no `code`). **Schema-verified by introspection only; no mutation has been run** | `mProductType`, `kProductType`, `keyType` | Change the category of an imported item in Universal Till and wait for the sweep: failing looks like `rejected_by_channel` with a logged `outbox.graphql_error`, or the product type not changing, or a collection changed (it must never be). Needs `write_products`, which the app holds. Run this on the dev store before a real shop's first category fill. |
| `Refund.totalRefundedSet { shopMoney { amount currencyCode } }` (a `MoneyBag`) on `refundCreate`'s `refund` is the money that refund moved; `Order.totalRefundedSet` is the order's refunded total (this one already verified on the orders read) | `fTotalRefunded`, `mRefundCreate`, `qRefundState` | Do a refund of one unit on a dev-store order: failing looks like a GraphQL schema error on every `refund.create` (`rejected_by_channel` with a logged `outbox.graphql_error`), or the op acked WITHOUT `refunded_amount_minor` and a `refund.amount_unread` log (answer missing or in another currency). Also refund 5.00 in the admin after queueing an op with `refunded_before_minor` 0 and check a refund above `max_amount_minor - 5.00` is refused `refund_exceeds_ceiling`. With a gateway whose refund stays pending, check what `refund.totalRefundedSet` reads (0 is acked as 0; the next op's cap re-reads the order total, so this is safe, but note it). |
| `discountAllocations { allocatedAmountSet }` on a line is the COMPLETE list of discounts (the line's own and its share of an order-level code) | `fLineAllocations` | `order.refused.total_mismatch` or `bad_line` for an order with a discount code. Check one order with a line discount and one with a code. |
| `totalTaxSet` is the order's tax and, with prices excluding tax, `totalPriceSet` = lines after discount + shipping + `totalTaxSet` | `fOrderTax`, `mapOrder` | `order.refused.total_mismatch` on every tax-excluded order (a cent off from rounding, or duties/tips in the total). |
| `shippingLines(first:)` `discountedPriceSet` (ex tax) lists every shipping line | `fShippingLines` | `order.refused.total_mismatch` on an order with two shipping lines; `mixed_shipping_tax` when their rates differ. |
| Duties and tips are not in `totalPriceSet`, or the formula refuses them | `mapOrder` | `order.refused.total_mismatch` for an order with a tip or duties; decide with the cloud whether the contract needs a field for them. |
| `totalPriceSet` is the ORIGINAL total (before edits), matching `quantity` as ordered; an edit that adds or removes units does not change it | `mapOrder` | `order.refused.total_mismatch` on an edited order (then the contract formula needs `currentTotalPriceSet` with current quantities, a design change). |
| `currentQuantity` = quantity minus every unit an edit removed AND every refunded unit | `mapOrder` (`max(refunded, removed)`) | `refunded_quantity` too low (an edit-removal plus a separate refund) or too high. |
| An edit-removed unit of an UNPAID order has no refund line, and Shopify restocks it (or not) by its own rule | `mapOrder` | `restocked_quantity` stays 0 for it by design. Failing looks like Universal Till stock staying below Shopify's by the removed units after the next import/tick (no stock observation corrects it): then the mapping needs the edit's restock choice. |
| *Verified 2026-10-24.* `customer` on an order needs `read_customers`, which the app does not hold, so no order query selects it; orders arrive without a `customer` ref (the contract makes it optional). The contact answer takes its name from the shipping address | `orderFields`, `qContact` | Failing looks like every order read answering ACCESS_DENIED. Add the scope deliberately before ever selecting `customer` again. |
| `read_all_orders` is not in the scope list | `manifest.json` | Orders older than 60 days are invisible: an import of history stays empty. |
| `refunds(first: 6)` / `refundLineItems(first: 21)`, `locations(first: 21)` cost and shape | `queries.go` | Throttling, or `order.refused.too_many_refunds` for ordinary orders. |
| Order query cost and page size | `orderPage`, `maxOrderPages` (`budget.go`), `Cost:` in `queries.go` | Read `extensions.cost.actualQueryCost` of one `Orders` query, then raise `orderPage`/`maxOrderPages` and lower the cost hint. Until then a first sweep of a big shop is slow (4 orders per tick). |
| `assignedLocation { location { id } }` on a fulfilment order | `fFOLocation` | A GraphQL error on every `fulfil` (non-retryable), or one fulfillment per order not per location (userErrors on multi-location orders). |
| *Verified 2026-10-24.* `orderCancel(orderId, reason, restock, refund, notifyCustomer, staffNote)`, its `job { id done }`, `orderCancelUserErrors` / `userErrors`, and the reasons `CUSTOMER` `INVENTORY` `FRAUD` `OTHER` | `mOrderCancel`, `reason*` | A GraphQL schema error on every `order.cancel` (ops dead-letter as `rejected_by_channel`, log `outbox.graphql_error`); or Shopify requires `@idempotent` on it (then add `dIdempotent` and a `key` variable). |
| *Verified 2026-10-24.* `job(id:) { id done }` answers the cancel job's state; a job has no error field, so a failed job (for example the gateway refund) shows as done with the order NOT cancelled | `rJob`, `mCancelJob`, `confirmCancel` | `cancel_pending` on an order that cancels fine, until the cloud dead-letters the op with that code after about 31 minutes (six retryable failures; staff then Retry or Mark as done) (the job query errors: log `outbox.graphql_error`), or `cancel_failed` for an order that IS cancelled a moment later (cancelledAt lags the job). Cancel an order with a real payment on the dev store and one whose gateway refund fails. |
| orderCancel's user error while a cancel of the same order runs contains `in progress` | `cancelInProgressText` | A retry while the job runs dead-letters as `rejected_by_channel`; read the real message and correct the text. |
| *Verified 2026-10-24.* `refund: true` on a cancel refunds the whole payment | `vRefund` | A cancelled order that shows no refund in Shopify, while Universal Till shows it refunded. |
| *Verified 2026-10-24.* `Order.suggestedRefund(refundLineItems:)` with `amountSet` and `suggestedTransactions { gateway parentTransaction { id } amountSet { shopMoney presentmentMoney } }`; shipping is not refunded by default | `fSuggestedRefund` | Every `refund.create` dead-letters as `rejected_by_channel` (schema error), or a refund is refused `refund_exceeds_ceiling` because the suggestion includes shipping. |
| *Verified 2026-10-24.* `refundCreate(input: RefundInput!)` with `orderId`, `note`, `notify`, `refundLineItems { lineItemId quantity restockType locationId }`, `transactions { orderId parentId kind gateway amount }`, plus `@idempotent(key:)`; `userErrors { field }` | `mRefundCreate`, `k*` | Every `refund.create` fails `rejected_by_channel` with a userError, or a schema error. Then correct the names; check `transactions.amount` is in the presentment currency. |
| `restockType` `RETURN` needs the `locationId`; `CANCEL` is for unfulfilled lines; `NO_RESTOCK` | `restock*` | A refund with restock refused by Shopify (userErrors) on a fulfilled or an unfulfilled order. |
| Lines of a `PARTIALLY_FULFILLED` order cannot be restocked by this adapter | `refundItems` | Staff must refund without restock, or mark the operation done after doing it in the admin. |
| A refund the cloud queued while the shop had refunded some units itself is refunded for the difference only. **Accepted race:** read, then `suggestedRefund`, then `refundCreate` are separate round trips with no compare-and-set; Shopify caps a refund at the refundable maximum, so the worst case is bounded | `owed` | Over-refund: one more unit refunded than staff intended when staff refund the same line in the admin within the same few seconds. Compare the refund in Shopify with `refunded_to` in the op. |
| The cloud's ceiling (floor of the discount share, ceiling of the tax) and Shopify's proportional refund agree to the minor unit | `refundTransactions` | A spurious `refund_exceeds_ceiling` dead letter (fail closed, no money moves) on an odd-priced 3-unit line with a discount code. Try exactly that on the dev store; if it happens the cloud ceiling needs a one-unit allowance per line. |
| The requested costs (`Cost:` per query) | `queries.go` | Shopify's `extensions.cost.requestedQueryCost` is above the hand count: raise the hint. |

## Layout

```
manifest.json            channel block, hosts ["cloud"], permissions ["net:{account}"]
locales/                 en-US.json (the plugin shows a shop owner no text of its own)
src/                     main.go (channel.Run) and app/ (the asks)
internal/fakeshop/       the invented Shopify (GraphQL subset, webhooks, throttling, hostile modes)
conformance/             the suite (Run) and the host emulator it runs an adapter on
scripts/                 build, validate, package, publish, approve
.golangci.yml            import guard: the shipped module may not reach for net/http, os, time ...
```

## Develop

```
go vet ./... && GOOS=wasip1 GOARCH=wasm go vet ./...
scripts/build.sh                 # bin/plugin.wasm
go test ./...                    # unit tests on the SDK's FakeHost + the conformance suite
scripts/validate.sh && scripts/package.sh
```

Tests run natively against `internal/fakeshop` behind the conformance emulator
(egress rules, credential injection and redaction, 429 and GraphQL `THROTTLED`
retry, per-call caps). The emulator is **not** the cloud sandbox; the sandbox's
own limits are pinned by ut-cloud `internal/channelhost`.

## The conformance suite

`conformance.Run(t, conformance.Target{...})` runs the checks of contract
section 6 against this adapter and its own fake server. This repository copied
the suite from `ut-plugin-channel-demo` and extended it for a GraphQL channel
(a read is a POST, so a write is told by the operation; GraphQL deletes by
mutation; cost hints; ids never reach a request body). Implemented: 1 (connect
test reads and writes nothing), 2 (adapter side: a second import submits only
what the first did, byte for byte), 7 (no outbox operation deletes) and 9 (a
hostile shop cannot make the adapter leave `{account}`, leak the credential or
overrun the caps). Checks 3, 4, 5, 6 and 8 are `t.Skip` TODOs naming the card
they wait for (#4762, #4767, #4771); their adapter side is pinned in
`src/app/*_test.go`. `conformance/selftest_test.go` runs the checks against
deliberately broken adapters.

## Release

A release is the tag `v<manifest version>`, cut on merge to `main`. The workflow
packages `dist/<id>_<version>_universal.tar.gz`, uploads it to the marketplace
(`scripts/publish.sh`, placeholder `MARKETPLACE_BASE_URL=https://cloud.universaltill.com`)
and, on the dev marketplace, approves it (`scripts/approve.sh`), which is where
the marketplace **signs** it. Any change to what ships must bump
`manifest.json`'s `version` (`scripts/check-version-bump.sh`).

## Security

No secret, client id or real shop data lives in this repo (ADR-0169 section 5).
The app's client id and secret are entered by the operator in my. and sealed per
tenant in the cloud; the test store's credentials live in Key Vault and private
CI only. Every token and secret in the tests is an invented constant. Never
point a test at a real shop.

MIT licensed, see `LICENSE`.
