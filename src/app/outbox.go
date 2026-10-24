package app

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

const maxOutboxRounds = 20

// pullOrdersAfterDrain is set by a stale stock.set: orders explain the
// difference, and one pull per ask is enough however many ops were stale.
var pullOrdersAfterDrain bool

// overBudget is checked BETWEEN operations only, so an operation that was
// started is always finished and acknowledged; an operation not started stays
// pending in the outbox for the next ask. The drain's budgets (budget.go) are
// sized so a worst-case ask, the order pull after it included, finishes well
// inside the host's 30 second deadline for this ask at Shopify's throttle.
func overBudget() bool {
	return spent.http >= drainHTTPBudget || spent.host >= drainHostBudget || spent.cost >= drainCostBudget
}

// drainOutbox performs the pending outbound operations one at a time and
// acknowledges each. The adapter has no delete: an operation it cannot or will
// not do is dead-lettered with a non-retryable error. Writes that are safe to
// repeat carry the operation id as their idempotency key; the fulfilment and
// cancel writes are made safe by reading the order first instead (see fulfil,
// orderCancel, refundCreate in writeback.go).
func drainOutbox(channel.Event) (any, error) {
	done := 0
	var firstErr error
rounds:
	for round := 0; round < maxOutboxRounds && !overBudget(); round++ {
		spent.host++
		ops, err := channel.OutboxNext(channel.MaxOutboxBatch)
		if err != nil {
			firstErr = err
			break
		}
		if len(ops) == 0 {
			break
		}
		acked := 0
		for _, op := range ops {
			if overBudget() {
				hostLog(channel.LevelInfo, "outbox.budget_reached", map[string]any{"done": done})
				break rounds // the rest stays queued for the next ask
			}
			ack := perform(op)
			spent.host++
			if err := channel.OutboxAck(op.ID, ack); err != nil {
				// The op was performed but not acknowledged. Stop: another
				// round would hand the same op out and perform it again.
				logErr("outbox.ack_failed", err)
				break rounds
			}
			acked++
			done++
		}
		if acked == 0 {
			break
		}
	}
	if pullOrdersAfterDrain {
		if _, err := pullOrders(); err != nil {
			logErr("outbox.stale_orders_failed", err)
		}
	}
	if firstErr != nil && done == 0 {
		return nil, firstErr
	}
	return map[string]any{"done": done}, nil
}

func fail(code string, retryable bool) channel.Ack {
	return channel.Ack{Error: &channel.AckError{Code: code, Retryable: retryable}}
}

// failFor classifies a transport, status or GraphQL error. A 4xx can never
// succeed on retry; a 5xx, a throttle or a lost host call can.
func failFor(err error) channel.Ack {
	var se *statusError
	var ge *gqlError
	switch {
	case errors.Is(err, errThrottled), errors.Is(err, channel.ErrQuota):
		return fail("rate_limited", true)
	case errors.As(err, &se):
		switch {
		case se.Status >= 500 || se.Status == 408 || se.Status == 423:
			return fail("channel_error", true)
		case se.Status == 401 || se.Status == 402 || se.Status == 403:
			return fail("unauthorized", false)
		}
		return fail("rejected_by_channel", false)
	case errors.As(err, &ge):
		// A schema or validation error (a wrong field name) fails every op of
		// that kind: leave one stable code for the operator.
		hostLog(channel.LevelWarn, "outbox.graphql_error", map[string]any{"code": ge.Code})
		if ge.Code == "ACCESS_DENIED" {
			return fail("access_denied", false)
		}
		return fail("rejected_by_channel", false)
	case errors.Is(err, channel.ErrDenied):
		return fail("egress_denied", false)
	}
	return fail("channel_unreachable", true)
}

