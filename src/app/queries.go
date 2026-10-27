package app

// Every GraphQL operation the adapter sends. The texts are compile-time
// constants: a value, an id or a cursor is NEVER put into a text, it goes in
// the variables object (contract section 3). queries_test.go pins that, and
// that no operation deletes anything (the contract has no delete).
//
// Field and input names are written against the Admin GraphQL API 2026-10 and
// have only been exercised against internal/fakeshop; the owner's development
// store run (ut-docs#4773 OWNER ACTION) is what proves them against Shopify.
// Costs are hand-counted upper bounds of the REQUESTED cost, kept under
// Shopify's 1,000-point single-query limit, and go to the host as the
// channel_http cost_hint. The host stores the hint and does not pace by it yet
// (budget.go).

// VERIFY FIRST ON THE DEV STORE (README, "Verify first on the dev store"):
// every Shopify name written from memory lives in this block, so a correction
// is one line. None of them has been run against Shopify yet.
const (
	// Every discount taken off a line: the line's own and its allocated share
	// of an order-level code. discountAllocations is Shopify's documented
	// complete source; a line's discountedTotalSet is NOT relied on (it may
	// leave the order-level allocations out). The record's discount is the
	// sum of these.
	fLineAllocations = `discountAllocations { allocatedAmountSet { shopMoney { amount currencyCode } } }`
	// The order-level tax (tax-excluded shops: tax_added_minor is this, not the
	// sum of the per-line taxes, which can be a cent off after rounding) and
	// all the shipping lines of an order (not just the first).
	fOrderTax      = `totalTaxSet { shopMoney { amount currencyCode } }`
	fShippingLines = `shippingLines(first: ` + shippingLinesAsked + `) { nodes { discountedPriceSet { shopMoney { amount currencyCode } } taxLines { rate priceSet { shopMoney { amount currencyCode } } } } }`
	// The compare-and-set field of inventorySetQuantities: required on every
	// quantity; null turns the compare off.
	fCompareField = "changeFromQuantity"
	// Location.isActive: whether the location is in use. The connect test
	// reports it as location_list[].active (ut-docs#5008).
	fLocationActive = "isActive"
	// Shopify's own idempotency directive for inventorySetQuantities. The
	// Idempotency-Key HTTP header is NOT a Shopify feature: it is only a signal
	// to the host that the POST is safe to re-send.
	dIdempotent = `@idempotent(key: $key)`
	// Suffixes that give the two mutations of one stock.set op their own
	// idempotency keys (an op id is at most 128 characters; Shopify allows more).
	keyActivate, keyNew = ":activate", ":new"
	// The location a fulfilment order is assigned to. Shopify refuses a
	// fulfillment whose fulfilment orders sit at different locations, so the
	// adapter sends one fulfillmentCreate per location.
	fFOLocation = `assignedLocation { location { id } }`

	// The category of a product (ut-docs#4944): the merchant's free-text
	// productType, the standard category (an object: only its name is read)
	// and the FIRST collection. collections needs first/last. Verified read-only
	// against the Admin API 2026-10 (README rows).
	fCategory = `productType category { name } collections(first: 1) { nodes { title } }`
	// The compare-at price of a variant (decimal string or null).
	fCompareAt = `compareAtPrice`
	// productUpdate takes product: ProductUpdateInput!; the adapter sets
	// productType and nothing else (never a collection, never the standard
	// category). The payload's userErrors are UserError: field and message,
	// no code.
	mProductType             = `productUpdate(product: $product) { userErrors { field } }`
	kProductID, kProductType = "id", "productType"
	// Suffix that gives the productType write of one listing.upsert op its own
	// idempotency key (the price write uses the op id).
	keyType = ":type"

	// Cancel and refund (ut-docs#4774). The operation roots, the argument and
	// input names, the enum values and the user-error fields of orderCancel,
	// refundCreate and suggestedRefund, all written from memory of the Admin
	// API 2026-10. README "Verify first on the dev store" has a row for each.
	rOrderCancel  = `orderCancel`
	rRefundCreate = `refundCreate`
	rSuggested    = `suggestedRefund`
	rJob          = `job`
	// totalRefundedSet: the money refunded so far on an Order (verified on the
	// orders read) and, written from memory, the money one Refund moved
	// (Refund.totalRefundedSet, README row "Refund.totalRefundedSet").
	fTotalRefunded = `totalRefundedSet { shopMoney { amount currencyCode } }`
	// orderCancel is an asynchronous job in Shopify: the answer says the job
	// was accepted. refund:true gives the buyer the money back (the cloud's
	// confirmation marks every unit refunded); notifyCustomer stays false; the
	// staff note is a constant, never customer text.
	mOrderCancel = rOrderCancel + `(orderId: $orderId, reason: $reason, restock: $restock, refund: $refund, notifyCustomer: $notifyCustomer, staffNote: $staffNote) ` +
		`{ job { id done } orderCancelUserErrors { field message } userErrors { field message } }`
	fCancelUserErrors = `orderCancelUserErrors`
	// The state of the cancel job, read once when orderCancel says it is not
	// done. A Job has an id and a done flag and, as remembered, nothing that
	// says how it ended: a job done without the order cancelled is how a
	// failure shows (the order is read again).
	mCancelJob = rJob + `(id: $id) { id done }`
	// Text (lower case, a substring of the user error's message) of the answer
	// orderCancel gives while an earlier cancel of the same order is still
	// running. Written from memory: the wording is unverified.
	cancelInProgressText = "in progress"
	fUserErrors          = `userErrors`
	// The price push reads the variant first: the cloud's external id for an
	// item is the VARIANT id, so the product to update comes from here.
	// productVariant(id:), its product { id } and price (a decimal string) are
	// written from memory: README row "productVariant(id:)".
	mVariantLookup = `productVariant(id: $id) { id product { id ` + fCategory + ` } price }`
	// refundCreate takes the op id as Shopify's own idempotency key too
	// (ADR-0169 section 3: mandatory on refund mutations).
	// refund { totalRefundedSet } (Refund.totalRefundedSet, a MoneyBag) is what
	// the refund moved: the adapter acks it as refunded_amount_minor (R1).
	mRefundCreate = rRefundCreate + `(input: $input) ` + dIdempotent + ` { refund { id ` + fTotalRefunded + ` } userErrors { field } }`
	// What Shopify would refund for these lines: the amount (tax, discounts and
	// rounding are Shopify's) and the transactions it proposes.
	fSuggestedRefund = rSuggested + `(refundLineItems: $items) { amountSet { shopMoney { amount currencyCode } } ` +
		`suggestedTransactions { gateway parentTransaction { id } amountSet { shopMoney { amount currencyCode } presentmentMoney { amount currencyCode } } } }`
	// Input keys of refundCreate (RefundInput, RefundLineItemInput,
	// OrderTransactionInput) and of orderCancel's variables.
	kOrderID, kNotify, kNote, kRefundLines, kTransactions = "orderId", "notify", "note", "refundLineItems", "transactions"
	kLineID, kQuantity, kRestockType, kLocationID         = "lineItemId", "quantity", "restockType", "locationId"
	kParentID, kKind, kGateway, kAmount                   = "parentId", "kind", "gateway", "amount"
	vReason, vRestock, vRefund, vNotifyCustomer, vNote    = "reason", "restock", "refund", "notifyCustomer", "staffNote"
	// Enum values: OrderCancelReason, RefundLineItemRestockType, the transaction
	// kind, and the display fulfilment statuses.
	reasonCustomer, reasonInventory, reasonFraud, reasonOther = "CUSTOMER", "INVENTORY", "FRAUD", "OTHER"
	restockReturn, restockCancel, restockNone                 = "RETURN", "CANCEL", "NO_RESTOCK"
	kindRefund                                                = "REFUND"
	statusUnfulfilled, statusFulfilled                        = "UNFULFILLED", "FULFILLED"
	statusPartly, statusInProgress                            = "PARTIALLY_FULFILLED", "IN_PROGRESS"
	// The refunds of an order and their lines, as the order query reads them.
	fOrderRefunds = `refunds(first: ` + refundsAsked + `) { refundLineItems(first: ` + refundLinesAsked + `) { nodes { quantity restockType lineItem { id } } } }`
)

