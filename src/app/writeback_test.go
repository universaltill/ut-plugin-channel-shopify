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

// order.cancel and refund.create (ut-docs#4774, cloud half ut-docs#4771).
// Orders of the fake shop: 5001 one unit at 6.10, 5002 two units at 7.10 less
// a 1.00 line discount (6.60 a unit), 5003 three units at 8.10; prices
// include tax. Every order is paid and unfulfilled at location 7001.

const (
	o1, o2, o3 = "gid://shopify/Order/5001", "gid://shopify/Order/5002", "gid://shopify/Order/5003"
	l1, l2, l3 = "gid://shopify/LineItem/6001", "gid://shopify/LineItem/6002", "gid://shopify/LineItem/6003"
)

func cancelOp(id, order, payload string) channel.OutboxOp {
	return channel.OutboxOp{ID: id, Kind: channel.OpOrderCancel, ExternalID: order, Payload: json.RawMessage(payload)}
}

func refundOp(id, order, payload string) channel.OutboxOp {
	return channel.OutboxOp{ID: id, Kind: channel.OpRefundCreate, ExternalID: order, Payload: json.RawMessage(payload)}
}

// refundJSON builds a refund.create payload; lines are {line, quantity,
// refunded_to, restocked_to}.
func refundJSON(order, cur string, max int64, lines ...[4]any) string {
	var ls []string
	for _, l := range lines {
		ls = append(ls, fmt.Sprintf(`{"line_external_id":%q,"quantity":%d,"refunded_to":%d,"restocked_to":%d}`, l[0], l[1], l[2], l[3]))
	}
	return fmt.Sprintf(`{"external_order_id":%q,"reason":"customer_request","restock":true,"currency":%q,"max_amount_minor":%d,"lines":[%s]}`, order, cur, max, strings.Join(ls, ","))
}

func (e *env) count(op string) (n int) {
	for _, r := range e.shop.Requests() {
		if r.Op == op {
			n++
		}
	}
	return n
}

func wantAck(t *testing.T, a channel.Ack, code string, retryable bool) {
	t.Helper()
	if a.Error == nil || a.Error.Code != code || a.Error.Retryable != retryable {
		t.Fatalf("ack %+v, want error %s retryable=%v", a, code, retryable)
	}
}

// --- order.cancel -----------------------------------------------------------

func TestCancelReachesShopifyOnceAndRestocksPerTheFlag(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	loc := e.shop.Locations()[0]
	before := e.shop.StockOfLine(o3, 0, loc)
	a := e.outbox(cancelOp("c-1", o3, fmt.Sprintf(`{"external_order_id":%q,"reason":"out_of_stock","restock":true}`, o3)))["c-1"]
	if a.Error != nil || a.ExternalID != o3 {
		t.Fatalf("ack %+v", a)
	}
	rec, ok := e.shop.Cancel(o3)
	if !ok || rec.Reason != "INVENTORY" || !rec.Restock || !rec.Refund || rec.Notify == nil || *rec.Notify || rec.StaffNote == "" {
		t.Fatalf("cancel carried %+v (want INVENTORY, restock, notifyCustomer false, a constant staff note)", rec)
	}
	if got := e.shop.StockOfLine(o3, 0, loc); got != before+3 {
		t.Fatalf("stock %d, want %d (the unrefunded units went back)", got, before+3)
	}
	// restock false: nothing goes back.
	before = e.shop.StockOfLine(o2, 0, loc)
	if a := e.outbox(cancelOp("c-2", o2, fmt.Sprintf(`{"external_order_id":%q,"reason":"fraud","restock":false}`, o2)))["c-2"]; a.Error != nil {
		t.Fatalf("ack %+v", a)
	}
	if rec, _ := e.shop.Cancel(o2); rec.Reason != "FRAUD" || rec.Restock {
		t.Fatalf("cancel carried %+v", rec)
	}
	if got := e.shop.StockOfLine(o2, 0, loc); got != before {
		t.Fatalf("stock %d, want %d: restock was false", got, before)
	}
}

func TestCancelReasonsMapToTheChannelsAndUnknownOnesAreRefused(t *testing.T) {
	for in, want := range map[string]string{"customer_request": "CUSTOMER", "out_of_stock": "INVENTORY", "fraud": "FRAUD", "other": "OTHER"} {
		e := newEnv(t, fakeshop.Options{})
		if a := e.outbox(cancelOp("c", o1, fmt.Sprintf(`{"reason":%q,"restock":false}`, in)))["c"]; a.Error != nil {
			t.Fatalf("%s: %+v", in, a)
		}
		if rec, _ := e.shop.Cancel(o1); rec.Reason != want {
			t.Errorf("%s mapped to %q, want %s", in, rec.Reason, want)
		}
	}
	e := newEnv(t, fakeshop.Options{})
	for i, p := range []string{`{"reason":"because"}`, `{"restock":true}`, `{"reason":"CUSTOMER"}`, `"nope"`, `{"reason":"other","external_order_id":"gid://shopify/Order/5002"}`} {
		wantAck(t, e.outbox(cancelOp(fmt.Sprintf("bad-%d", i), o1, p))[fmt.Sprintf("bad-%d", i)], "invalid_payload", false)
	}
	wantAck(t, e.outbox(cancelOp("h", "../../admin", `{"reason":"other"}`))["h"], "invalid_payload", false)
	if len(e.shop.Requests()) != 0 {
		t.Fatalf("a refused payload reached the shop: %d requests", len(e.shop.Requests()))
	}
}

