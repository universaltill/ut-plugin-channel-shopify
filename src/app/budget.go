package app

import (
	"errors"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Per-ask budgets. The host gives the outbox and tick asks (and every ask but
// import) a 30 second deadline, import 5 minutes (ut-cloud channelhost Call),
// and a run killed at the deadline is lost work. Shopify restores 50 points a
// second into a 1,000 point bucket and the host paces every request by its
// cost_hint, so TIME is bought with requested cost: an ask that spends C points
// needs about (C-1000)/50 seconds more than a full bucket. The budgets below
// keep a worst-case ask near 1,600 points (about 12 seconds of waiting) and
// stop BETWEEN units of work, so every stop leaves the cursor or the
// outbox exactly where the next ask resumes. The host-call counts bound the
// chatter inside the host's own caps (2,000 HTTP, 5,000 host calls); a
// throttled request is retried by the host up to six times, so 40 requests
// are at most 240 HTTP calls on the wire.
const (
	drainHTTPBudget = 40  // outbox: requests before it stops starting operations
	drainHostBudget = 150 // outbox: all host calls (next, ack, log, cursor, submit, http)
	drainCostBudget = 600 // outbox: requested points before it stops starting operations
	// askCostBudget is when a paging loop (orders) stops starting another
	// page; askCostCap is the hard limit inside a page (line completion).
	askCostBudget = 1500
	askCostCap    = 1800
	// importCostBudget: 5 minute deadline, about 1,000 + 50/s * 160 s.
	importCostBudget = 9000
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