// How many refunds and refund lines per refund the order query reads. The
// query asks for ONE MORE than the cap (refundsAsked, refundLinesAsked), so an
// order that overflows is seen and refused rather than silently under-counted.
// queries_test.go pins asked == cap + 1.
const (
	maxRefunds       = 5
	maxRefundLines   = 20
	refundsAsked     = "6"
	refundLinesAsked = "21"
	// shippingLinesAsked: one more than maxShippingLines, so an order with
	// more shipping lines is seen and refused, not under-counted.
	maxShippingLines   = 10
	shippingLinesAsked = "11"
	// maxLocations is how many locations a shop is read for; a shop with more
	// is logged (import.locations_truncated), never dropped silently.
	maxLocations = 20
)

const variantFields = `id sku barcode price ` + fCompareAt + ` taxable selectedOptions { name value } inventoryItem { tracked }`

const pageInfo = `pageInfo { hasNextPage endCursor }`

// The order fields select ONLY what the order record needs. Customer name,
// email, phone and street address are not selected anywhere but qContact.
const lineFields = `id name sku quantity currentQuantity variant { id } ` +
	`originalUnitPriceSet { shopMoney { amount currencyCode } } ` +
	fLineAllocations + ` ` +
	`taxLines { rate priceSet { shopMoney { amount currencyCode } } }`