func TestCancelAlreadyCancelledIsSuccessAndRedeliveryActsOnce(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p := fmt.Sprintf(`{"external_order_id":%q,"reason":"other","restock":true}`, o1)
	e.outbox(cancelOp("c-1", o1, p))
	if a := e.outbox(cancelOp("c-1", o1, p))["c-1"]; a.Error != nil || e.shop.Performed("cancel", o1) != 1 || e.count("OrderCancel") != 1 {
		t.Fatalf("redelivery %+v performed %d, mutations %d", a, e.shop.Performed("cancel", o1), e.count("OrderCancel"))
	}
	// The merchant cancelled in Shopify's admin: success, no mutation sent.
	e.shop.CancelOrder(o2)
	if a := e.outbox(cancelOp("c-2", o2, `{"reason":"other"}`))["c-2"]; a.Error != nil || a.ExternalID != o2 || e.shop.Performed("cancel", o2) != 0 || e.count("OrderCancel") != 1 {
		t.Fatalf("already cancelled %+v performed %d mutations %d", a, e.shop.Performed("cancel", o2), e.count("OrderCancel"))
	}
}

func TestCancelOfAFulfilledOrderIsRefusedBeforeAnyMutation(t *testing.T) {
	for _, st := range []string{"FULFILLED", "PARTIALLY_FULFILLED", "IN_PROGRESS"} {
		e := newEnv(t, fakeshop.Options{})
		e.shop.SetFulfilmentStatus(o1, st)
		wantAck(t, e.outbox(cancelOp("c", o1, `{"reason":"other","restock":true}`))["c"], "order_fulfilled", false)
		if e.count("OrderCancel") != 0 || e.shop.Performed("cancel", o1) != 0 {
			t.Errorf("%s: a mutation was sent for an order Shopify cannot cancel", st)
		}
	}
	e := newEnv(t, fakeshop.Options{})
	wantAck(t, e.outbox(cancelOp("n", "gid://shopify/Order/424242", `{"reason":"other"}`))["n"], "not_found", false)
}

func TestCancelThatShopifyRefusesIsDeadLetteredAndASchemaErrorToo(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.RefuseCancel(o1)
	wantAck(t, e.outbox(cancelOp("c", o1, `{"reason":"other"}`))["c"], "rejected_by_channel", false)
	e = newEnv(t, fakeshop.Options{})
	e.shop.SchemaError("OrderCancel")
	wantAck(t, e.outbox(cancelOp("c", o1, `{"reason":"other"}`))["c"], "rejected_by_channel", false)
	if e.count("OrderCancel") != 1 {
		t.Errorf("the schema error was sent %d times", e.count("OrderCancel"))
	}
}

func TestCancelWhoseAnswerWasLostIsFoundCancelledOnTheRetry(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.LoseAnswer("OrderCancel", 1)
	p := `{"reason":"customer_request","restock":true}`
	wantAck(t, e.outbox(cancelOp("c-1", o3, p))["c-1"], "channel_error", true)
	if e.shop.Performed("cancel", o3) != 1 {
		t.Fatalf("the effect should have happened once, got %d", e.shop.Performed("cancel", o3))
	}
	if a := e.outbox(cancelOp("c-1", o3, p))["c-1"]; a.Error != nil || e.shop.Performed("cancel", o3) != 1 || e.count("OrderCancel") != 1 {
		t.Fatalf("retry %+v performed %d mutations %d: read-first must find it cancelled", a, e.shop.Performed("cancel", o3), e.count("OrderCancel"))
	}
	// No Idempotency-Key on a cancel: the host must not re-send a POST that may have acted.
	for _, r := range e.shop.Requests() {
		if r.Op == "OrderCancel" && r.Header.Get("Idempotency-Key") != "" {
			t.Error("OrderCancel carries an Idempotency-Key")
		}
	}
}

// --- refund.create ----------------------------------------------------------

