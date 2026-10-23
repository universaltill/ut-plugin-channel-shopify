//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// order is the contract's order record as the cloud receives it. Decoding is
// strict (see decodeOrders) so a field the contract does not name fails.
type order struct {
	ExternalID          string `json:"external_id"`
	ExternalVersion     string `json:"external_version"`
	OrderNumber         string `json:"order_number"`
	PlacedAt            string `json:"placed_at"`
	Currency            string `json:"currency"`
	Status              string `json:"status"`
	PaymentState        string `json:"payment_state"`
	ShippingMinor       int64  `json:"shipping_minor"`
	ShippingTaxBP       int64  `json:"shipping_tax_bp"`
	ShippingTaxIncluded bool   `json:"shipping_tax_included"`
	OrderDiscountMinor  int64  `json:"order_discount_minor"`
	TaxAddedMinor       int64  `json:"tax_added_minor"`
	RefundTotalMinor    int64  `json:"refund_total_minor"`
	TotalMinor          int64  `json:"total_minor"`
	Lines               []struct {
		ExternalID        string `json:"external_id"`
		VariantExternalID string `json:"variant_external_id"`
		SKU               string `json:"sku"`
		Name              string `json:"name"`
		Qty               int64  `json:"qty"`
		UnitPriceMinor    int64  `json:"unit_price_minor"`
		TaxBP             int64  `json:"tax_bp"`
		TaxIncluded       bool   `json:"tax_included"`
		DiscountLineMinor int64  `json:"discount_line_minor"`
		RefundedQuantity  int64  `json:"refunded_quantity"`
		RestockedQuantity int64  `json:"restocked_quantity"`
	} `json:"lines"`
	Customer *struct {
		ExternalID string `json:"external_id"`
		Country    string `json:"country"`
	} `json:"customer"`
}

func (e *env) orders() []order {
	e.t.Helper()
	var out []order
	for _, raw := range e.submitted(channel.KindOrder) {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		var o order
		if err := dec.Decode(&o); err != nil {
			e.t.Fatalf("an order record carries a field the contract does not name: %v\n%s", err, raw)
		}
		out = append(out, o)
	}
	return out
}

// total is the contract's formula (section 5.1).
func (o order) formula() int64 {
	var t int64
	for _, l := range o.Lines {
		t += l.Qty*l.UnitPriceMinor - l.DiscountLineMinor
	}
	return t + o.ShippingMinor - o.OrderDiscountMinor + o.TaxAddedMinor
}

func TestOrdersAreSubmittedWithoutPersonalData(t *testing.T) {
	for _, excl := range []bool{false, true} {
		t.Run(fmt.Sprintf("taxes_excluded=%v", excl), func(t *testing.T) {
			e := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
			if n := e.tick(); n != 3 {
				t.Fatalf("%d orders", n)
			}
			orders := e.orders()
			if len(orders) != 3 {
				t.Fatalf("%d records", len(orders))
			}
			raw := ""
			for _, r := range e.submitted(channel.KindOrder) {
				raw += string(r)
			}
			for k, v := range e.em.Host.Cursors {
				raw += k + string(v)
			}
			for _, l := range e.em.Host.Logs {
				b, _ := json.Marshal(l)
				raw += string(b)
			}
			for _, pii := range []string{"Invented Person", "invented@example.invalid", "example.invalid", "Invented Street", "+440000000000", "displayName", "address1"} {
				if strings.Contains(raw, pii) {
					t.Errorf("the buyer's %q reached a record, cursor or log", pii)
				}
			}
			// The queries do not even ask for it.
			for _, r := range e.shop.Requests() {
				if r.Op == "Contact" {
					t.Error("a tick asked for contact details")
				}
				for _, f := range []string{"email", "phone", "firstName", "lastName", "address1", "displayName", "zip"} {
					if strings.Contains(r.Query, f) {
						t.Errorf("operation %s selects %q", r.Op, f)
					}
				}
			}
			for _, o := range orders {
				if o.TotalMinor != o.formula() {
					t.Errorf("%s: total %d, contract formula says %d", o.OrderNumber, o.TotalMinor, o.formula())
				}
				if o.Customer == nil || o.Customer.Country != "GB" || !strings.HasPrefix(o.Customer.ExternalID, "gid://shopify/Customer/") {
					t.Errorf("%s: customer %+v", o.OrderNumber, o.Customer)
				}
				if o.Currency != "GBP" || o.Status != "new" || o.PaymentState != "paid" || o.ExternalVersion == "" || o.PlacedAt == "" || o.ShippingTaxBP != 2000 || o.ShippingTaxIncluded == excl {
					t.Errorf("%s: %+v", o.OrderNumber, o)
				}
				if excl != (o.TaxAddedMinor > 0) {
					t.Errorf("%s: tax_added_minor %d with taxes excluded=%v", o.OrderNumber, o.TaxAddedMinor, excl)
				}
				for _, l := range o.Lines {
					if l.TaxBP != 2000 || l.TaxIncluded == excl || l.SKU == "" || l.Name == "" || !strings.HasPrefix(l.VariantExternalID, "gid://shopify/ProductVariant/") {
						t.Errorf("%s: line %+v", o.OrderNumber, l)
					}
				}
			}
			// Order 2 carries a line discount of 1.00 for the whole line.
			if orders[1].Lines[0].DiscountLineMinor != 100 || orders[1].Lines[0].Qty != 2 {
				t.Errorf("line discount %+v", orders[1].Lines[0])
			}
			if e.shop.Writes() != 0 {
				t.Error("a tick wrote to the shop")
			}
		})
	}
}

