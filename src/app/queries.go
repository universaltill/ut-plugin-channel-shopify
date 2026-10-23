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
	// The compare-and-set field of inventorySetQuantities, and the flag that
	// turns the compare off.
	fCompareField = "changeFromQuantity"
	fCompareOff   = "ignoreCompareQuantity"
	// Shopify's own idempotency directive for inventorySetQuantities. The
	// Idempotency-Key HTTP header is NOT a Shopify feature: it is only a signal
	// to the host that the POST is safe to re-send.
	dIdempotent = `@idempotent(key: $key)`
	// The location a fulfilment order is assigned to. Shopify refuses a
	// fulfillment whose fulfilment orders sit at different locations, so the
	// adapter sends one fulfillmentCreate per location.
	fFOLocation = `assignedLocation { location { id } }`
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

const variantFields = `id sku barcode price taxable selectedOptions { name value } inventoryItem { tracked }`

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
	`totalRefundedSet { shopMoney { amount currencyCode } } ` +
	fOrderTax + ` ` + fShippingLines + ` ` +
	`customer { id } shippingAddress { countryCodeV2 } ` +
	`refunds(first: ` + refundsAsked + `) { refundLineItems(first: ` + refundLinesAsked + `) { nodes { quantity restockType lineItem { id } } } } ` +
	`lineItems(first: $lfirst) { ` + pageInfo + ` nodes { ` + lineFields + ` } }`

var (
	qConnectTest = queryDef{Name: "ConnectTest", Cost: 40, Text: `query ConnectTest { shop { name } ` +
		`currentAppInstallation { accessScopes { handle } } locations(first: 20) { nodes { name } } ` +
		`productsCount { count } ordersCount { count } }`}

	qProducts = queryDef{Name: "Products", Cost: 700, Text: `query Products($first: Int!, $after: String, $vfirst: Int!) { ` +
		`shop { currencyCode taxesIncluded } products(first: $first, after: $after, sortKey: ID) { ` + pageInfo +
		` nodes { id title description status updatedAt variants(first: $vfirst) { ` + pageInfo + ` nodes { ` + variantFields + ` } } } } }`}

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
		`customer { displayName } shippingAddress { name address1 address2 city province zip country } } }`}

	qStockTarget = queryDef{Name: "StockTarget", Cost: 60, Text: `query StockTarget($id: ID!) { node(id: $id) { ... on ProductVariant { ` +
		`inventoryItem { id tracked inventoryLevels(first: 10) { nodes { location { id } quantities(names: ["available"]) { quantity } } } } } } ` +
		`locations(first: 3, sortKey: ID) { nodes { id } } }`}

	qInventorySet = queryDef{Name: "InventorySet", Cost: 10, Mutation: true, Text: `mutation InventorySet($input: InventorySetQuantitiesInput!, $key: String!) { ` +
		`inventorySetQuantities(input: $input) ` + dIdempotent + ` { userErrors { code } } }`}

	qInventoryActivate = queryDef{Name: "InventoryActivate", Cost: 10, Mutation: true, Text: `mutation InventoryActivate($item: ID!, $loc: ID!) { ` +
		`inventoryActivate(inventoryItemId: $item, locationId: $loc) { userErrors { field } } }`}

	qVariantPrices = queryDef{Name: "VariantPrices", Cost: 20, Mutation: true, Text: `mutation VariantPrices($productId: ID!, $variants: [ProductVariantsBulkInput!]!) { ` +
		`productVariantsBulkUpdate(productId: $productId, variants: $variants) { userErrors { code } } }`}

	qFulfilmentOrders = queryDef{Name: "FulfilmentOrders", Cost: 20, Text: `query FulfilmentOrders($id: ID!) { order(id: $id) { ` +
		`cancelledAt displayFulfillmentStatus fulfillmentOrders(first: 10) { nodes { id status ` + fFOLocation + ` } } } }`}

	qFulfillmentCreate = queryDef{Name: "FulfillmentCreate", Cost: 10, Mutation: true, Text: `mutation FulfillmentCreate($fulfillment: FulfillmentInput!) { ` +
		`fulfillmentCreate(fulfillment: $fulfillment) { userErrors { field } } }`}
)
