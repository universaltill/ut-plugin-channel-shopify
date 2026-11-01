package app

import (
	"errors"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Order cursors. A sweep reads orders updated since orders.since, page by
// page; orders.after and orders.newest let a sweep that ran out of budget
// resume where it stopped. orders.since moves only when a sweep has read every
// page, so a lost run repeats work and never skips an order.
const (
	ordersSince  = "orders.since"
	ordersAfter  = "orders.after"
	ordersNewest = "orders.newest"
)

type gqlMoney struct {
	ShopMoney struct {
		Amount   string `json:"amount"`
		Currency string `json:"currencyCode"`
	} `json:"shopMoney"`
}

type gqlTaxLine struct {
	Rate     float64  `json:"rate"`
	PriceSet gqlMoney `json:"priceSet"`
}

type gqlLine struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	SKU             string `json:"sku"`
	Quantity        int64  `json:"quantity"`
	CurrentQuantity int64  `json:"currentQuantity"`
	Variant         *struct {
		ID string `json:"id"`
	} `json:"variant"`
	UnitPrice gqlMoney `json:"originalUnitPriceSet"`
	// Allocations are every discount taken off the line, order-level code
	// shares included (fLineAllocations); the record's discount is their sum.
	Allocations []struct {
		Amount gqlMoney `json:"allocatedAmountSet"`
	} `json:"discountAllocations"`
	TaxLines []gqlTaxLine `json:"taxLines"`
}

// gqlRefund is one refund of an order and the lines it covers.
type gqlRefund struct {
	Items struct {
		Nodes []struct {
			Quantity    int64  `json:"quantity"`
			RestockType string `json:"restockType"`
			LineItem    struct {
				ID string `json:"id"`
			} `json:"lineItem"`
		} `json:"nodes"`
	} `json:"refundLineItems"`
}

type gqlLines struct {
	PageInfo pageInfoT `json:"pageInfo"`
	Nodes    []gqlLine `json:"nodes"`
}

// gqlOrder decodes ONLY what an order record needs. The buyer's name, email,
// phone and street address are neither selected in the query nor decoded
// here, so they cannot reach a submitted record, a cursor or a log line. The
// customer object is not selected at all (read_customers, ut-docs#4920).
type gqlOrder struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	CreatedAt     string   `json:"createdAt"`
	UpdatedAt     string   `json:"updatedAt"`
	CancelledAt   string   `json:"cancelledAt"`
	Currency      string   `json:"currencyCode"`
	TaxesIncluded bool     `json:"taxesIncluded"`
	Financial     string   `json:"displayFinancialStatus"`
	Fulfilment    string   `json:"displayFulfillmentStatus"`
	Total         gqlMoney `json:"totalPriceSet"`
	TotalRefunded gqlMoney `json:"totalRefundedSet"`
	TotalTax      gqlMoney `json:"totalTaxSet"`
	ShippingLines struct {
		Nodes []struct {
			Price    gqlMoney     `json:"discountedPriceSet"`
			TaxLines []gqlTaxLine `json:"taxLines"`
		} `json:"nodes"`
	} `json:"shippingLines"`
	Refunds []gqlRefund `json:"refunds"`
	Lines   gqlLines    `json:"lineItems"`
}

// completeLines follows an order's line pages. An order whose lines do not
// fit in the page budget is refused, never truncated.
func completeLines(o *gqlOrder) error {
	for extra := 0; o.Lines.PageInfo.HasNextPage; extra++ {
		if extra >= maxExtraLinePgs || !okGID(tOrder, o.ID) || !cursorRe.MatchString(o.Lines.PageInfo.EndCursor) {
			return nil // still HasNextPage: mapOrder refuses it
		}
		if spent.cost+qOrderLines.Cost > askCostCap {
			return errOverBudget // the caller stops the ask; the order is read again next ask
		}
		var d struct {
			Node *struct {
				Lines gqlLines `json:"lineItems"`
			} `json:"node"`
		}
		vars := map[string]any{"id": o.ID, "first": linePage, "after": o.Lines.PageInfo.EndCursor}
		if err := run(qOrderLines, vars, "", &d); err != nil {
			return err
		}
		if d.Node == nil {
			return nil
		}
		o.Lines.Nodes = append(o.Lines.Nodes, d.Node.Lines.Nodes...)
		o.Lines.PageInfo = d.Node.Lines.PageInfo
	}
	return nil
}