func TestRefundByQuantityRestocksAndMovesTheMoneyOnce(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	loc := e.shop.Locations()[0]
	before := e.shop.StockOfLine(o3, 0, loc)
	p := refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 2})
	a := e.outbox(refundOp("r-1", o3, p))["r-1"]
	if a.Error != nil || a.ExternalID != o3 {
		t.Fatalf("ack %+v", a)
	}
	rs := e.shop.Refunds(o3)
	if len(rs) != 1 || rs[0].Lines[l3] != 2 || rs[0].RestockType[l3] != "CANCEL" || rs[0].Amount != 1620 || rs[0].Notify == nil || *rs[0].Notify {
		t.Fatalf("refunds %+v", rs)
	}
	if strings.Contains(strings.ToLower(rs[0].Note), "invented") || rs[0].Note == "" {
		t.Errorf("note %q: a constant text, never customer text", rs[0].Note)
	}
	if got := e.shop.StockOfLine(o3, 0, loc); got != before+2 || e.shop.RefundTotal(o3) != 1620 {
		t.Fatalf("stock %d (want %d), refunded %d", got, before+2, e.shop.RefundTotal(o3))
	}
	// Redelivered after a lost ack: every line already shows refunded >= refunded_to.
	if a := e.outbox(refundOp("r-1", o3, p))["r-1"]; a.Error != nil || e.count("RefundCreate") != 1 || e.shop.Performed("refund", o3) != 1 {
		t.Fatalf("redelivery %+v mutations %d performed %d", a, e.count("RefundCreate"), e.shop.Performed("refund", o3))
	}
}

func TestRefundNeverSendsMoreThanItIsOwedWhenTheShopRefundedSomeItself(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.RefundLine(o3, 0, 1, "NO_RESTOCK") // the merchant refunded one unit in Shopify's admin
	// The cloud asked for two more on a baseline of zero: refunded_to 2.
	a := e.outbox(refundOp("r-1", o3, refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 0})))["r-1"]
	if a.Error != nil {
		t.Fatalf("ack %+v", a)
	}
	rs := e.shop.Refunds(o3)
	if len(rs) != 1 || rs[0].Lines[l3] != 1 || rs[0].RestockType[l3] != "NO_RESTOCK" {
		t.Fatalf("refunds %+v: one unit was already refunded, only one is owed", rs)
	}
	// Everything already there (refunded_to 1): success without a request that writes.
	e2 := newEnv(t, fakeshop.Options{})
	e2.shop.RefundLine(o3, 0, 2, "RETURN")
	if a := e2.outbox(refundOp("r-2", o3, refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 2})))["r-2"]; a.Error != nil || e2.count("RefundCreate") != 0 || e2.count("SuggestedRefund") != 0 {
		t.Fatalf("done by the shop: %+v mutations %d suggestions %d", a, e2.count("RefundCreate"), e2.count("SuggestedRefund"))
	}
}

func TestRefundIsNeverMoreThanTheQuantityAskedEvenWhenTheBaselineIsBehind(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	// The cloud believes two units were refunded already (refunded_to 3) but the
	// channel shows none: the staff asked for ONE unit.
	if a := e.outbox(refundOp("r-1", o3, refundJSON(o3, "GBP", 810, [4]any{l3, 1, 3, 0})))["r-1"]; a.Error != nil {
		t.Fatalf("ack %+v", a)
	}
	if rs := e.shop.Refunds(o3); len(rs) != 1 || rs[0].Lines[l3] != 1 {
		t.Fatalf("refunds %+v, want one unit", rs)
	}
}

func TestRefundRefusesWhenShopifysOwnSuggestionIsAboveTheCeilingEvenIfLessMovesAsCash(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.MutateOrder(o3, func(o *fakeshop.Order) { o.RefundTotal = o.Total - 100 }) // only 1.00 is left to move
	wantAck(t, e.outbox(refundOp("r-1", o3, refundJSON(o3, "GBP", 500, [4]any{l3, 1, 1, 0})))["r-1"], "refund_exceeds_ceiling", false)
	if e.count("RefundCreate") != 0 {
		t.Fatal("sent")
	}
}

func TestRefundOfAnOrderWithMoreRefundsThanTheQueryReadsIsRefusedNotGuessed(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	for i := 0; i < 6; i++ {
		e.shop.RefundLine(o3, 0, 0, "NO_RESTOCK") // six refunds on the order
	}
	wantAck(t, e.outbox(refundOp("r-1", o3, refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 0})))["r-1"], "too_many_refunds", false)
	if e.count("RefundCreate") != 0 {
		t.Fatal("sent")
	}
}

