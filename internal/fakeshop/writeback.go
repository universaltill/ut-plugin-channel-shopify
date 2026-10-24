package fakeshop

import (
	"encoding/json"
	"strings"
	"time"
)

// Cancel and refund, as far as the adapter's tests need them. Every shape here
// is the fake's reading of the Admin API as remembered (README, "Verify first
// on the dev store"); only the owner's development store run proves it.

// CancelRecord is what the last successful orderCancel of an order carried.
type CancelRecord struct {
	Reason     string
	Restock    bool
	Refund     bool
	Notify     *bool
	StaffNote  string
	HadJobDone bool
}

// RefundRecord is one refund the adapter (or the merchant) made through
// refundCreate: the lines, the restock types and the money sent.
type RefundRecord struct {
	Lines       map[string]int64  // line id -> units
	RestockType map[string]string // line id -> restock type
	LocationID  map[string]string // line id -> location sent with it
	Amount      int64
	Note        string
	Notify      *bool
}

// LoseAnswer makes the next n mutations of the named operation take effect and
// then come back as a 502 with no body: the adapter cannot tell whether it
// worked (the case that exactly-once must survive).
func (s *Shop) LoseAnswer(operation string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lose[operation] = n
}

// RefuseCancel makes orderCancel of this order come back with a user error.
func (s *Shop) RefuseCancel(orderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noCancel[orderID] = true
}

// SuggestExtra makes every suggested refund ask for n minor units more than
// the shop can refund (a race with another refund, or a Shopify rounding that
// disagrees): the refund that follows is refused for exceeding what was paid.
func (s *Shop) SuggestExtra(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suggestExtra = n
}

// Cancel returns what orderCancel carried for the order (ok false when it was
// never cancelled through the API).
func (s *Shop) Cancel(orderID string) (CancelRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cancels[orderID]
	return c, ok
}

// Refunds returns the refunds made through refundCreate for the order.
func (s *Shop) Refunds(orderID string) []RefundRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RefundRecord(nil), s.refunds[orderID]...)
}

// RefundTotal is the money refunded on the order, minor units.
func (s *Shop) RefundTotal(orderID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orders {
		if o.ID == orderID {
			return o.RefundTotal
		}
	}
	return -1
}

// AddOrderLine adds a line of qty units at unit price (minor) to the order and
// returns its id; the total follows.
func (s *Shop) AddOrderLine(orderID string, qty, unit int64) string {
	id := ""
	s.MutateOrder(orderID, func(o *Order) {
		v := s.products[len(o.Lines)].Variants[0]
		id = gid("LineItem", 6500+len(o.Lines))
		o.Lines = append(o.Lines, orderLine{ID: id, Name: "Invented line", SKU: v.SKU, VariantID: v.ID, Qty: qty, CurQty: qty, Unit: unit, Rate: 0.2})
		o.Total = totalOf(*o)
	})
	return id
}

// SetFulfilmentStatus sets the order's displayFulfillmentStatus (UNFULFILLED,
// PARTIALLY_FULFILLED, FULFILLED, IN_PROGRESS ...).
func (s *Shop) SetFulfilmentStatus(orderID, status string) {
	s.MutateOrder(orderID, func(o *Order) { o.Ful = status })
}

// SetFulfilmentOrderStatus sets every fulfilment order of the order to status.
func (s *Shop) SetFulfilmentOrderStatus(orderID, status string) {
	s.MutateOrder(orderID, func(o *Order) {
		for i := range o.FOs {
			o.FOs[i].Status = status
		}
	})
}

// StockOfLine is the available quantity of the variant of the order's line idx at
// the location.
func (s *Shop) StockOfLine(orderID string, idx int, loc string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orders {
		if o.ID == orderID {
			if lv := s.levels[o.Lines[idx].VariantID+"@"+loc]; lv != nil {
				return lv.Qty
			}
		}
	}
	return -1
}

func (s *Shop) orderByID(id string) *Order {
	for i := range s.orders {
		if s.orders[i].ID == id {
			return &s.orders[i]
		}
	}
	return nil
}

func refundedUnits(o *Order) map[string]int64 {
	m := map[string]int64{}
	for _, rf := range o.Refunds {
		for _, l := range rf {
			m[l.LineID] += l.Qty
		}
	}
	return m
}

// lineValue is q units of the line at unit price less their share of the
// discounts (rounded down), plus the tax when prices exclude it.
func lineValue(o *Order, l orderLine, q int64) int64 {
	net := l.Unit*q - (l.Discount+l.CodeAlloc)*q/l.Qty
	if !o.TaxesIncluded {
		net += taxOf(net, l.Rate, false)
	}
	return net
}

