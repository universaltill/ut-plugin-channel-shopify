package app

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// order.cancel and refund.create (ut-docs#4774; the cloud's half is
// ut-docs#4771, ADR-0169 section 9). Both are READ FIRST, never blind, like
// fulfilment: the cloud re-offers an operation unchanged until it is
// acknowledged, so a retry after a lost answer must find the effect already
// there and call it success. Nothing is ever deleted: a cancel keeps the
// order, a refund adds a refund.

// cancelReasons maps the contract's reasons (channel-order-action.md) to
// Shopify's OrderCancelReason.
var cancelReasons = map[string]string{
	"customer_request": reasonCustomer, "out_of_stock": reasonInventory, "fraud": reasonFraud, "other": reasonOther,
}

// The notes Shopify shows staff on the order. Constants: a note never carries
// customer text, a name or an order number.
const (
	cancelNote = "Cancelled from Universal Till"
	refundNote = "Refunded from Universal Till"
)

// hasErrors says whether a user-error list in an answer is not empty.
func hasErrors(raw json.RawMessage) bool {
	var l []json.RawMessage
	return json.Unmarshal(raw, &l) == nil && len(l) > 0
}

// payloadErrors is true if the mutation's answer carries any user error.
func payloadErrors(root json.RawMessage, fields ...string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(root, &m) != nil {
		return true // an answer that is not an object is not a success
	}
	for _, f := range fields {
		if hasErrors(m[f]) {
			return true
		}
	}
	return false
}

// orderCancel cancels an order. Read first: an already-cancelled order is
// success (we did it on an earlier attempt, or the shop did); one Shopify
// cannot cancel any more (fulfilment has begun) is refused for good.
func orderCancel(op channel.OutboxOp) channel.Ack {
	id, ok := orderTarget(op)
	var p struct {
		Reason  string `json:"reason"`
		Restock bool   `json:"restock"`
	}
	reason, known := "", false
	if ok && json.Unmarshal(op.Payload, &p) == nil {
		reason, known = cancelReasons[p.Reason]
	}
	if !ok || !known {
		return fail("invalid_payload", false)
	}
	var st fulfilmentState
	if err := run(qFulfilmentOrders, map[string]any{"id": id}, "", &st); err != nil {
		return failFor(err)
	}
	if st.Order == nil {
		return fail("not_found", false)
	}
	if st.Order.CancelledAt != "" {
		return channel.Ack{ExternalID: id}
	}
	switch st.Order.Fulfilment {
	case statusFulfilled, statusPartly, statusInProgress:
		return fail("order_fulfilled", false)
	}
	vars := map[string]any{
		kOrderID: id, vReason: reason, vRestock: p.Restock, vRefund: true, vNotifyCustomer: false, vNote: cancelNote,
	}
	var r map[string]json.RawMessage
	// No Idempotency-Key: the host would re-send a POST that may have acted,
	// and a second cancel is a user error that would hide the first's success.
	// A lost answer comes back retryable and the next attempt's read above
	// finds the order cancelled.
	if err := run(qOrderCancel, vars, "", &r); err != nil {
		return failFor(err)
	}
	if payloadErrors(r[rOrderCancel], fCancelUserErrors, fUserErrors) {
		// Someone cancelled it between the read and the write: look again.
		var again fulfilmentState
		if err := run(qFulfilmentOrders, map[string]any{"id": id}, "", &again); err != nil {
			return failFor(err) // retryable if transport: the next attempt reads first
		}
		if again.Order != nil && again.Order.CancelledAt != "" {
			return channel.Ack{ExternalID: id}
		}
		if cancelRunning(r[rOrderCancel]) {
			// An earlier cancel of this order (our lost answer) is still a job.
			return fail("cancel_pending", true)
		}
		return fail("rejected_by_channel", false)
	}
	return confirmCancel(id, r[rOrderCancel])
}

// cancelRunning is true when orderCancel's user errors say a cancel of the
// order is already in progress, which is not a rejection.
func cancelRunning(root json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(root, &m) != nil {
		return false
	}
	for _, f := range []string{fCancelUserErrors, fUserErrors} {
		var es []struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(m[f], &es) != nil {
			continue
		}
		for _, e := range es {
			if strings.Contains(strings.ToLower(e.Message), cancelInProgressText) {
				return true
			}
		}
	}
	return false
}

