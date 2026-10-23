//go:build !wasip1

package app

// AllQueries lists every operation text, for the pinning tests.
func AllQueries() []string {
	var out []string
	for _, q := range []queryDef{qConnectTest, qProducts, qMoreVariants, qShopCurrency, qLocations, qLevels, qOrders, qOrder, qOrderLines, qContact,
		qStockTarget, qInventorySet, qInventoryActivate, qVariantPrices, qFulfilmentOrders, qFulfillmentCreate} {
		out = append(out, q.Text)
	}
	return out
}

// Clip, MaxTitleBytes and Budgets expose the text and cost limits to tests.
func Clip(s string, n int) string { return clip(s, n) }

const (
	MaxTitleBytes    = maxTitleBytes
	OrderPage        = orderPage
	MaxOrderPages    = maxOrderPages
	DrainHTTPBudget  = drainHTTPBudget
	DrainHostBudget  = drainHostBudget
	DrainCostBudget  = drainCostBudget
	AskCostBudget    = askCostBudget
	AskCostCap       = askCostCap
	ImportCostBudget = importCostBudget
)

// ParseMoney, FormatMoney and RateBP expose the money helpers to tests.
func ParseMoney(a, c string) (int64, bool) { return parseMoney(a, c) }
func FormatMoney(m int64, c string) string { return formatMoney(m, c) }
func RateBP(r float64) (int64, bool)       { return rateBP(r) }

// OKGID exposes the id check to the tests.
func OKGID(t, id string) bool { return okGID(t, id) }

// Caps and patterns the pinning tests compare against the query texts.
const (
	MaxRefunds     = maxRefunds
	MaxRefundLines = maxRefundLines
	RefundsAsked   = refundsAsked
	LinesAsked     = refundLinesAsked
	MaxLocations   = maxLocations
	MaxShipLines   = maxShippingLines
	ShipLinesAsked = shippingLinesAsked
)

// Names written from memory, for the "verify first" test.
const (
	FLineAllocations = fLineAllocations
	FOrderTax        = fOrderTax
	FShippingLines   = fShippingLines
	FCompareField    = fCompareField
	FCompareOff      = fCompareOff
	DIdempotent      = dIdempotent
)

func CursorOK(s string) bool  { return cursorRe.MatchString(s) }
func AccountOK(s string) bool { return accountRe.MatchString(s) }
func AccountPattern() string  { return accountRe.String() }