func perform(op channel.OutboxOp) channel.Ack {
	if !opIDRe.MatchString(op.ID) {
		return fail("invalid_payload", false)
	}
	switch op.Kind {
	case channel.OpStockSet:
		return stockSet(op)
	case channel.OpFulfilmentCreate:
		return fulfil(op)
	case channel.OpListingUpsert:
		return listingUpsert(op)
	case channel.OpOrderCancel:
		return orderCancel(op)
	case channel.OpRefundCreate:
		return refundCreate(op)
	}
	// listing.unpublish is NOT done: the adapter cannot set a product to draft
	// (an item push, ut-docs#4774) although items is "both".
	// Nothing else is done either; the op is dead-lettered with a logged code.
	hostLog(channel.LevelWarn, "outbox.unsupported_op", map[string]any{"kind": clip(strings.ToValidUTF8(op.Kind, ""), 40)})
	return fail("unsupported_op", false)
}

// splitStockID reads a stock.set external id: a variant, or a variant and a
// location joined with "@" (the id of the observation the cloud saw).
func splitStockID(id string) (variant, location string, ok bool) {
	variant, location, hasLoc := strings.Cut(id, "@")
	if !okGID(tVariant, variant) || (hasLoc && !okGID(tLocation, location)) {
		return "", "", false
	}
	return variant, location, true
}

type stockTarget struct {
	Node *struct {
		Item *struct {
			ID      string `json:"id"`
			Tracked bool   `json:"tracked"`
			Levels  struct {
				Nodes []struct {
					Location struct {
						ID string `json:"id"`
					} `json:"location"`
					Quantities []struct {
						Quantity int64 `json:"quantity"`
					} `json:"quantities"`
				} `json:"nodes"`
			} `json:"inventoryLevels"`
		} `json:"inventoryItem"`
	} `json:"node"`
	Locations struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	} `json:"locations"`
}

func (t *stockTarget) level(loc string) (qty int64, ok bool) {
	for _, l := range t.Node.Item.Levels.Nodes {
		if l.Location.ID == loc && len(l.Quantities) == 1 {
			return l.Quantities[0].Quantity, true
		}
	}
	return 0, false
}

func stockSet(op channel.OutboxOp) channel.Ack {
	variant, loc, ok := splitStockID(op.ExternalID)
	var p struct {
		Available *int64 `json:"available"`
		Location  string `json:"location"`
	}
	if !ok || json.Unmarshal(op.Payload, &p) != nil || p.Available == nil || *p.Available < 0 || *p.Available > 1_000_000_000 {
		return fail("invalid_payload", false)
	}
	if p.Location != "" {
		if !okGID(tLocation, p.Location) || (loc != "" && loc != p.Location) {
			return fail("invalid_payload", false)
		}
		loc = p.Location
	}
	var t stockTarget
	if err := run(qStockTarget, map[string]any{"id": variant}, "", &t); err != nil {
		return failFor(err)
	}
	if t.Node == nil || t.Node.Item == nil || !okGID(tInvItem, t.Node.Item.ID) {
		return fail("not_found", false)
	}
	if !t.Node.Item.Tracked {
		return fail("not_tracked", false)
	}
	if loc == "" { // never guess between locations
		var held []string
		for _, l := range t.Node.Item.Levels.Nodes {
			held = append(held, l.Location.ID)
		}
		switch {
		case len(held) == 1:
			loc = held[0]
		case len(held) == 0 && len(t.Locations.Nodes) == 1:
			loc = t.Locations.Nodes[0].ID
		default:
			return fail("location_required", false)
		}
	}
	if !okGID(tLocation, loc) {
		return fail("invalid_payload", false)
	}
	_, held := t.level(loc)
	if !held { // an inactive inventory level must be activated before it can be set
		var a struct {
			Activate struct {
				Errors []json.RawMessage `json:"userErrors"`
			} `json:"inventoryActivate"`
		}
		if err := run(qInventoryActivate, map[string]any{"item": t.Node.Item.ID, "loc": loc}, "", &a); err != nil {
			return failFor(err)
		}
		if len(a.Activate.Errors) > 0 {
			return fail("rejected_by_channel", false)
		}
	}
	line := map[string]any{"inventoryItemId": t.Node.Item.ID, kLocationID: loc, "quantity": *p.Available}
	input := map[string]any{"name": "available", "reason": "correction", "quantities": []any{line}}
	if op.Expect != nil && held {
		line[fCompareField] = *op.Expect // compare-and-set on the last value the cloud saw
	} else {
		input[fCompareOff] = true
	}
	var r struct {
		Set struct {
			Errors []userError `json:"userErrors"`
		} `json:"inventorySetQuantities"`
	}
	// The operation id is the idempotency key: as Shopify's own idempotency
	// directive variable, and as a header that Shopify does NOT read: the header is only
	// the host's signal that this POST is safe to re-send after a lost answer.
	if err := run(qInventorySet, map[string]any{"input": input, "key": op.ID}, op.ID, &r); err != nil {
		return failFor(err)
	}
	switch code := userErrCode(r.Set.Errors); {
	case code == "":
		return channel.Ack{ExternalID: op.ExternalID, ExternalVersion: strconv.FormatInt(*p.Available, 10)}
	case strings.Contains(code, "STALE"):
		return staleStock(variant, loc, op)
	}
	return fail("rejected_by_channel", false)
}