func userErr(root string) obj {
	return obj{root: obj{"refund": nil, "userErrors": []obj{{"field": []string{"input"}}}}}
}

func (s *Shop) opRefundState(req gqlRequest) (any, string) {
	o := s.orderByID(s.str(req, "id"))
	if o == nil {
		return obj{"order": nil}, ""
	}
	q := req.Query
	maxRefunds, maxLines := firstOf(q, "refunds"), firstOf(q, "refundLineItems")
	var refunds []obj
	for ri, rf := range o.Refunds {
		if ri >= maxRefunds {
			break
		}
		var rl []obj
		for li, l := range rf {
			if li >= maxLines {
				break
			}
			rl = append(rl, obj{"quantity": l.Qty, "restockType": l.RestockType, "lineItem": obj{"id": l.LineID}})
		}
		refunds = append(refunds, obj{"refundLineItems": obj{"nodes": rl}})
	}
	var fos []obj
	for _, f := range o.FOs {
		fos = append(fos, obj{"id": f.ID, "status": f.Status, "assignedLocation": obj{"location": obj{"id": f.Loc}}})
	}
	var cancelled any
	if o.CancelledAt != "" {
		cancelled = o.CancelledAt
	}
	return obj{"order": obj{"cancelledAt": cancelled, "currencyCode": s.opt.Currency, "displayFulfillmentStatus": o.Ful,
		"refunds": refunds, "fulfillmentOrders": obj{"nodes": fos}}}, ""
}

type refundItem struct {
	LineItemID  string `json:"lineItemId"`
	Quantity    int64  `json:"quantity"`
	RestockType string `json:"restockType"`
	LocationID  string `json:"locationId"`
}

var restockTypes = map[string]bool{"NO_RESTOCK": true, "RETURN": true, "CANCEL": true, "LEGACY_RESTOCK": true}

// checkItems validates refund line items against the order like Shopify:
// the line exists, the quantity is within what is still refundable, once.
func (s *Shop) checkItems(o *Order, items []refundItem) (int64, bool) {
	done := refundedUnits(o)
	seen := map[string]bool{}
	var value int64
	if len(items) == 0 {
		return 0, false
	}
	for _, it := range items {
		var line *orderLine
		for i := range o.Lines {
			if o.Lines[i].ID == it.LineItemID {
				line = &o.Lines[i]
			}
		}
		if line == nil || seen[it.LineItemID] || it.Quantity < 1 || it.Quantity > line.Qty-done[line.ID] {
			return 0, false
		}
		seen[it.LineItemID] = true
		value += lineValue(o, *line, it.Quantity)
	}
	return value, true
}

func (s *Shop) opSuggestedRefund(req gqlRequest) (any, string) {
	o := s.orderByID(s.str(req, "id"))
	if o == nil {
		return obj{"order": nil}, ""
	}
	var items []refundItem
	s.vars(req, "items", &items)
	value, ok := s.checkItems(o, items)
	if !ok {
		return nil, "INVALID_REFUND_LINE_ITEMS"
	}
	cur := s.opt.Currency
	amount := value
	if left := o.Total - o.RefundTotal; amount > left {
		amount = left
	}
	amount += s.suggestExtra
	var txns []obj
	if amount > 0 {
		m := ms(amount, cur)
		m["presentmentMoney"] = obj{"amount": minor(amount), "currencyCode": cur}
		if s.txnCurrency != "" {
			m["shopMoney"] = obj{"amount": minor(amount), "currencyCode": s.txnCurrency}
		}
		txns = append(txns, obj{"gateway": "manual", "kind": "SUGGESTED_REFUND", "parentTransaction": obj{"id": gid("OrderTransaction", 9100)}, "amountSet": m})
	}
	return obj{"order": obj{"suggestedRefund": obj{"amountSet": ms(value, cur), "suggestedTransactions": txns}}}, ""
}