func TestOrderStatusIsWhatShopifyReports(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	ids := []string{"gid://shopify/Order/5001", "gid://shopify/Order/5002", "gid://shopify/Order/5003"}
	e.shop.FulfilExternally(ids[0])
	e.shop.CancelOrder(ids[1])
	e.shop.RefundLine(ids[2], 0, 3, "RETURN") // all 3 units: refunded in full (the shipping is not)
	e.tick()
	got := map[string]order{}
	for _, o := range e.orders() {
		got[o.ExternalID] = o
	}
	if got[ids[0]].Status != "shipped" || got[ids[1]].Status != "cancelled" {
		t.Errorf("statuses %q %q", got[ids[0]].Status, got[ids[1]].Status)
	}
	if s := got[ids[2]].Status; s != "partially_refunded" || got[ids[2]].PaymentState != "partially_refunded" {
		t.Errorf("order 3 status %q payment %q (shipping was not refunded)", s, got[ids[2]].PaymentState)
	}
}

func TestOrderEditAndRestockedRefundArePassedThroughNeverLoweringQty(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	id := "gid://shopify/Order/5001" // 1 unit
	e.shop.EditAddUnits(id, 0, 1)    // an order edit adds a unit: qty 2
	e.shop.RefundLine(id, 0, 1, "RETURN")
	e.tick()
	o := lineOf(t, e, id)
	if o.Qty != 2 || o.RefundedQuantity != 1 || o.RestockedQuantity != 1 {
		t.Fatalf("line %+v: want qty 2 (never lowered), refunded 1, restocked 1", o)
	}
	// A refund without restock is not stock coming back.
	e2 := newEnv(t, fakeshop.Options{})
	e2.shop.EditAddUnits(id, 0, 1)
	e2.shop.RefundLine(id, 0, 1, "NO_RESTOCK")
	e2.tick()
	if o := lineOf(t, e2, id); o.Qty != 2 || o.RefundedQuantity != 1 || o.RestockedQuantity != 0 {
		t.Fatalf("no-restock refund: %+v", o)
	}
	// An order edit that removed a unit shows as refunded, never as a lower qty.
	// On a PAID order Shopify also creates the refund line for the removed unit:
	// the unit is counted once, not twice.
	e3 := newEnv(t, fakeshop.Options{})
	e3.shop.EditRemoveUnits("gid://shopify/Order/5002", 0, 1) // 2 ordered, 1 removed, paid: refund line too
	e3.tick()
	if o := lineOf(t, e3, "gid://shopify/Order/5002"); o.Qty != 2 || o.RefundedQuantity != 1 || o.RestockedQuantity != 0 {
		t.Fatalf("removed unit on a paid order: %+v, want refunded 1 (counted once)", o)
	}
	// On an UNPAID order the edit leaves no refund line: the removal alone counts.
	e4 := newEnv(t, fakeshop.Options{})
	e4.shop.MutateOrder("gid://shopify/Order/5002", func(o *fakeshop.Order) { o.Fin = "PENDING" })
	e4.shop.EditRemoveUnits("gid://shopify/Order/5002", 0, 1)
	e4.tick()
	if o := lineOf(t, e4, "gid://shopify/Order/5002"); o.Qty != 2 || o.RefundedQuantity != 1 || o.RestockedQuantity != 0 {
		t.Fatalf("removed unit on an unpaid order: %+v", o)
	}
}

func lineOf(t *testing.T, e *env, orderID string) (l struct {
	Qty, RefundedQuantity, RestockedQuantity int64
}) {
	t.Helper()
	for _, o := range e.orders() {
		if o.ExternalID == orderID {
			l.Qty, l.RefundedQuantity, l.RestockedQuantity = o.Lines[0].Qty, o.Lines[0].RefundedQuantity, o.Lines[0].RestockedQuantity
		}
	}
	return l
}