// staleStock handles a compare-and-set that lost: the channel's value is not
// the one the cloud last saw. Report what the channel holds now, ask for a pull of recent
// orders (once per ask, after the drain) (ADR-0169 section 8: orders explain the difference), and let the
// cloud recompute and requeue. Nothing was written.
func staleStock(variant, loc string, op channel.OutboxOp) channel.Ack {
	var t stockTarget
	if err := run(qStockTarget, map[string]any{"id": variant}, "", &t); err == nil && t.Node != nil && t.Node.Item != nil {
		if qty, ok := t.level(loc); ok && qty >= 0 {
			if _, err := submit(channel.KindStockObserved, []channel.StockObserved{observed(variant, loc, qty, "read")}); err != nil {
				logErr("outbox.stale_report_failed", err)
			}
		}
	}
	pullOrdersAfterDrain = true
	return fail("stale_expect", true)
}

func orderTarget(op channel.OutboxOp) (string, bool) {
	var p struct {
		ExternalOrderID string `json:"external_order_id"`
	}
	if json.Unmarshal(op.Payload, &p) != nil {
		return "", false
	}
	id := p.ExternalOrderID
	if id == "" {
		id = op.ExternalID
	}
	if !okGID(tOrder, id) || (op.ExternalID != "" && op.ExternalID != id) {
		return "", false
	}
	return id, true
}