func TestRefundOfSeveralLinesIsOneRefundWithARestockTypePerLine(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	extra := e.shop.AddOrderLine(o3, 4, 250)
	loc := e.shop.Locations()[0]
	before := e.shop.StockOfLine(o3, 0, loc)
	// Line 6003: 1 unit restocked. Added line: 2 units, not restocked.
	p := refundJSON(o3, "GBP", 810+500, [4]any{l3, 1, 1, 1}, [4]any{extra, 2, 2, 0})
	if a := e.outbox(refundOp("r-1", o3, p))["r-1"]; a.Error != nil {
		t.Fatalf("ack %+v", a)
	}
	rs := e.shop.Refunds(o3)
	if len(rs) != 1 || e.count("RefundCreate") != 1 {
		t.Fatalf("refunds %d mutations %d, want one refund for the whole op", len(rs), e.count("RefundCreate"))
	}
	if rs[0].Lines[l3] != 1 || rs[0].Lines[extra] != 2 || rs[0].RestockType[l3] != "CANCEL" || rs[0].RestockType[extra] != "NO_RESTOCK" || rs[0].Amount != 1310 {
		t.Fatalf("refund %+v", rs[0])
	}
	if e.shop.StockOfLine(o3, 0, loc) != before+1 {
		t.Fatalf("stock %d, want %d", e.shop.StockOfLine(o3, 0, loc), before+1)
	}
}

func TestRefundOfAFulfilledOrderRestocksAsAReturnToTheFulfilmentLocation(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.FulfilExternally(o2)
	loc := e.shop.Locations()[0]
	before := e.shop.StockOfLine(o2, 0, loc)
	if a := e.outbox(refundOp("r-1", o2, refundJSON(o2, "GBP", 660, [4]any{l2, 1, 1, 1})))["r-1"]; a.Error != nil {
		t.Fatalf("ack %+v", a)
	}
	rs := e.shop.Refunds(o2)
	if len(rs) != 1 || rs[0].RestockType[l2] != "RETURN" || rs[0].LocationID[l2] != loc || rs[0].Amount != 660 || e.shop.StockOfLine(o2, 0, loc) != before+1 {
		t.Fatalf("refund %+v stock %d", rs, e.shop.StockOfLine(o2, 0, loc))
	}
	// Without restock a fulfilled order needs no location.
	e2 := newEnv(t, fakeshop.Options{Locations: 2})
	e2.shop.FulfilExternally(o2)
	e2.shop.AddFulfilmentOrder(o2, 2)
	e2.shop.SetFulfilmentOrderStatus(o2, "CLOSED")
	if a := e2.outbox(refundOp("r-2", o2, refundJSON(o2, "GBP", 660, [4]any{l2, 1, 1, 0})))["r-2"]; a.Error != nil {
		t.Fatalf("no restock, two locations: %+v", a)
	}
	// With restock and two locations: never guess.
	e3 := newEnv(t, fakeshop.Options{Locations: 2})
	e3.shop.FulfilExternally(o2)
	e3.shop.AddFulfilmentOrder(o2, 2)
	e3.shop.SetFulfilmentOrderStatus(o2, "CLOSED")
	wantAck(t, e3.outbox(refundOp("r-3", o2, refundJSON(o2, "GBP", 660, [4]any{l2, 1, 1, 1})))["r-3"], "location_required", false)
	if e3.count("RefundCreate") != 0 {
		t.Error("a refund was sent without knowing where to restock")
	}
	// A partly fulfilled order cannot say which units are where.
	e4 := newEnv(t, fakeshop.Options{})
	e4.shop.SetFulfilmentStatus(o2, "PARTIALLY_FULFILLED")
	wantAck(t, e4.outbox(refundOp("r-4", o2, refundJSON(o2, "GBP", 660, [4]any{l2, 1, 1, 1})))["r-4"], "restock_not_supported", false)
}

func TestRefundAboveTheCeilingIsRefusedBeforeSendingAndAtTheCeilingIsDone(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	a := e.outbox(refundOp("r-1", o3, refundJSON(o3, "GBP", 1619, [4]any{l3, 2, 2, 2})))["r-1"]
	wantAck(t, a, "refund_exceeds_ceiling", false)
	if e.count("RefundCreate") != 0 || e.shop.Writes() != 0 || e.shop.RefundTotal(o3) != 0 {
		t.Fatalf("something was written: mutations %d writes %d refunded %d", e.count("RefundCreate"), e.shop.Writes(), e.shop.RefundTotal(o3))
	}
	if a := e.outbox(refundOp("r-2", o3, refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 2})))["r-2"]; a.Error != nil || e.shop.RefundTotal(o3) != 1620 {
		t.Fatalf("at the ceiling: %+v refunded %d", a, e.shop.RefundTotal(o3))
	}
	// The ceiling is also a ceiling on what is actually sent: a tax-exclusive
	// shop's suggested amount carries tax the cloud's ceiling already counted.
	e2 := newEnv(t, fakeshop.Options{TaxesExcluded: true})
	wantAck(t, e2.outbox(refundOp("r-3", o3, refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 0})))["r-3"], "refund_exceeds_ceiling", false)
	if a := e2.outbox(refundOp("r-4", o3, refundJSON(o3, "GBP", 972, [4]any{l3, 1, 1, 0})))["r-4"]; a.Error != nil {
		t.Fatalf("tax-exclusive at its ceiling: %+v", a)
	}
}