// money reads one amount in the order's currency.
func (o *gqlOrder) money(m gqlMoney) (int64, bool) {
	if m.ShopMoney.Currency != o.Currency {
		return 0, false
	}
	return parseMoney(m.ShopMoney.Amount, o.Currency)
}

// taxes sums a list of tax lines into a rate (basis points) and an amount.
func (o *gqlOrder) taxes(ts []gqlTaxLine) (bp, amount int64, ok bool) {
	for _, t := range ts {
		r, rok := rateBP(t.Rate)
		a, aok := o.money(t.PriceSet)
		if !rok || !aok {
			return 0, 0, false
		}
		bp += r
		amount += a
	}
	return bp, amount, bp <= 10000
}

// mapOrder turns Shopify's order into the contract's record. The status is
// what Shopify reports; the cloud owns the canonical one. A reason code is
// returned for an order that cannot be mapped faithfully.
func mapOrder(o *gqlOrder) (channel.Order, string) {
	if !okGID(tOrder, o.ID) || !timeRe.MatchString(o.UpdatedAt) || !timeRe.MatchString(o.CreatedAt) || !curRe.MatchString(o.Currency) {
		return channel.Order{}, "bad_order"
	}
	if o.Lines.PageInfo.HasNextPage {
		return channel.Order{}, "too_many_lines"
	}
	// The query asked for one more refund / refund line than the cap: seeing
	// it means units would be under-counted, so the order is refused.
	if len(o.Refunds) > maxRefunds {
		return channel.Order{}, "too_many_refunds"
	}
	for _, rf := range o.Refunds {
		if len(rf.Items.Nodes) > maxRefundLines {
			return channel.Order{}, "too_many_refunds"
		}
	}
	if len(o.ShippingLines.Nodes) > maxShippingLines {
		return channel.Order{}, "too_many_shipping_lines"
	}
	total, ok1 := o.money(o.Total)
	refundTotal, ok2 := o.money(o.TotalRefunded)
	orderTax, ok3 := o.money(o.TotalTax)
	if !ok1 || !ok2 || !ok3 {
		return channel.Order{}, "bad_money"
	}
	r := channel.Order{
		Identity:    channel.Identity{ExternalID: o.ID, ExternalVersion: o.UpdatedAt},
		OrderNumber: clip(o.Name, 40), PlacedAt: o.CreatedAt, Currency: o.Currency, PaymentState: paymentState(o.Financial),
		ShippingTaxIncluded: o.TaxesIncluded, TotalMinor: total, RefundTotalMinor: refundTotal, Lines: []channel.OrderLine{},
	}
	// Refunded and restocked units come from the refunds, per line.
	refunded, restocked := map[string]int64{}, map[string]int64{}
	for _, rf := range o.Refunds {
		for _, n := range rf.Items.Nodes {
			refunded[n.LineItem.ID] += n.Quantity
			if n.RestockType != "" && n.RestockType != restockNone {
				restocked[n.LineItem.ID] += n.Quantity
			}
		}
	}
	var linesSum int64
	for _, l := range o.Lines.Nodes {
		unit, ok1 := o.money(l.UnitPrice)
		bp, _, ok3 := o.taxes(l.TaxLines)
		var disc int64
		okD := true
		for _, a := range l.Allocations {
			m, ok := o.money(a.Amount)
			okD = okD && ok && m >= 0
			disc += m
		}
		if !okGID(tLineItem, l.ID) || !ok1 || !okD || !ok3 || l.Quantity < 0 || l.CurrentQuantity < 0 || l.CurrentQuantity > l.Quantity || disc > l.Quantity*unit {
			return channel.Order{}, "bad_line"
		}
		linesSum += l.Quantity*unit - disc
		// qty is the quantity as ordered (including units an order edit
		// removed); a removed or refunded unit is expressed ONLY through
		// refunded_quantity, never by lowering qty (contract section 5.1).
		//
		// ASSUMPTION (README "Verify first on the dev store"): currentQuantity
		// is the quantity minus every unit an edit removed or a refund took, so
		// qty-currentQuantity is the number of units that are gone. On a PAID
		// order an edit-removed unit also has a refund line, on an UNPAID one it
		// has none; take the larger of the two views, never their sum.
		// restocked_quantity comes ONLY from refund lines that name a restock
		// type. A unit an edit removed from an unpaid order has no refund line,
		// so it is reported refunded but NOT restocked, even if Shopify put it
		// back on the shelf: the conservative reading (claiming a restock that
		// did not happen would put phantom stock in Universal Till), and the
		// stock observation of the next import or tick corrects the count.
		removed := l.Quantity - l.CurrentQuantity
		ref := min(l.Quantity, max(refunded[l.ID], removed))
		lr := channel.OrderLine{
			ExternalID: l.ID, SKU: clip(l.SKU, 100), Name: clip(l.Name, maxTitleBytes), Qty: l.Quantity, UnitPriceMinor: unit,
			TaxBP: bp, TaxIncluded: o.TaxesIncluded, DiscountLineMinor: disc,
			RefundedQuantity: ref, RestockedQuantity: min(ref, restocked[l.ID]),
		}
		if l.Variant != nil && okGID(tVariant, l.Variant.ID) {
			lr.VariantExternalID = l.Variant.ID
		}
		r.Lines = append(r.Lines, lr)
	}
	// Shipping: every shipping line, summed. Differing tax rates across them
	// cannot be one shipping_tax_bp, so such an order is refused.
	var ship int64
	for i, s := range o.ShippingLines.Nodes {
		price, ok1 := o.money(s.Price)
		bp, _, ok2 := o.taxes(s.TaxLines)
		if !ok1 || !ok2 || price < 0 {
			return channel.Order{}, "bad_money"
		}
		if i > 0 && bp != r.ShippingTaxBP {
			return channel.Order{}, "mixed_shipping_tax"
		}
		ship += price
		r.ShippingTaxBP = bp
	}
	r.ShippingMinor = ship
	// tax_added_minor is the ORDER's tax (totalTaxSet) when prices exclude tax:
	// summing the per-line taxes can be a cent off Shopify's per-order rounding.
	if !o.TaxesIncluded {
		r.TaxAddedMinor = orderTax
	}
	// The cloud rejects an order whose total is not the contract's formula, and
	// a rejected record is invisible to the shop. Recompute it here: an order
	// Shopify's numbers do not explain (a duty, a tip, an edit that changed the
	// original total, a rounding gap) is refused with a logged code instead.
	if got := linesSum + ship - r.OrderDiscountMinor + r.TaxAddedMinor; got != total {
		return channel.Order{}, "total_mismatch"
	}
	r.Status = orderStatus(o, total, refundTotal)
	return r, ""
}