type fulfilmentState struct {
	Order *struct {
		CancelledAt string `json:"cancelledAt"`
		Fulfilment  string `json:"displayFulfillmentStatus"`
		FOs         struct {
			Nodes []struct {
				ID     string `json:"id"`
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

// fulfil ships an order: it reads the order's fulfilment orders, then
// fulfils the open ones with the tracking details. The read makes it
// retry-safe without an idempotency key: an order that is already fulfilled,
// by us on an earlier attempt or by the shop in its own admin, is success.
func fulfil(op channel.OutboxOp) channel.Ack {
	id, ok := orderTarget(op)
	var p struct {
		Carrier     string `json:"carrier"`
		Tracking    string `json:"tracking_number"`
		TrackingURL string `json:"tracking_url"`
	}
	if !ok || json.Unmarshal(op.Payload, &p) != nil {
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
		return fail("order_cancelled", false)
	}
	// One group per location, in the order Shopify lists them: a fulfillment
	// may only hold fulfilment orders of one location.
	var groups [][]map[string]any
	byLoc := map[string]int{}
	onHold := false
	for _, f := range st.Order.FOs.Nodes {
		switch f.Status {
		case "OPEN", "IN_PROGRESS":
			if !okGID(tFulfOrder, f.ID) {
				continue
			}
			loc := ""
			if f.Loc != nil && f.Loc.Location != nil && okGID(tLocation, f.Loc.Location.ID) {
				loc = f.Loc.Location.ID
			}
			i, seen := byLoc[loc]
			if !seen {
				i = len(groups)
				byLoc[loc] = i
				groups = append(groups, nil)
			}
			groups[i] = append(groups[i], map[string]any{"fulfillmentOrderId": f.ID})
		case "ON_HOLD":
			onHold = true
		}
	}
	if len(groups) == 0 {
		switch {
		case st.Order.Fulfilment == "FULFILLED":
			return channel.Ack{ExternalID: id}
		case onHold:
			return fail("fulfilment_on_hold", false)
		}
		return fail("nothing_to_fulfil", false)
	}
	tracking := map[string]any{}
	if c := clip(p.Carrier, 100); c != "" {
		tracking["company"] = c
	}
	if n := clip(p.Tracking, 100); n != "" {
		tracking["number"] = n
	}
	if u := trackingURL(p.TrackingURL); u != "" {
		tracking["url"] = u
	}
	// A group that comes back with user errors does not stop the others: the
	// op fails at the end, and a retry re-reads, so a location already
	// fulfilled is never sent again.
	var failed *channel.Ack
	for _, group := range groups {
		input := map[string]any{vNotifyCustomer: false, "lineItemsByFulfillmentOrder": group}
		if len(tracking) > 0 {
			input["trackingInfo"] = tracking
		}
		var r struct {
			Create struct {
				Errors []json.RawMessage `json:"userErrors"`
			} `json:"fulfillmentCreate"`
		}
		// No Idempotency-Key here: the host would re-send a POST that may have
		// acted. A lost answer comes back as a retryable error and the next
		// attempt's read above sees what is already fulfilled.
		if err := run(qFulfillmentCreate, map[string]any{"fulfillment": input}, "", &r); err != nil {
			return failFor(err) // transport trouble: stop, the retry re-reads
		}
		if len(r.Create.Errors) > 0 && failed == nil {
			// Probably someone fulfilled it between the read and the write:
			// look again before calling it a failure.
			var again fulfilmentState
			if err := run(qFulfilmentOrders, map[string]any{"id": id}, "", &again); err == nil && again.Order != nil && groupDone(again, group) {
				continue
			}
			a := fail("rejected_by_channel", false)
			failed = &a
		} else if len(r.Create.Errors) > 0 {
			hostLog(channel.LevelWarn, "fulfil.group_rejected", map[string]any{"groups": len(groups)})
		}
	}
	if failed != nil {
		return *failed
	}
	return channel.Ack{ExternalID: id}
}

// groupDone is true when the whole order shows fulfilled or every fulfilment
// order of the group is closed (someone else fulfilled them). A cancelled or
// held one is not "done".
func groupDone(s fulfilmentState, group []map[string]any) bool {
	if s.Order.Fulfilment == "FULFILLED" {
		return true
	}
	for _, g := range group {
		closed := false
		for _, f := range s.Order.FOs.Nodes {
			if f.ID == g["fulfillmentOrderId"] && f.Status == "CLOSED" {
				closed = true
			}
		}
		if !closed {
			return false
		}
	}
	return true
}

// trackingURL keeps a tracking link only when it is an https URL.
func trackingURL(raw string) string {
	u, err := url.Parse(raw)
	if raw == "" || len(raw) > 500 || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return raw
}

// listingUpsert pushes the price of an EXISTING variant. The manifest declares
// items "both" so the cloud queues price edits (ADR-0169 criterion 6). The
// cloud's external id for an imported item is the Shopify VARIANT id, in
// op.ExternalID and in variants[0].external_id; the product to update is read
// from the variant. The record also carries fields this adapter does not push
// (title, description, category, image, compare-at): ignored, counted once per
// ask on success only. A create (no external id) is refused for good before any
// request: the adapter cannot create products (ut-docs#4774). A price in
// another currency than the shop's is refused, not converted (ADR-0169 section
// 7); a zero or negative price is refused (a live product must not go free).
// The ack keeps op.ExternalID (the variant id): the cloud stores it on the
// listing.
func listingUpsert(op channel.OutboxOp) channel.Ack {
	if op.ExternalID == "" {
		hostLog(channel.LevelWarn, "listing.create_not_supported", nil)
		return fail("create_not_supported", false)
	}
	var p struct {
		Variants []struct {
			ExternalID string `json:"external_id"`
			PriceMinor int64  `json:"price_minor"`
			Currency   string `json:"currency"`
			TaxIncl    bool   `json:"tax_included"`
		} `json:"variants"`
	}
	if !okGID(tVariant, op.ExternalID) || json.Unmarshal(op.Payload, &p) != nil || len(p.Variants) != 1 ||
		p.Variants[0].ExternalID != op.ExternalID || !curRe.MatchString(p.Variants[0].Currency) {
		return fail("invalid_payload", false)
	}
	want := p.Variants[0]
	if want.PriceMinor <= 0 {
		return fail("invalid_price", false)
	}
	if want.PriceMinor > 999_999_999_999 {
		return fail("invalid_payload", false)
	}
	var shop struct {
		Shop shopInfo `json:"shop"`
	}
	if err := run(qShopCurrency, nil, "", &shop); err != nil {
		return failFor(err)
	}
	if want.Currency != shop.Shop.Currency {
		return fail("currency_mismatch", false)
	}
	// Tax basis. variants[0].tax_included is the basis the cloud recorded for
	// this item at import (the shop's taxesIncluded AND the variant taxable) and
	// the price is in that basis (ut-cloud channelsync/outbound.go build). Only
	// one mismatch is unambiguous: the cloud says "tax included" but the shop's
	// prices now EXCLUDE tax (a stale link). Pushing would put a tax-inclusive
	// number where Shopify adds tax on top, so the customer pays tax twice.
	// "false" in a tax-included shop is legitimate (a non-taxable variant has
	// no tax to include, catalogue.go), so it is never refused.
	if want.TaxIncl && !shop.Shop.TaxesIncluded {
		return fail("tax_basis_mismatch", false)
	}
	var cur struct {
		Variant *struct {
			ID      string `json:"id"`
			Product *struct {
				ID string `json:"id"`
			} `json:"product"`
			Price string `json:"price"`
		} `json:"productVariant"`
	}
	if err := run(qVariantPrice, map[string]any{"id": op.ExternalID}, "", &cur); err != nil {
		return failFor(err)
	}
	if cur.Variant == nil || cur.Variant.Product == nil || cur.Variant.ID != op.ExternalID || !okGID(tProduct, cur.Variant.Product.ID) {
		return fail("not_found", false)
	}
	if now, ok := parseMoney(cur.Variant.Price, want.Currency); ok && now == want.PriceMinor {
		logFieldsIgnored(op)
		return channel.Ack{ExternalID: op.ExternalID} // already there: no write
	}
	var r struct {
		Update struct {
			Errors []userError `json:"userErrors"`
		} `json:"productVariantsBulkUpdate"`
	}
	inputs := []map[string]any{{"id": op.ExternalID, "price": formatMoney(want.PriceMinor, want.Currency)}}
	// An absolute price is safe to repeat, so the host may re-send it.
	if err := run(qVariantPrices, map[string]any{"productId": cur.Variant.Product.ID, "variants": inputs}, op.ID, &r); err != nil {
		return failFor(err)
	}
	switch code := userErrCode(r.Update.Errors); {
	case code == "":
		logFieldsIgnored(op)
		return channel.Ack{ExternalID: op.ExternalID}
	case strings.Contains(code, "DOES_NOT_EXIST"):
		return fail("not_found", false)
	}
	return fail("rejected_by_channel", false)
}

// Content fields of a listing record that this adapter does not push. Only
// these are counted: structural keys (cloud_id, external_id, variants,
// currency, price_minor, tax_included) are not fields the owner edited.
var (
	ignoredTop     = []string{"name", "description", "status", "category_path", "category", "images", "image", "tags"}
	ignoredVariant = []string{"sku", "barcodes", "compare_at_minor"}
)

// logFieldsIgnored counts, once per ask and only for an op that succeeded, the
// content fields the listing record carried that this adapter does not push.
func logFieldsIgnored(op channel.OutboxOp) {
	var all struct {
		Variants []map[string]json.RawMessage `json:"variants"`
	}
	var top map[string]json.RawMessage
	if spent.ignoredLogged || json.Unmarshal(op.Payload, &top) != nil || json.Unmarshal(op.Payload, &all) != nil {
		return
	}
	n := 0
	for _, k := range ignoredTop {
		if _, ok := top[k]; ok {
			n++
		}
	}
	for _, v := range all.Variants {
		for _, k := range ignoredVariant {
			if _, ok := v[k]; ok {
				n++
			}
		}
	}
	if n > 0 {
		spent.ignoredLogged = true
		hostLog(channel.LevelInfo, "listing.fields_ignored", map[string]any{"count": n})
	}
}