func TestRefundRefusesACurrencyThatIsNotTheShops(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	wantAck(t, e.outbox(refundOp("r-1", o3, refundJSON(o3, "EUR", 1620, [4]any{l3, 2, 2, 2})))["r-1"], "currency_mismatch", false)
	if e.shop.Writes() != 0 {
		t.Fatal("a refund in another currency was sent")
	}
}

func TestRefundShopifyRefusesIsRejectedAndNeverRetried(t *testing.T) {
	// More than was paid: the shop's own check refuses the refund.
	e := newEnv(t, fakeshop.Options{})
	e.shop.MutateOrder(o3, func(o *fakeshop.Order) { o.RefundTotal = o.Total - 100 }) // refunded elsewhere, no line change
	e.shop.SuggestExtra(50)
	wantAck(t, e.outbox(refundOp("r-1", o3, refundJSON(o3, "GBP", 5000, [4]any{l3, 1, 1, 0})))["r-1"], "rejected_by_channel", false)
	if e.shop.Performed("refund", o3) != 0 {
		t.Fatal("a refund above what was paid went through")
	}
	// A schema error (wrong from-memory names) is a stable non-retryable code.
	for _, op := range []string{"SuggestedRefund", "RefundCreate", "RefundState"} {
		e := newEnv(t, fakeshop.Options{})
		e.shop.SchemaError(op)
		wantAck(t, e.outbox(refundOp("r", o3, refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 2})))["r"], "rejected_by_channel", false)
		if e.count(op) != 1 || e.shop.Performed("refund", o3) != 0 {
			t.Errorf("%s: sent %d times, performed %d", op, e.count(op), e.shop.Performed("refund", o3))
		}
	}
	// A cancelled order takes no refund.
	e2 := newEnv(t, fakeshop.Options{})
	e2.shop.CancelOrder(o3)
	wantAck(t, e2.outbox(refundOp("r", o3, refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 2})))["r"], "order_cancelled", false)
	wantAck(t, e2.outbox(refundOp("n", "gid://shopify/Order/424242", refundJSON("gid://shopify/Order/424242", "GBP", 1, [4]any{l3, 1, 1, 0})))["n"], "not_found", false)
}

func TestRefundWhoseAnswerWasLostIsFoundDoneOnTheRetry(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.LoseAnswer("RefundCreate", 1)
	p := refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 2})
	wantAck(t, e.outbox(refundOp("r-1", o3, p))["r-1"], "channel_error", true)
	if e.shop.Performed("refund", o3) != 1 || e.shop.RefundTotal(o3) != 1620 {
		t.Fatalf("the effect should have happened once: performed %d refunded %d", e.shop.Performed("refund", o3), e.shop.RefundTotal(o3))
	}
	if a := e.outbox(refundOp("r-1", o3, p))["r-1"]; a.Error != nil || e.shop.Performed("refund", o3) != 1 || e.shop.RefundTotal(o3) != 1620 || e.count("RefundCreate") != 1 {
		t.Fatalf("retry %+v performed %d refunded %d mutations %d", a, e.shop.Performed("refund", o3), e.shop.RefundTotal(o3), e.count("RefundCreate"))
	}
}

func TestRefundCarriesTheOpIDAsTheIdempotencyKeyAndNoInterpolation(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.outbox(refundOp("r-key-1", o3, refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 0})))
	n := 0
	for _, r := range e.shop.Requests() {
		if r.Op != "RefundCreate" {
			continue
		}
		var k string
		_ = json.Unmarshal(r.Variables["key"], &k)
		if k != "r-key-1" || r.Header.Get("Idempotency-Key") != "r-key-1" {
			t.Errorf("key %q header %q", k, r.Header.Get("Idempotency-Key"))
		}
		if strings.Contains(r.Query, "gid://") || strings.Contains(r.Query, "810") {
			t.Errorf("a value was interpolated: %.120s", r.Query)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("%d refund mutations", n)
	}
}