// tickAll ticks until a sweep has finished (the window moved, no place kept),
// and returns how many asks it took.
func (e *env) tickAll() (asks int) {
	e.t.Helper()
	for asks = 1; asks <= 60; asks++ {
		e.tick()
		if string(e.em.Host.Cursors["orders.after"]) == "" {
			return asks
		}
	}
	e.t.Fatal("the sweep never finished")
	return 0
}

func TestTickPagesThroughManyOrdersAndAdvancesTheCursor(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.AddOrders(10)
	if asks := e.tickAll(); asks < 2 {
		t.Fatalf("13 orders were read in %d ask: one ask must not read more than its budget", asks)
	}
	seen := map[string]bool{}
	for _, o := range e.orders() {
		seen[o.ExternalID] = true
	}
	if len(seen) != 13 {
		t.Fatalf("%d distinct orders", len(seen))
	}
	since := string(e.em.Host.Cursors["orders.since"])
	if since == "" {
		t.Fatal("the cursor did not advance")
	}
	e.em.Host.Submitted = nil
	// The window is inclusive: the newest order comes again with the same
	// version, so the host's idempotency gives zero changes.
	if n := e.tick(); n != 1 {
		t.Fatalf("second tick re-read %d orders, want only the newest (inclusive window)", n)
	}
	// A change after the cursor is picked up.
	e.shop.FulfilExternally("gid://shopify/Order/5001")
	e.em.Host.Submitted = nil
	if n := e.tick(); n != 2 {
		t.Fatalf("tick after a change read %d orders, want 2", n)
	}
}

func TestTickResumesWithinItsBudgetWithoutSkippingOrders(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.AddOrders(80) // 83 orders, 2 per page, 2 pages per ask
	n1 := e.tick()
	if want := app.OrderPage * app.MaxOrderPages; n1 != want {
		t.Fatalf("first tick read %d, want %d", n1, want)
	}
	if c := askCost(e, 0); c > app.AskCostBudget {
		t.Fatalf("one tick requested %d points: more than the %d the 30 s deadline allows", c, app.AskCostBudget)
	}
	if string(e.em.Host.Cursors["orders.since"]) != "" || string(e.em.Host.Cursors["orders.after"]) == "" || string(e.em.Host.Cursors["orders.newest"]) == "" {
		t.Fatalf("an unfinished sweep must keep its place and must not move the window: %v", e.em.Host.Cursors)
	}
	e.tickAll()
	if string(e.em.Host.Cursors["orders.since"]) == "" || string(e.em.Host.Cursors["orders.after"]) != "" {
		t.Fatalf("a finished sweep moves the window and clears its place: %v", e.em.Host.Cursors)
	}
	seen := map[string]bool{}
	for _, o := range e.orders() {
		seen[o.ExternalID] = true
	}
	if len(seen) != 83 {
		t.Fatalf("%d distinct orders after resuming, want 83 (none skipped)", len(seen))
	}
}

func TestOrderWithMoreLinesThanOnePageIsCompleted(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	id := "gid://shopify/Order/5001"
	e.shop.MutateOrder(id, func(o *fakeshop.Order) {
		for i := 0; i < 24; i++ { // 25 lines: past one page of 20
			l := o.Lines[0]
			l.ID = fmt.Sprintf("gid://shopify/LineItem/%d", 7000+i)
			o.Lines = append(o.Lines, l)
		}
	})
	e.tick()
	for _, o := range e.orders() {
		if o.ExternalID == id && len(o.Lines) != 25 {
			t.Fatalf("%d lines, want 25 (the second page of lines was not followed)", len(o.Lines))
		}
	}
}