func orderStatus(o *gqlOrder, total, refunded int64) string {
	switch {
	case o.CancelledAt != "":
		return "cancelled"
	case o.Financial == "REFUNDED" || (refunded > 0 && refunded >= total):
		return "refunded"
	case o.Fulfilment == "FULFILLED":
		return "shipped"
	case refunded > 0 || o.Financial == "PARTIALLY_REFUNDED":
		return "partially_refunded"
	}
	return "new"
}

func paymentState(s string) string {
	switch s {
	case "PENDING", "PARTIALLY_PAID":
		// Not fully paid yet: the contract has no "partially paid" state.
		return "pending"
	case "AUTHORIZED":
		return "authorized"
	case "PAID":
		return "paid"
	case "PARTIALLY_REFUNDED":
		return "partially_refunded"
	case "REFUNDED":
		return "refunded"
	case "VOIDED", "EXPIRED":
		// An authorization that lapsed is void: no money will be taken.
		return "voided"
	}
	return ""
}

// logRefused records why an order was not submitted: a code and counts, never
// an amount, a name or an id.
func logRefused(reason string, o *gqlOrder) {
	hostLog(channel.LevelWarn, "order.refused."+reason, map[string]any{"lines": len(o.Lines.Nodes), "shipping_lines": len(o.ShippingLines.Nodes), "refunds": len(o.Refunds)})
}