func TestRefundPayloadIsValidatedBeforeAnyRequestAndExtraFieldsAreTolerated(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	good := func(line string) string {
		return fmt.Sprintf(`{"external_order_id":%q,"reason":"other","restock":false,"currency":"GBP","max_amount_minor":810,"lines":[%s]}`, o3, line)
	}
	bad := map[string]string{
		"no max":             fmt.Sprintf(`{"external_order_id":%q,"reason":"other","currency":"GBP","lines":[{"line_external_id":%q,"quantity":1,"refunded_to":1,"restocked_to":0}]}`, o3, l3),
		"negative max":       strings.Replace(refundJSON(o3, "GBP", 5, [4]any{l3, 1, 1, 0}), `"max_amount_minor":5`, `"max_amount_minor":-5`, 1),
		"no lines":           good(""),
		"hostile line":       refundJSON(o3, "GBP", 810, [4]any{"../../x", 1, 1, 0}),
		"zero quantity":      refundJSON(o3, "GBP", 810, [4]any{l3, 0, 1, 0}),
		"target below qty":   refundJSON(o3, "GBP", 810, [4]any{l3, 2, 1, 0}),
		"restock above":      refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 2}),
		"duplicate line":     refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 0}, [4]any{l3, 1, 2, 0}),
		"huge quantity":      refundJSON(o3, "GBP", 810, [4]any{l3, 99999999999, 99999999999, 0}),
		"bad currency":       refundJSON(o3, "gbp", 810, [4]any{l3, 1, 1, 0}),
		"bad reason":         strings.Replace(refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 0}), "customer_request", "whim", 1),
		"another order":      refundJSON(o2, "GBP", 810, [4]any{l3, 1, 1, 0}),
		"not json":           `"nope"`,
		"line of wrong type": refundJSON(o3, "GBP", 810, [4]any{o3, 1, 1, 0}),
	}
	many := []string{}
	for i := 0; i < 101; i++ {
		many = append(many, fmt.Sprintf(`{"line_external_id":"gid://shopify/LineItem/%d","quantity":1,"refunded_to":1,"restocked_to":0}`, 7000+i))
	}
	bad["101 lines"] = good(strings.Join(many, ","))
	for name, p := range bad {
		wantAck(t, e.outbox(refundOp("v-"+strings.ReplaceAll(name, " ", "-"), o3, p))["v-"+strings.ReplaceAll(name, " ", "-")], "invalid_payload", false)
	}
	if len(e.shop.Requests()) != 0 {
		t.Fatalf("%d requests for refused payloads", len(e.shop.Requests()))
	}
	// Fields the hardening card adds (and fields nobody knows) are tolerated.
	rich := fmt.Sprintf(`{"external_order_id":%q,"reason":"other","restock":false,"currency":"GBP","max_amount_minor":810,"future":{"x":1},`+
		`"reported_refund_minor":0,"lines":[{"line_external_id":%q,"quantity":1,"refunded_to":1,"restocked_to":0,"unit_price_minor":810,"discount_line_minor":0,"extra":true}]}`, o3, l3)
	if a := e.outbox(refundOp("rich", o3, rich))["rich"]; a.Error != nil {
		t.Fatalf("extra fields: %+v", a)
	}
}

// --- shared discipline ------------------------------------------------------

func TestCancelAndRefundSelectNoPersonalData(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.outbox(cancelOp("c", o1, `{"reason":"other"}`), refundOp("r", o3, refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 0})))
	for _, r := range e.shop.Requests() {
		for _, bad := range []string{"email", "phone", "displayName", "address", "customer"} {
			if strings.Contains(r.Query, bad) {
				t.Errorf("%s selects %q", r.Op, bad)
			}
		}
	}
	for _, l := range e.em.Host.Logs {
		b, _ := json.Marshal(l)
		if strings.Contains(string(b), "Invented Person") || strings.Contains(string(b), "invented@") {
			t.Errorf("personal data in a log: %s", b)
		}
	}
	for _, a := range e.em.Host.Acks {
		b, _ := json.Marshal(a)
		if strings.Contains(string(b), "Invented") {
			t.Errorf("personal data in an ack: %s", b)
		}
	}
}

func TestCancelAndRefundOpsStayInsideTheDrainBudgetsAndEveryStartedOpIsAcked(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.AddOrders(60)
	var ids []string
	for _, o := range e.shop.Orders() {
		ids = append(ids, o.ID)
	}
	for i, id := range ids[3:] {
		var op channel.OutboxOp
		if i%2 == 0 {
			op = cancelOp(fmt.Sprintf("b-%03d", i), id, `{"reason":"other","restock":true}`)
		} else {
			line := strings.Replace(id, "Order/5", "LineItem/6", 1)
			op = refundOp(fmt.Sprintf("b-%03d", i), id, refundJSON(id, "GBP", 100000, [4]any{line, 1, 1, 1}))
		}
		e.em.Host.Outbox = append(e.em.Host.Outbox, op)
	}
	total := len(e.em.Host.Outbox)
	e.ask(channel.AskOutbox, struct{}{})
	acked := len(e.em.Host.Acks)
	if acked == 0 || acked >= total || acked+len(e.em.Host.Outbox) != total {
		t.Fatalf("acked %d pending %d of %d: it must do some, stop short and lose none", acked, len(e.em.Host.Outbox), total)
	}
	if got := len(e.em.Seen()); got > app.DrainHTTPBudget+4 {
		t.Errorf("%d requests, budget %d (+ the op that was started)", got, app.DrainHTTPBudget)
	}
	for id, a := range e.em.Host.Acks {
		if a.Error != nil {
			t.Errorf("%s: %+v", id, a)
		}
	}
}