// confirmCancel decides the ack after orderCancel accepted the job. Shopify
// cancels asynchronously, and the cloud's confirmation marks every unit
// refunded and restocked, so success is acknowledged only when the order shows
// cancelled: a job not done is polled ONCE, still running is a retryable
// cancel_pending (the retry reads first and finds it cancelled), and a job
// that ended without cancelling the order is cancel_failed for good.
func confirmCancel(id string, root json.RawMessage) channel.Ack {
	var m map[string]json.RawMessage
	var j struct {
		ID   string `json:"id"`
		Done bool   `json:"done"`
	}
	_ = json.Unmarshal(root, &m)
	known := json.Unmarshal(m[rJob], &j) == nil && okGID(tJob, j.ID)
	done := known && j.Done
	if known && !done {
		var pr map[string]json.RawMessage
		if err := run(qCancelJob, map[string]any{"id": j.ID}, "", &pr); err != nil {
			var ge *gqlError
			if errors.As(err, &ge) { // a wrong job name must not dead-letter an order being cancelled
				hostLog(channel.LevelWarn, "outbox.graphql_error", map[string]any{"code": ge.Code})
				return fail("cancel_pending", true)
			}
			return failFor(err)
		}
		var pj struct {
			Done bool `json:"done"`
		}
		_ = json.Unmarshal(pr[rJob], &pj)
		if done = pj.Done; !done {
			return fail("cancel_pending", true)
		}
	}
	var st fulfilmentState
	if err := run(qFulfilmentOrders, map[string]any{"id": id}, "", &st); err != nil {
		return failFor(err)
	}
	switch {
	case st.Order != nil && st.Order.CancelledAt != "":
		return channel.Ack{ExternalID: id}
	case done:
		return fail("cancel_failed", false) // the job ended and the order is not cancelled
	}
	return fail("cancel_pending", true)
}

// Bounds of a refund payload. The cloud sends up to 100 lines.
const (
	maxRefundPayloadLines = 100
	maxRefundQuantity     = 1_000_000
	maxMinorAmount        = 999_999_999_999
)

type refundLinePayload struct {
	LineID      string `json:"line_external_id"`
	Quantity    int64  `json:"quantity"`
	RefundedTo  int64  `json:"refunded_to"`
	RestockedTo int64  `json:"restocked_to"`
	// unit_price_minor and discount_line_minor (and anything else the cloud
	// adds) are tolerated and not needed: Shopify works the money out.
}

type refundPayload struct {
	Reason   string              `json:"reason"`
	Currency string              `json:"currency"`
	Max      *int64              `json:"max_amount_minor"`
	Lines    []refundLinePayload `json:"lines"`
}

func (p *refundPayload) valid() bool {
	if _, ok := cancelReasons[p.Reason]; !ok || !curRe.MatchString(p.Currency) || p.Max == nil || *p.Max < 0 || *p.Max > maxMinorAmount ||
		len(p.Lines) == 0 || len(p.Lines) > maxRefundPayloadLines {
		return false
	}
	seen := map[string]bool{}
	for _, l := range p.Lines {
		if !okGID(tLineItem, l.LineID) || seen[l.LineID] || l.Quantity < 1 || l.Quantity > maxRefundQuantity ||
			l.RefundedTo < l.Quantity || l.RefundedTo > maxRefundQuantity || l.RestockedTo < 0 || l.RestockedTo > l.RefundedTo {
			return false
		}
		seen[l.LineID] = true
	}
	return true
}

type refundState struct {
	Order *struct {
		CancelledAt string      `json:"cancelledAt"`
		Currency    string      `json:"currencyCode"`
		Fulfilment  string      `json:"displayFulfillmentStatus"`
		Refunds     []gqlRefund `json:"refunds"`
		FOs         struct {
			Nodes []struct {
				Status string `json:"status"`
				Loc    *struct {
					Location *struct {
						ID string `json:"id"`
					} `json:"location"`
				} `json:"assignedLocation"`
			} `json:"nodes"`
		} `json:"fulfillmentOrders"`
	} `json:"order"`
}