func TestFetchReadsTheNamedOrdersAndIgnoresBadOnes(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	ids := []string{"gid://shopify/Order/5002", "gid://shopify/Order/999999", "../../admin", "gid://shopify/Order/5002?x=1", "gid://shopify/Product/1001", ""}
	e.ask(channel.AskFetch, channel.FetchInput{Event: channel.EventOrderChanged, ResourceIDs: ids})
	got := e.orders()
	if len(got) != 1 || got[0].ExternalID != "gid://shopify/Order/5002" {
		t.Fatalf("%+v", got)
	}
	for _, r := range e.shop.Requests() {
		if strings.Contains(string(mustJSON(r.Variables)), "../") || strings.Contains(string(mustJSON(r.Variables)), "?x=1") {
			t.Errorf("a bad id reached a request: %v", r.Variables)
		}
	}
	if r := e.em.Ask(channel.AskFetch, channel.FetchInput{ResourceIDs: make([]string, 101)}); r.Err == nil {
		t.Error("101 resources accepted")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestContactIsFetchedLiveAndNeverStored(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	var c channel.ContactResult
	in := channel.ContactInput{OrderExternalID: "gid://shopify/Order/5001"}
	if err := json.Unmarshal(e.ask(channel.AskContact, in), &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "Invented Person" || c.Email != "invented@example.invalid" || c.Phone != "+440000000000" || !strings.Contains(c.Address, "Invented Street") {
		t.Fatalf("%+v", c)
	}
	if len(e.em.Host.Submitted) != 0 || len(e.em.Host.CursorKeys()) != 0 || len(e.em.Host.Logs) != 0 {
		t.Fatal("contact details were stored, submitted or logged")
	}
	before := len(e.shop.Requests())
	for _, bad := range []string{"", "../../admin", "gid://shopify/Order/5001/../x", "gid://shopify/Customer/9001"} {
		in.OrderExternalID = bad
		if r := e.em.Ask(channel.AskContact, in); r.Err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if len(e.shop.Requests()) != before {
		t.Error("an invalid order id reached the shop")
	}
}

func TestOrderLevelDiscountCodeSatisfiesTheContractFormula(t *testing.T) {
	for _, excl := range []bool{false, true} {
		t.Run(fmt.Sprintf("taxes_excluded=%v", excl), func(t *testing.T) {
			e := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
			id := "gid://shopify/Order/5002"        // 2 units, line discount 1.00
			e.shop.SetOrderCodeDiscount(id, 0, 150) // a 1.50 share of an order-level code
			e.tick()
			for _, o := range e.orders() {
				if o.TotalMinor != o.formula() {
					t.Errorf("%s: total %d, contract formula says %d (%+v)", o.OrderNumber, o.TotalMinor, o.formula(), o.Lines)
				}
				if o.ExternalID == id && (o.Lines[0].DiscountLineMinor != 250 || o.OrderDiscountMinor != 0) {
					t.Errorf("line discount %d, order discount %d: want the line's own 100 plus the code's 150 on the line, 0 at order level",
						o.Lines[0].DiscountLineMinor, o.OrderDiscountMinor)
				}
			}
		})
	}
}

func TestMoreRefundsThanTheQueryReadsRefuseTheOrderAndLogWhy(t *testing.T) {
	const id = "gid://shopify/Order/5003"
	run := func(refunds, linesInOne int) (*env, []order) {
		e := newEnv(t, fakeshop.Options{})
		e.shop.EditAddUnits(id, 0, 30) // 33 units: enough to refund
		for i := 0; i < refunds; i++ {
			e.shop.RefundLine(id, 0, 1, "RETURN")
		}
		if linesInOne > 0 {
			e.shop.RefundLines(id, 0, linesInOne)
		}
		e.tick()
		return e, e.orders()
	}
	has := func(e *env, code string) bool {
		for _, l := range e.em.Host.Logs {
			if l.Code == code {
				return true
			}
		}
		return false
	}
	has5 := func(os []order) *order {
		for i := range os {
			if os[i].ExternalID == id {
				return &os[i]
			}
		}
		return nil
	}
	// At the cap everything is read.
	if e, os := run(5, 0); has5(os) == nil || has5(os).Lines[0].RefundedQuantity != 5 || has(e, "order.refused.too_many_refunds") {
		t.Fatalf("5 refunds: %+v", has5(os))
	}
	if e, os := run(0, 20); has5(os) == nil || has5(os).Lines[0].RefundedQuantity != 20 || has(e, "order.refused.too_many_refunds") {
		t.Fatalf("20 refund lines: %+v", has5(os))
	}
	// One more than the cap: the order is refused, never submitted with a short count.
	if e, os := run(6, 0); has5(os) != nil || !has(e, "order.refused.too_many_refunds") {
		t.Fatalf("6 refunds: submitted %+v, logged=%v", has5(os), has(e, "order.refused.too_many_refunds"))
	}
	if e, os := run(0, 21); has5(os) != nil || !has(e, "order.refused.too_many_refunds") {
		t.Fatalf("21 refund lines: submitted %+v, logged=%v", has5(os), has(e, "order.refused.too_many_refunds"))
	}
}

func TestPaymentStatesThatAreNotPaidAreNotReportedAsPaid(t *testing.T) {
	for fin, want := range map[string]string{"PARTIALLY_PAID": "pending", "EXPIRED": "voided", "PENDING": "pending", "VOIDED": "voided", "PAID": "paid"} {
		e := newEnv(t, fakeshop.Options{})
		e.shop.MutateOrder("gid://shopify/Order/5001", func(o *fakeshop.Order) { o.Fin = fin })
		e.tick()
		for _, o := range e.orders() {
			if o.ExternalID == "gid://shopify/Order/5001" && o.PaymentState != want {
				t.Errorf("%s: payment_state %q, want %q", fin, o.PaymentState, want)
			}
		}
	}
}