const orderFields = `id name createdAt updatedAt cancelledAt currencyCode taxesIncluded ` +
	`displayFinancialStatus displayFulfillmentStatus ` +
	`totalPriceSet { shopMoney { amount currencyCode } } ` +
	fTotalRefunded + ` ` +
	fOrderTax + ` ` + fShippingLines + ` ` +
	fOrderRefunds + ` ` +
	`lineItems(first: $lfirst) { ` + pageInfo + ` nodes { ` + lineFields + ` } }`

var (
	qConnectTest = queryDef{Name: "ConnectTest", Cost: 70, Text: `query ConnectTest { shop { name } ` +
		`currentAppInstallation { accessScopes { handle } } locations(first: 50) { nodes { id name ` + fLocationActive + ` } } ` +
		`productsCount { count } ordersCount { count } }`}

	qProducts = queryDef{Name: "Products", Cost: 700, Text: `query Products($first: Int!, $after: String, $vfirst: Int!) { ` +
		`shop { currencyCode taxesIncluded } products(first: $first, after: $after, sortKey: ID) { ` + pageInfo +
		` nodes { id title description status updatedAt ` + fCategory + ` variants(first: $vfirst) { ` + pageInfo + ` nodes { ` + variantFields + ` } } } } }`}

	qMoreVariants = queryDef{Name: "MoreVariants", Cost: 200, Text: `query MoreVariants($id: ID!, $first: Int!, $after: String) { ` +
		`node(id: $id) { ... on Product { variants(first: $first, after: $after) { ` + pageInfo + ` nodes { ` + variantFields + ` } } } } }`}

	qShopCurrency = queryDef{Name: "ShopCurrency", Cost: 5, Text: `query ShopCurrency { shop { currencyCode taxesIncluded } }`}

	qLocations = queryDef{Name: "Locations", Cost: 30, Text: `query Locations($first: Int!) { locations(first: $first, sortKey: ID) { nodes { id } } }`}

	qLevels = queryDef{Name: "Levels", Cost: 300, Text: `query Levels($id: ID!, $first: Int!, $after: String) { location(id: $id) { ` +
		`inventoryLevels(first: $first, after: $after) { ` + pageInfo + ` nodes { updatedAt item { tracked variant { id } } ` +
		`quantities(names: ["available"]) { quantity } } } } }`}

	qOrders = queryDef{Name: "Orders", Cost: 560, Text: `query Orders($first: Int!, $after: String, $query: String, $lfirst: Int!) { ` +
		`orders(first: $first, after: $after, query: $query, sortKey: UPDATED_AT) { ` + pageInfo + ` nodes { ` + orderFields + ` } } }`}

	qOrder = queryDef{Name: "Order", Cost: 300, Text: `query Order($id: ID!, $lfirst: Int!) { order(id: $id) { ` + orderFields + ` } }`}

	qOrderLines = queryDef{Name: "OrderLines", Cost: 250, Text: `query OrderLines($id: ID!, $first: Int!, $after: String) { ` +
		`node(id: $id) { ... on Order { lineItems(first: $first, after: $after) { ` + pageInfo + ` nodes { ` + lineFields + ` } } } } }`}

	// qContact is the ONLY operation that selects customer contact details.
	// Its answer goes to the person who opened the order and nowhere else.
	qContact = queryDef{Name: "Contact", Cost: 20, Text: `query Contact($id: ID!) { order(id: $id) { email phone ` +
		`shippingAddress { name address1 address2 city province zip country } } }`}

	qStockTarget = queryDef{Name: "StockTarget", Cost: 60, Text: `query StockTarget($id: ID!) { node(id: $id) { ... on ProductVariant { ` +
		`inventoryItem { id tracked inventoryLevels(first: 10) { nodes { location { id } quantities(names: ["available"]) { quantity } } } } } } ` +
		`locations(first: 3, sortKey: ID) { nodes { id } } }`}

	qInventorySet = queryDef{Name: "InventorySet", Cost: 10, Mutation: true, Text: `mutation InventorySet($input: InventorySetQuantitiesInput!, $key: String!) { ` +
		`inventorySetQuantities(input: $input) ` + dIdempotent + ` { userErrors { code } } }`}

	qInventoryActivate = queryDef{Name: "InventoryActivate", Cost: 10, Mutation: true, Text: `mutation InventoryActivate($item: ID!, $loc: ID!, $key: String!) { ` +
		`inventoryActivate(inventoryItemId: $item, locationId: $loc) ` + dIdempotent + ` { userErrors { field } } }`}

	qVariantPrice = queryDef{Name: "VariantPrice", Cost: 5, Text: `query VariantPrice($id: ID!) { ` + mVariantLookup + ` }`}

	qProductType = queryDef{Name: "ProductType", Cost: 10, Mutation: true, Text: `mutation ProductType($product: ProductUpdateInput!) { ` + mProductType + ` }`}

	qVariantPrices = queryDef{Name: "VariantPrices", Cost: 20, Mutation: true, Text: `mutation VariantPrices($productId: ID!, $variants: [ProductVariantsBulkInput!]!) { ` +
		`productVariantsBulkUpdate(productId: $productId, variants: $variants) { userErrors { code } } }`}

	qFulfilmentOrders = queryDef{Name: "FulfilmentOrders", Cost: 20, Text: `query FulfilmentOrders($id: ID!) { order(id: $id) { ` +
		`cancelledAt displayFulfillmentStatus fulfillmentOrders(first: 10) { nodes { id status ` + fFOLocation + ` } } } }`}

	qFulfillmentCreate = queryDef{Name: "FulfillmentCreate", Cost: 10, Mutation: true, Text: `mutation FulfillmentCreate($fulfillment: FulfillmentInput!) { ` +
		`fulfillmentCreate(fulfillment: $fulfillment) { userErrors { field } } }`}

	// Cancel reads with qFulfilmentOrders (cancelledAt, display status).

	qCancelJob = queryDef{Name: "CancelJob", Cost: 5, Text: `query CancelJob($id: ID!) { ` + mCancelJob + ` }`}

	qOrderCancel = queryDef{Name: "OrderCancel", Cost: 10, Mutation: true, Text: `mutation OrderCancel($orderId: ID!, $reason: OrderCancelReason!, $restock: Boolean!, ` +
		`$refund: Boolean!, $notifyCustomer: Boolean, $staffNote: String) { ` + mOrderCancel + ` }`}

	// RefundState is what a refund reads first: is the order cancelled, in which
	// currency, how fulfilled and where (for a return), and what is already
	// refunded per line.
	qRefundState = queryDef{Name: "RefundState", Cost: 150, Text: `query RefundState($id: ID!) { order(id: $id) { ` +
		`cancelledAt currencyCode displayFulfillmentStatus ` + fTotalRefunded + ` ` + fOrderRefunds + ` fulfillmentOrders(first: 10) { nodes { id status ` + fFOLocation + ` } } } }`}

	qSuggestedRefund = queryDef{Name: "SuggestedRefund", Cost: 40, Text: `query SuggestedRefund($id: ID!, $items: [RefundLineItemInput!]!) { order(id: $id) { ` +
		fSuggestedRefund + ` } }`}

	qRefundCreate = queryDef{Name: "RefundCreate", Cost: 10, Mutation: true, Text: `mutation RefundCreate($input: RefundInput!, $key: String!) { ` + mRefundCreate + ` }`}
)