// done returns, per line, the units already refunded and the units already
// restocked; ok is false when the order has more refunds than the query reads
// (it cannot be told what is already done, so nothing is sent).
func (s *refundState) done() (refunded, restocked map[string]int64, ok bool) {
	refunded, restocked = map[string]int64{}, map[string]int64{}
	if len(s.Order.Refunds) > maxRefunds {
		return nil, nil, false
	}
	for _, rf := range s.Order.Refunds {
		if len(rf.Items.Nodes) > maxRefundLines {
			return nil, nil, false
		}
		for _, n := range rf.Items.Nodes {
			refunded[n.LineItem.ID] += n.Quantity
			if n.RestockType != "" && n.RestockType != restockNone {
				restocked[n.LineItem.ID] += n.Quantity
			}
		}
	}
	return refunded, restocked, true
}

// owed is what is still to refund per line: the target the cloud named minus
// what the order already shows, never more than the quantity asked for. A line
// that is already at its target (our earlier attempt, or the shop's own
// refund) owes nothing.
func (p *refundPayload) owed(refunded map[string]int64) (lines []refundLinePayload) {
	for _, l := range p.Lines {
		n := min(l.Quantity, l.RefundedTo-refunded[l.LineID])
		if n > 0 {
			l.Quantity = n
			lines = append(lines, l)
		}
	}
	return lines
}

type moneyBag struct {
	Amount   string `json:"amount"`
	Currency string `json:"currencyCode"`
}

type suggestedAnswer struct {
	Amount struct {
		Shop moneyBag `json:"shopMoney"`
	} `json:"amountSet"`
	Transactions []struct {
		Gateway string `json:"gateway"`
		Parent  *struct {
			ID string `json:"id"`
		} `json:"parentTransaction"`
		Amount struct {
			Shop         moneyBag `json:"shopMoney"`
			Presentation moneyBag `json:"presentmentMoney"`
		} `json:"amountSet"`
	} `json:"suggestedTransactions"`
}

// refundCreate refunds units of an order's lines. Read first (what the order
// already shows refunded), then ask Shopify what the owed units come to and
// refuse, before sending, any amount above the ceiling the cloud allows.
func refundCreate(op channel.OutboxOp) channel.Ack {
	id, ok := orderTarget(op)
	var p refundPayload
	if !ok || json.Unmarshal(op.Payload, &p) != nil || !p.valid() {
		return fail("invalid_payload", false)
	}
	var st refundState
	if err := run(qRefundState, map[string]any{"id": id}, "", &st); err != nil {
		return failFor(err)
	}
	if st.Order == nil {
		return fail("not_found", false)
	}
	refunded, restocked, ok := st.done()
	if !ok {
		return fail("too_many_refunds", false)
	}
	owed := p.owed(refunded)
	if len(owed) == 0 {
		return channel.Ack{ExternalID: id} // already refunded: by us earlier, or by the shop
	}
	switch {
	case st.Order.CancelledAt != "":
		return fail("order_cancelled", false)
	case st.Order.Currency != p.Currency:
		return fail("currency_mismatch", false)
	}
	items, code := refundItems(&st, owed, restocked)
	if code != "" {
		return fail(code, false)
	}
	var sug struct {
		Order map[string]json.RawMessage `json:"order"`
	}
	if err := run(qSuggestedRefund, map[string]any{"id": id, "items": items}, "", &sug); err != nil {
		return failFor(err)
	}
	var s suggestedAnswer
	if sug.Order == nil || json.Unmarshal(sug.Order[rSuggested], &s) != nil {
		return fail("rejected_by_channel", false)
	}
	txns, amount, code := refundTransactions(id, p.Currency, &s)
	if code != "" {
		return fail(code, false)
	}
	if amount > *p.Max {
		// Shopify's own figure is above the ceiling: refund NOTHING.
		hostLog(channel.LevelWarn, "refund.above_ceiling", map[string]any{"lines": len(owed)})
		return fail("refund_exceeds_ceiling", false)
	}
	input := map[string]any{kOrderID: id, kNotify: false, kNote: refundNote, kRefundLines: items}
	if len(txns) > 0 {
		input[kTransactions] = txns
	}
	var r map[string]json.RawMessage
	// The op id is Shopify's idempotency key (a re-sent POST is deduplicated by
	// Shopify) and the host's signal that a re-send is safe. Exactly-once across
	// attempts still rests on the read above.
	if err := run(qRefundCreate, map[string]any{"input": input, "key": op.ID}, op.ID, &r); err != nil {
		return failFor(err)
	}
	if payloadErrors(r[rRefundCreate], fUserErrors) {
		// Probably someone refunded it between the read and the write.
		var again refundState
		if err := run(qRefundState, map[string]any{"id": id}, "", &again); err == nil && again.Order != nil {
			if rf, _, ok := again.done(); ok && len(p.owed(rf)) == 0 {
				return channel.Ack{ExternalID: id}
			}
		}
		return fail("rejected_by_channel", false)
	}
	return channel.Ack{ExternalID: id}
}