func TestAnAckFailureAfterARefundStopsTheDrain(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.em.Host.Deny["outbox_ack"] = true
	e.em.Host.Outbox = append(e.em.Host.Outbox,
		refundOp("r-1", o3, refundJSON(o3, "GBP", 810, [4]any{l3, 1, 1, 0})),
		cancelOp("c-1", o1, `{"reason":"other"}`),
		cancelOp("c-2", o2, `{"reason":"other"}`))
	e.ask(channel.AskOutbox, struct{}{})
	if e.shop.Performed("refund", o3) != 1 || e.shop.Performed("cancel", o1) != 0 || e.shop.Performed("cancel", o2) != 0 || len(e.em.Host.Outbox) != 3 {
		t.Fatalf("refund %d cancel1 %d cancel2 %d pending %d: after an ack failure nothing else may start",
			e.shop.Performed("refund", o3), e.shop.Performed("cancel", o1), e.shop.Performed("cancel", o2), len(e.em.Host.Outbox))
	}
}

// --- order.cancel is an asynchronous Shopify job -----------------------------
// A cancel is acknowledged only once the order SHOWS cancelled. The cloud's
// confirmation marks every unit refunded and restocked, so an ack for a job
// that later fails on the gateway refund would leave the customer's money in
// Shopify while Universal Till says refunded.

func cancelNow(e *env, id, order string) channel.Ack {
	return e.outbox(cancelOp(id, order, `{"reason":"customer_request","restock":true}`))[id]
}

func TestCancelJobDoneImmediatelyNeedsNoPollButIsVerified(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	if a := cancelNow(e, "c", o3); a.Error != nil || a.ExternalID != o3 {
		t.Fatalf("ack %+v", a)
	}
	if e.shop.JobPolls() != 0 || e.count("FulfilmentOrders") != 2 {
		t.Fatalf("polls %d, order reads %d: no poll, one read before and one to verify", e.shop.JobPolls(), e.count("FulfilmentOrders"))
	}
}

func TestCancelJobDoneAfterOnePollIsSuccess(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.SlowCancel(1)
	if a := cancelNow(e, "c", o3); a.Error != nil || a.ExternalID != o3 {
		t.Fatalf("ack %+v", a)
	}
	if e.shop.JobPolls() != 1 || e.shop.Performed("cancel", o3) != 1 {
		t.Fatalf("polls %d performed %d", e.shop.JobPolls(), e.shop.Performed("cancel", o3))
	}
	// The poll counts against the ask's budget like every request.
	if got, want := app.SpentHTTP(), len(e.shop.Requests()); got != want {
		t.Fatalf("budget counted %d requests, the shop saw %d", got, want)
	}
}

func TestCancelJobStillRunningIsPolledOnceAndAckedRetryable(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.SlowCancel(3)
	wantAck(t, cancelNow(e, "c", o3), "cancel_pending", true)
	if e.shop.JobPolls() != 1 {
		t.Fatalf("polled %d times, want exactly once", e.shop.JobPolls())
	}
	// The job finishes; the retry's read finds the order cancelled and sends nothing.
	e.shop.FinishCancelJobs()
	if a := cancelNow(e, "c", o3); a.Error != nil || e.count("OrderCancel") != 1 || e.shop.Performed("cancel", o3) != 1 {
		t.Fatalf("retry %+v mutations %d performed %d", a, e.count("OrderCancel"), e.shop.Performed("cancel", o3))
	}
}

func TestCancelAnswerLostWhileTheJobRunsIsPendingNotDeadLettered(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.SlowCancel(5)
	e.shop.LoseAnswer("OrderCancel", 1)
	wantAck(t, cancelNow(e, "c", o3), "channel_error", true)
	// Retry while the job still runs: Shopify says a cancel is in progress.
	// That is not a rejection of an order that IS being cancelled.
	wantAck(t, cancelNow(e, "c", o3), "cancel_pending", true)
	e.shop.FinishCancelJobs()
	if a := cancelNow(e, "c", o3); a.Error != nil || e.shop.Performed("cancel", o3) != 1 {
		t.Fatalf("after the job: %+v performed %d", a, e.shop.Performed("cancel", o3))
	}
}