func (s *Shop) opRefundCreate(req gqlRequest) (any, string) {
	key := s.str(req, "key")
	if cached, ok := s.idem["refund:"+key]; ok {
		var out obj
		_ = json.Unmarshal([]byte(cached), &out)
		return out, ""
	}
	var in struct {
		OrderID      string       `json:"orderId"`
		Note         string       `json:"note"`
		Notify       *bool        `json:"notify"`
		RefundLines  []refundItem `json:"refundLineItems"`
		Transactions []struct {
			OrderID  string `json:"orderId"`
			ParentID string `json:"parentId"`
			Kind     string `json:"kind"`
			Gateway  string `json:"gateway"`
			Amount   string `json:"amount"`
		} `json:"transactions"`
	}
	s.vars(req, "input", &in)
	fail := func() (any, string) { return userErr("refundCreate"), "" }
	o := s.orderByID(in.OrderID)
	if o == nil || o.CancelledAt != "" {
		return fail()
	}
	_, ok := s.checkItems(o, in.RefundLines)
	if !ok {
		return fail()
	}
	var paid int64
	for _, t := range in.Transactions {
		if t.Kind != "REFUND" || t.OrderID != o.ID || !strings.HasPrefix(t.ParentID, "gid://shopify/OrderTransaction/") || t.Gateway == "" {
			return fail()
		}
		paid += parseMinor(t.Amount)
	}
	if paid > o.Total-o.RefundTotal { // refund exceeding what was paid
		return fail()
	}
	rec := RefundRecord{Lines: map[string]int64{}, RestockType: map[string]string{}, LocationID: map[string]string{}, Amount: paid, Note: in.Note, Notify: in.Notify}
	var rl []refundLine
	for _, it := range in.RefundLines {
		if !restockTypes[it.RestockType] {
			return fail()
		}
		switch it.RestockType {
		case "RETURN":
			if s.levels[s.variantOf(o, it.LineItemID)+"@"+it.LocationID] == nil {
				return fail()
			}
		case "CANCEL":
			if o.Ful != "UNFULFILLED" {
				return fail()
			}
		}
		rec.Lines[it.LineItemID], rec.RestockType[it.LineItemID], rec.LocationID[it.LineItemID] = it.Quantity, it.RestockType, it.LocationID
		rl = append(rl, refundLine{LineID: it.LineItemID, Qty: it.Quantity, RestockType: it.RestockType})
	}
	for _, it := range in.RefundLines { // the effect
		loc := it.LocationID
		if loc == "" && len(o.FOs) > 0 {
			loc = o.FOs[0].Loc
		}
		if it.RestockType != "NO_RESTOCK" {
			if lv := s.levels[s.variantOf(o, it.LineItemID)+"@"+loc]; lv != nil {
				lv.Qty += it.Quantity
				lv.UpdatedAt = s.bump()
			}
		}
	}
	o.Refunds = append(o.Refunds, rl)
	o.RefundTotal += paid
	switch {
	case o.RefundTotal >= o.Total:
		o.Fin = "REFUNDED"
	case o.RefundTotal > 0:
		o.Fin = "PARTIALLY_REFUNDED"
	}
	o.UpdatedAt = s.bump()
	s.refunds[o.ID] = append(s.refunds[o.ID], rec)
	s.acts["refund:"+o.ID]++
	out := obj{"refundCreate": obj{"refund": obj{"id": gid("Refund", 9200+len(s.refunds[o.ID]))}, "userErrors": []obj{}}}
	b, _ := json.Marshal(out)
	s.idem["refund:"+key] = string(b)
	return out, ""
}

func (s *Shop) variantOf(o *Order, lineID string) string {
	for _, l := range o.Lines {
		if l.ID == lineID {
			return l.VariantID
		}
	}
	return ""
}

var cancelReasons = map[string]bool{"CUSTOMER": true, "DECLINED": true, "FRAUD": true, "INVENTORY": true, "OTHER": true, "STAFF": true}

// cancelJob is an orderCancel Shopify accepted and has not finished.
type cancelJob struct {
	order  string
	left   int // job queries until it is done
	fail   bool
	args   CancelRecord
	finish bool
}

// SlowCancel makes the next orderCancel an asynchronous job that is done only
// at the polls-th job query (or when FinishCancelJobs is called).
func (s *Shop) SlowCancel(polls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelPolls = polls
}

// OddCancelJob makes the next orderCancel come back accepted with this job
// (nil: no job at all) while the order stays uncancelled.
func (s *Shop) OddCancelJob(job map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.oddJobSet = true
	if job == nil {
		s.oddJob = nil
		return
	}
	o := obj(job)
	s.oddJob = &o
}

// FailCancelJob makes the next cancel job end done without cancelling (the
// gateway refund failed).
func (s *Shop) FailCancelJob() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failJob = true
}

// FinishCancelJobs is time passing: every running cancel job completes.
func (s *Shop) FinishCancelJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		s.complete(j)
	}
}

// BeforeMutation runs f once, just before the next mutation of that operation
// reaches the shop: somebody else acting between the adapter's read and write.
func (s *Shop) BeforeMutation(operation string, f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks[operation] = f
}

// SuggestTransactionCurrency makes the suggested transactions' shopMoney carry
// another currency than the shop's.
func (s *Shop) SuggestTransactionCurrency(cur string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txnCurrency = cur
}

// JobPolls is how often the adapter asked for a job's state.
func (s *Shop) JobPolls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reqs {
		if r.Op == "CancelJob" {
			n++
		}
	}
	return n
}