// refundItems builds the refund line items. A line is restocked only when the
// cloud asks for every owed unit of it to go back (a refund line has one
// restock type; splitting a line would risk restocking units that were not
// meant to return). An unfulfilled order restocks as a cancel, a fulfilled one
// as a return to the single location it shipped from; a part-fulfilled order
// cannot say which units are where and is refused.
func refundItems(st *refundState, owed []refundLinePayload, restocked map[string]int64) ([]map[string]any, string) {
	var items []map[string]any
	for _, l := range owed {
		item := map[string]any{kLineID: l.LineID, kQuantity: l.Quantity, kRestockType: restockNone}
		if l.RestockedTo-restocked[l.LineID] >= l.Quantity {
			switch st.Order.Fulfilment {
			case statusUnfulfilled:
				item[kRestockType] = restockCancel
			case statusFulfilled:
				loc, code := fulfilmentLocation(st)
				if code != "" {
					return nil, code
				}
				item[kRestockType], item[kLocationID] = restockReturn, loc
			default:
				return nil, "restock_not_supported"
			}
		}
		items = append(items, item)
	}
	return items, ""
}

// fulfilmentLocation is the one location a fulfilled order shipped from.
func fulfilmentLocation(st *refundState) (string, string) {
	loc := ""
	for _, f := range st.Order.FOs.Nodes {
		if f.Status == "CANCELLED" || f.Loc == nil || f.Loc.Location == nil || !okGID(tLocation, f.Loc.Location.ID) {
			continue
		}
		if loc != "" && loc != f.Loc.Location.ID {
			return "", "location_required" // never guess between locations
		}
		loc = f.Loc.Location.ID
	}
	if loc == "" {
		return "", "location_required"
	}
	return loc, ""
}

// refundTransactions turns Shopify's suggested transactions into the refund's
// transactions and returns the money they move in the shop's currency. The
// suggested refund's own amount counts too: the larger of the two is what is
// compared with the ceiling.
func refundTransactions(order, cur string, s *suggestedAnswer) (txns []map[string]any, amount int64, code string) {
	amount, ok := parseMoney(s.Amount.Shop.Amount, s.Amount.Shop.Currency)
	if !ok || s.Amount.Shop.Currency != cur {
		return nil, 0, "rejected_by_channel"
	}
	var sum int64
	for _, t := range s.Transactions {
		shop, ok1 := parseMoney(t.Amount.Shop.Amount, t.Amount.Shop.Currency)
		pres, ok2 := parseMoney(t.Amount.Presentation.Amount, t.Amount.Presentation.Currency)
		if !ok1 || !ok2 || t.Amount.Shop.Currency != cur || t.Parent == nil || !okGID(tTxn, t.Parent.ID) || shop > maxMinorAmount {
			return nil, 0, "rejected_by_channel"
		}
		sum += shop
		if shop == 0 {
			continue
		}
		txns = append(txns, map[string]any{kOrderID: order, kParentID: t.Parent.ID, kKind: kindRefund, kGateway: clip(t.Gateway, 60),
			kAmount: formatMoney(pres, t.Amount.Presentation.Currency)})
	}
	return txns, max(amount, sum), ""
}