func TestCancelJobThatFailedIsNonRetryableWithAStableCode(t *testing.T) {
	// Done at once, no effect.
	e := newEnv(t, fakeshop.Options{})
	e.shop.FailCancelJob()
	wantAck(t, cancelNow(e, "c", o3), "cancel_failed", false)
	// Done at the poll, no effect.
	e2 := newEnv(t, fakeshop.Options{})
	e2.shop.SlowCancel(1)
	e2.shop.FailCancelJob()
	wantAck(t, cancelNow(e2, "c", o3), "cancel_failed", false)
	if e2.shop.Performed("cancel", o3) != 0 {
		t.Fatal("a failed job cancelled the order")
	}
}

func TestCancelJobPollThatErrorsStaysRetryable(t *testing.T) {
	// A wrong from-memory `job` name must not dead-letter an order that is being cancelled.
	e := newEnv(t, fakeshop.Options{})
	e.shop.SlowCancel(2)
	e.shop.SchemaError("CancelJob")
	wantAck(t, cancelNow(e, "c", o3), "cancel_pending", true)
	e.shop.FinishCancelJobs()
	if a := cancelNow(e, "c", o3); a.Error != nil {
		t.Fatalf("retry %+v", a)
	}
}

func TestCancelAndRefundRaceWithAnotherActorBetweenReadAndWriteAreSuccess(t *testing.T) {
	// Cancelled in the admin between our read and our mutation.
	e := newEnv(t, fakeshop.Options{})
	e.shop.BeforeMutation("OrderCancel", func() { e.shop.CancelOrder(o1) })
	if a := cancelNow(e, "c", o1); a.Error != nil || a.ExternalID != o1 {
		t.Fatalf("cancel race: %+v", a)
	}
	if e.shop.Performed("cancel", o1) != 0 {
		t.Fatal("we did not cancel it; the shop did")
	}
	// Refunded in the admin between our read and our mutation.
	e2 := newEnv(t, fakeshop.Options{})
	e2.shop.BeforeMutation("RefundCreate", func() { e2.shop.RefundLine(o3, 0, 2, "NO_RESTOCK") })
	if a := e2.outbox(refundOp("r", o3, refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 0})))["r"]; a.Error != nil || a.ExternalID != o3 {
		t.Fatalf("refund race: %+v", a)
	}
	if e2.shop.Performed("refund", o3) != 0 || e2.count("RefundState") != 2 {
		t.Fatalf("performed %d, state reads %d (the re-read after the user error decides)", e2.shop.Performed("refund", o3), e2.count("RefundState"))
	}
	// A user error whose re-read shows only part of it done is still a rejection.
	e3 := newEnv(t, fakeshop.Options{})
	e3.shop.BeforeMutation("RefundCreate", func() { e3.shop.RefundLine(o3, 0, 1, "NO_RESTOCK") })
	wantAck(t, e3.outbox(refundOp("r", o3, refundJSON(o3, "GBP", 2430, [4]any{l3, 3, 3, 0})))["r"], "rejected_by_channel", false)
}

func TestRefundRefusesATransactionInAnotherCurrency(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.SuggestTransactionCurrency("EUR")
	wantAck(t, e.outbox(refundOp("r", o3, refundJSON(o3, "GBP", 1620, [4]any{l3, 2, 2, 2})))["r"], "rejected_by_channel", false)
	if e.count("RefundCreate") != 0 || e.shop.RefundTotal(o3) != 0 {
		t.Fatal("a refund with a foreign-currency transaction was sent")
	}
}

// The cloud's ceiling (floor discount share, ceiling tax) and Shopify's own
// proportional rounding can differ by a minor unit on an odd-priced line with
// a discount code. The adapter fails closed: no money moves, a stable code.
// README "Verify first": 'odd-priced 3-unit line with a discount code'.
func TestRefundOneMinorUnitAboveTheCeilingFromRoundingFailsClosed(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	line := e.shop.AddOrderLine(o1, 3, 333)
	e.shop.SuggestExtra(1) // Shopify's figure is one minor unit above the cloud's
	wantAck(t, e.outbox(refundOp("r", o1, refundJSON(o1, "GBP", 999, [4]any{line, 3, 3, 0})))["r"], "refund_exceeds_ceiling", false)
	if e.count("RefundCreate") != 0 || e.shop.RefundTotal(o1) != 0 {
		t.Fatal("money moved on a one-unit overshoot")
	}
}

func TestCancelAcceptedWithNoJobOrAnUnusableJobIdIsPendingAndPollsNothingHostile(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.OddCancelJob(nil) // accepted, no job, the order is not cancelled
	wantAck(t, cancelNow(e, "c", o3), "cancel_pending", true)
	e2 := newEnv(t, fakeshop.Options{})
	e2.shop.OddCancelJob(map[string]any{"id": "../../admin", "done": false})
	wantAck(t, cancelNow(e2, "c", o3), "cancel_pending", true)
	if e2.shop.JobPolls() != 0 {
		t.Fatal("an id that is not a Shopify Job id was sent to the shop")
	}
}