// complete ends a job: the order is cancelled unless the job fails.
func (s *Shop) complete(j *cancelJob) {
	if j.finish {
		return
	}
	j.finish, j.left = true, 0
	delete(s.pending, j.order)
	if j.fail {
		return
	}
	if o := s.orderByID(j.order); o != nil {
		s.applyCancel(o, j.args)
	}
}

func (s *Shop) opCancelJob(req gqlRequest) (any, string) {
	j := s.jobs[s.str(req, "id")]
	if j == nil {
		return obj{"job": nil}, ""
	}
	if !j.finish {
		if j.left--; j.left <= 0 {
			s.complete(j)
		}
	}
	return obj{"job": obj{"id": s.str(req, "id"), "done": j.finish}}, ""
}

func (s *Shop) opOrderCancel(req gqlRequest) (any, string) {
	var reason string
	var restock, refund bool
	var notify *bool
	id := s.str(req, "orderId")
	s.vars(req, "reason", &reason)
	s.vars(req, "restock", &restock)
	s.vars(req, "refund", &refund)
	s.vars(req, "notifyCustomer", &notify)
	note := s.str(req, "staffNote")
	bad := func() (any, string) {
		return obj{"orderCancel": obj{"job": nil, "orderCancelUserErrors": []obj{{"field": []string{"orderId"}}}, "userErrors": []obj{}}}, ""
	}
	o := s.orderByID(id)
	if o == nil {
		return bad()
	}
	if _, running := s.pending[id]; running {
		return obj{"orderCancel": obj{"job": nil, "orderCancelUserErrors": []obj{{"field": []string{"orderId"}, "message": "A cancel job is already in progress for this order."}}, "userErrors": []obj{}}}, ""
	}
	if !cancelReasons[reason] || o.CancelledAt != "" || s.noCancel[id] {
		return bad()
	}
	switch o.Ful {
	case "FULFILLED", "PARTIALLY_FULFILLED", "IN_PROGRESS":
		return bad()
	}
	if s.oddJobSet {
		s.oddJobSet = false
		var job any
		if s.oddJob != nil {
			job = *s.oddJob
		}
		return obj{"orderCancel": obj{"job": job, "orderCancelUserErrors": []obj{}, "userErrors": []obj{}}}, ""
	}
	rec := CancelRecord{Reason: reason, Restock: restock, Refund: refund, Notify: notify, StaffNote: note}
	jobID := "gid://shopify/Job/1"
	if s.cancelPolls > 0 || s.failJob {
		jobID = gid("Job", 100+len(s.jobs))
		s.jobs[jobID] = &cancelJob{order: id, left: s.cancelPolls, fail: s.failJob, args: rec}
		s.pending[id] = jobID
		if s.cancelPolls == 0 { // done at once, but without the effect
			s.complete(s.jobs[jobID])
		}
		s.cancelPolls, s.failJob = 0, false
		return obj{"orderCancel": obj{"job": obj{"id": jobID, "done": s.jobs[jobID].finish}, "orderCancelUserErrors": []obj{}, "userErrors": []obj{}}}, ""
	}
	s.applyCancel(o, rec)
	return obj{"orderCancel": obj{"job": obj{"id": jobID, "done": true}, "orderCancelUserErrors": []obj{}, "userErrors": []obj{}}}, ""
}

// applyCancel is the effect of a cancel: the order is cancelled and, with
// refund, its unrefunded units refunded (and restocked when asked).
func (s *Shop) applyCancel(o *Order, rec CancelRecord) {
	o.CancelledAt = Base.Add(72 * time.Hour).Format(time.RFC3339)
	for i := range o.FOs {
		o.FOs[i].Status = "CANCELLED"
	}
	if rec.Refund {
		done := refundedUnits(o)
		var rl []refundLine
		for _, l := range o.Lines {
			if left := l.Qty - done[l.ID]; left > 0 {
				rt := "NO_RESTOCK"
				if rec.Restock {
					rt = "CANCEL"
					if lv := s.levels[l.VariantID+"@"+o.FOs[0].Loc]; lv != nil {
						lv.Qty += left
						lv.UpdatedAt = s.bump()
					}
				}
				rl = append(rl, refundLine{LineID: l.ID, Qty: left, RestockType: rt})
			}
		}
		if len(rl) > 0 {
			o.Refunds = append(o.Refunds, rl)
		}
		o.RefundTotal = o.Total
		o.Fin = "REFUNDED"
	}
	o.UpdatedAt = s.bump()
	rec.HadJobDone = true
	s.cancels[o.ID] = rec
	s.acts["cancel:"+o.ID]++
}