// fetch re-fetches the named orders and submits them. A webhook is only a
// doorbell: the order is read from Shopify, never taken from the hook.
func fetch(ev channel.Event) (any, error) {
	var in channel.FetchInput
	if err := ev.Decode(&in); err != nil {
		return nil, errors.New("fetch input is not valid JSON")
	}
	if len(in.ResourceIDs) > maxFetchIDs {
		return nil, errors.New("too many resources")
	}
	var recs []channel.Order
	overBudget := false
	for _, id := range in.ResourceIDs {
		if spent.cost >= askCostBudget {
			overBudget = true // read the rest on the retry: the ask must end inside the host's deadline
			break
		}
		if !okGID(tOrder, id) {
			hostLog(channel.LevelWarn, "fetch.skip_id", map[string]any{"reason": "not_an_id"})
			continue
		}
		var d struct {
			Order *gqlOrder `json:"order"`
		}
		if err := run(qOrder, map[string]any{"id": id, "lfirst": linePage}, "", &d); err != nil {
			return nil, err
		}
		if d.Order == nil {
			continue // gone in the channel: the host marks the link, nothing is deleted
		}
		if err := completeLines(d.Order); err != nil {
			if errors.Is(err, errOverBudget) {
				overBudget = true
				break
			}
			return nil, err
		}
		r, reason := mapOrder(d.Order)
		if reason != "" {
			logRefused(reason, d.Order)
			continue
		}
		recs = append(recs, r)
	}
	if len(recs) > 0 {
		if _, err := submit(channel.KindOrder, recs); err != nil {
			return nil, err
		}
	}
	out := map[string]any{"orders": len(recs)}
	if overBudget {
		// The ask must end inside the host's deadline. What was read is
		// submitted; the rest are orders that changed recently, so the tick's
		// sweep (orders updated since its cursor) reads them: none is lost.
		hostLog(channel.LevelWarn, "fetch.deferred_to_tick", map[string]any{"read": len(recs), "asked": len(in.ResourceIDs)})
		out["deferred"] = true
	}
	return out, nil
}

// tick is the scheduled reconciliation: orders updated since the cursor.
func tick(channel.Event) (any, error) {
	n, err := pullOrders()
	if err != nil {
		return nil, err
	}
	return map[string]any{"orders": n}, nil
}

// sinceQuery is Shopify's search filter for "updated since". The timestamp is
// the only value in it and passed timeRe; the whole string is a GraphQL
// variable, never part of the query text.
func sinceQuery(since string) (string, bool) {
	if !timeRe.MatchString(since) {
		return "", false
	}
	return "updated_at:>='" + since + "'", true
}

func pullOrders() (int, error) {
	since := cursor(ordersSince, timeRe)
	after := cursor(ordersAfter, cursorRe)
	newest := cursor(ordersNewest, timeRe)
	if newest == "" {
		newest = since
	}
	total := 0
	for pages := 0; pages < maxOrderPages; pages++ {
		if pages > 0 && spent.cost+qOrders.Cost > askCostBudget {
			break // the rest resumes from orders.after next ask
		}
		vars := map[string]any{"first": orderPage, "lfirst": linePage}
		if q, ok := sinceQuery(since); ok {
			vars["query"] = q
		}
		if after != "" {
			vars["after"] = after
		}
		var d struct {
			Orders struct {
				PageInfo pageInfoT  `json:"pageInfo"`
				Nodes    []gqlOrder `json:"nodes"`
			} `json:"orders"`
		}
		if err := run(qOrders, vars, "", &d); err != nil {
			return total, err
		}
		recs := make([]channel.Order, 0, len(d.Orders.Nodes))
		for i := range d.Orders.Nodes {
			o := &d.Orders.Nodes[i]
			if timeRe.MatchString(o.UpdatedAt) && o.UpdatedAt > newest {
				newest = o.UpdatedAt
			}
			if err := completeLines(o); err != nil {
				if errors.Is(err, errOverBudget) {
					// This page is neither submitted nor passed: orders.after
					// still points at it, so the next ask reads it again.
					hostLog(channel.LevelInfo, "orders.budget_reached", map[string]any{"orders": total})
					return total, nil
				}
				return total, err
			}
			r, reason := mapOrder(o)
			if reason != "" {
				logRefused(reason, o)
				continue
			}
			recs = append(recs, r)
		}
		if len(recs) > 0 {
			if _, err := submit(channel.KindOrder, recs); err != nil {
				return total, err
			}
			total += len(recs)
		}
		pi := d.Orders.PageInfo
		if !pi.HasNextPage {
			// The sweep is complete: only now does the window move.
			if newest != "" && newest != since {
				if err := cursorSet(ordersSince, []byte(newest)); err != nil {
					return total, err
				}
			}
			for _, k := range []string{ordersAfter, ordersNewest} {
				if err := cursorSet(k, nil); err != nil {
					return total, err
				}
			}
			return total, nil
		}
		if !cursorRe.MatchString(pi.EndCursor) {
			return total, errors.New("channel sent a page cursor that is not a cursor")
		}
		after = pi.EndCursor
		if err := cursorSet(ordersAfter, []byte(after)); err != nil {
			return total, err
		}
		if newest != "" {
			if err := cursorSet(ordersNewest, []byte(newest)); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}
