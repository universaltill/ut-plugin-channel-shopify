package app

import (
	"errors"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Per-ask budgets. The host gives the outbox and tick asks (and every ask but
// import) a 30 second deadline, import 5 minutes (ut-cloud channelhost Call),
// and a run killed at the deadline is lost work.
//
// What the host does and does not do: it does NOT yet pace requests by their
// cost_hint. ut-cloud internal/channelhost/egress.go stores the hint and never
// reads it; Egress.Do only retries a throttled request with back-off (500 ms
// doubling, jittered, six attempts, 7.75 to 15.5 s in all) and then
// dead-letters it. So the budgets below are accounting INSIDE the plugin:
// call counts and Shopify-point sums that keep one ask small. Wall time is
// bounded by the request count times the latency of each request, not by
// points, and a throttle the back-off cannot clear ends the ask with an error
// (the cursor or outbox is untouched, so the next ask resumes). A ut-cloud card
// tracks the host pacing by cost_hint; until it lands nothing here waits.
//
// The budgets stop BETWEEN units of work, so every stop leaves the cursor or
// the outbox exactly where the next ask resumes. The host-call counts bound the
// chatter inside the host's own caps (2,000 HTTP, 5,000 host calls); a
// throttled request is retried by the host up to six times, so 40 requests are
// at most 240 HTTP calls on the wire.
const (
	drainHTTPBudget = 40  // outbox: requests before it stops starting operations
	drainHostBudget = 150 // outbox: all host calls (next, ack, log, cursor, submit, http)
	drainCostBudget = 600 // outbox: requested points before it stops starting operations
	// askCostBudget is when a paging loop (orders) stops starting another
	// page; askCostCap is the hard limit inside a page (line completion).
	askCostBudget = 1500
	askCostCap    = 1800
	// importCostBudget is a points sum kept for the 5 minute deadline.
	importCostBudget = 9000
)

// Order throughput. These are deliberately small and NOT measured against
// Shopify: the hand-counted cost of the orders query (queries.go, Cost 560 for
// two orders) is an upper bound, and the real cost
// (extensions.cost.actualQueryCost) is only known after the development store
// run (README, "Verify first on the dev store"). Until then a tick reads at
// most orderPage*maxOrderPages = 4 orders per 15 minute tick, so a first
// sweep of 1,000 orders takes about 62 hours. Nothing is lost, the sweep only
// lags (and webhook fetches still deliver changed orders sooner). After the
// dev store run: read actualQueryCost, raise orderPage and maxOrderPages,
// lower the cost hint.
const (
	orderPage     = 2
	maxOrderPages = 2 // per tick; the rest resumes from orders.after
	// maxFetchIDs is the most resource ids the host hands one fetch ask. A fetch
	// reads what its budget allows and defers the rest to the tick sweep.
	maxFetchIDs = 250
)

// spent counts what the current ask has used: requests, host calls
// (channel_http, submit, outbox, cursor, log) and requested GraphQL cost.
var spent struct{ http, host, cost int }

// errOverBudget stops an ask between units of work; the work resumes next ask.
var errOverBudget = errors.New("the ask used its budget")

// budgetedHandlers resets the counters at the start of every ask.
func budgetedHandlers(h channel.Handlers) channel.Handlers {
	out := channel.Handlers{}
	for k, f := range h {
		f := f
		out[k] = func(ev channel.Event) (any, error) {
			spent.http, spent.host, spent.cost = 0, 0, 0
			pullOrdersAfterDrain = false
			return f(ev)
		}
	}
	return out
}

// hostLog and cursorSet are the only ways the adapter logs or writes a
// cursor, so every host call is counted.
func hostLog(level int, code string, fields map[string]any) {
	spent.host++
	channel.Log(level, code, fields)
}

func cursorSet(key string, val []byte) error {
	spent.host++
	return channel.CursorSet(key, val)
}
