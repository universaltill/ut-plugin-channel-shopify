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

func i64(n int64) *int64 { return &n }

func (e *env) outbox(ops ...channel.OutboxOp) map[string]channel.Ack {
	e.t.Helper()
	e.em.Host.Outbox = append(e.em.Host.Outbox, ops...)
	e.ask(channel.AskOutbox, struct{}{})
	return e.em.Host.Acks
}

func stockOp(id, ext string, expect *int64, payload string) channel.OutboxOp {
	return channel.OutboxOp{ID: id, Kind: channel.OpStockSet, ExternalID: ext, Expect: expect, Payload: json.RawMessage(payload)}
}

func fulfilOp(id, order, payload string) channel.OutboxOp {
	return channel.OutboxOp{ID: id, Kind: channel.OpFulfilmentCreate, ExternalID: order, Payload: json.RawMessage(payload)}
}

func TestStockSetIsAbsoluteAndAStaleExpectReReads(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v, loc := e.shop.VariantIDs(1)[0], e.shop.Locations()[0]
	if e.shop.Stock(v, loc) != 11 {
		t.Fatalf("seed stock %d", e.shop.Stock(v, loc))
	}
	acks := e.outbox(stockOp("op-1", v, i64(11), `{"available": 4}`))
	if a := acks["op-1"]; a.Error != nil || a.ExternalID != v || a.ExternalVersion != "4" {
		t.Fatalf("ack %+v", a)
	}
	if e.shop.Stock(v, loc) != 4 {
		t.Fatalf("stock %d, want the absolute value 4", e.shop.Stock(v, loc))
	}
	// Sending the same absolute value again is still 4, never 4 less.
	acks = e.outbox(stockOp("op-2", v, i64(4), `{"available": 4}`))
	if acks["op-2"].Error != nil || e.shop.Stock(v, loc) != 4 {
		t.Fatalf("%+v %d", acks["op-2"], e.shop.Stock(v, loc))
	}

	// The merchant sold two in Shopify meanwhile: expect 4 is stale.
	e.shop.SetStock(v, loc, 2)
	e.em.Host.Submitted = nil
	acks = e.outbox(stockOp("op-3", v, i64(4), `{"available": 3}`))
	a := acks["op-3"]
	if a.Error == nil || a.Error.Code != "stale_expect" || !a.Error.Retryable {
		t.Fatalf("stale ack %+v", a)
	}
	if e.shop.Stock(v, loc) != 2 {
		t.Fatalf("a stale compare-and-set changed the stock to %d", e.shop.Stock(v, loc))
	}
	obs := e.submitted(channel.KindStockObserved)
	if len(obs) != 1 {
		t.Fatalf("%d observations, want the channel's current value reported", len(obs))
	}
	var o channel.StockObserved
	_ = json.Unmarshal(obs[0], &o)
	if o.Available != 2 || o.Variant != v || o.Location != loc {
		t.Fatalf("observation %+v", o)
	}
	var pulled bool
	for _, r := range e.shop.Requests() {
		pulled = pulled || r.Op == "Orders"
	}
	if !pulled {
		t.Error("a stale compare-and-set must pull recent orders (ADR-0169 section 8) before the cloud recomputes")
	}

	// No expect: an unconditional set.
	acks = e.outbox(stockOp("op-4", v, nil, `{"available": 9}`))
	if acks["op-4"].Error != nil || e.shop.Stock(v, loc) != 9 {
		t.Fatalf("%+v %d", acks["op-4"], e.shop.Stock(v, loc))
	}
}

func TestStockSetRetryActsOnce(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v, loc := e.shop.VariantIDs(1)[0], e.shop.Locations()[0]
	e.outbox(stockOp("op-1", v, i64(11), `{"available": 6}`))
	// The host re-delivers the same operation (a lost ack): same op id, same key.
	acks := e.outbox(stockOp("op-1", v, i64(11), `{"available": 6}`))
	if acks["op-1"].Error != nil {
		t.Fatalf("retry %+v", acks["op-1"])
	}
	if n := e.shop.Performed("inventory", v); n != 1 || e.shop.Stock(v, loc) != 6 {
		t.Fatalf("performed %d times, stock %d", n, e.shop.Stock(v, loc))
	}
	// Every inventory write carries the op id as the @idempotent key and header.
	var keyed int
	for _, r := range e.shop.Requests() {
		if r.Op == "InventorySet" {
			var k string
			_ = json.Unmarshal(r.Variables["key"], &k)
			if k != "op-1" || r.Header.Get("Idempotency-Key") != "op-1" {
				t.Errorf("key %q header %q", k, r.Header.Get("Idempotency-Key"))
			}
			keyed++
		}
	}
	if keyed != 2 {
		t.Errorf("%d inventory writes", keyed)
	}
}

func TestStockSetLocations(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Locations: 2})
	v := e.shop.VariantIDs(1)[0]
	l1, l2 := e.shop.Locations()[0], e.shop.Locations()[1]
	// Two locations and no location named: refuse, never guess.
	a := e.outbox(stockOp("op-1", v, nil, `{"available": 3}`))["op-1"]
	if a.Error == nil || a.Error.Code != "location_required" || a.Error.Retryable {
		t.Fatalf("%+v", a)
	}
	// Named in the external id or the payload.
	if a := e.outbox(stockOp("op-2", v+"@"+l2, nil, `{"available": 3}`))["op-2"]; a.Error != nil || e.shop.Stock(v, l2) != 3 || e.shop.Stock(v, l1) != 11 {
		t.Fatalf("%+v %d %d", a, e.shop.Stock(v, l2), e.shop.Stock(v, l1))
	}
	if a := e.outbox(stockOp("op-3", v, nil, fmt.Sprintf(`{"available": 5, "location": %q}`, l1)))["op-3"]; a.Error != nil || e.shop.Stock(v, l1) != 5 {
		t.Fatalf("%+v", a)
	}
	// A level Shopify does not hold yet is activated first.
	e.shop.DeactivateLevel(v, l2)
	if a := e.outbox(stockOp("op-4", v+"@"+l2, nil, `{"available": 7}`))["op-4"]; a.Error != nil || !e.shop.HasLevel(v, l2) || e.shop.Stock(v, l2) != 7 {
		t.Fatalf("%+v", a)
	}
	// A location that is not the shop's is refused.
	if a := e.outbox(stockOp("op-5", v+"@gid://shopify/Location/424242", nil, `{"available": 1}`))["op-5"]; a.Error == nil || a.Error.Retryable {
		t.Fatalf("%+v", a)
	}
}

// An op that activates a level and then sets it sends two different mutations:
// each carries its own key, so a redelivery that no longer needs the activation
// (the level is held now) is judged on the set alone (ut-docs#4920).
func TestStockSetActivationAndSetCarryTheirOwnKeys(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Locations: 2})
	v := e.shop.VariantIDs(1)[0]
	l2 := e.shop.Locations()[1]
	e.shop.DeactivateLevel(v, l2)
	if a := e.outbox(stockOp("op-1", v+"@"+l2, nil, `{"available": 7}`))["op-1"]; a.Error != nil || e.shop.Stock(v, l2) != 7 {
		t.Fatalf("%+v", a)
	}
	keys := map[string]string{}
	for _, r := range e.shop.Requests() {
		if r.Op == "InventoryActivate" || r.Op == "InventorySet" {
			var k string
			_ = json.Unmarshal(r.Variables["key"], &k)
			keys[r.Op] = k
			if r.Header.Get("Idempotency-Key") != k {
				t.Errorf("%s: header %q, key %q", r.Op, r.Header.Get("Idempotency-Key"), k)
			}
		}
	}
	if keys["InventoryActivate"] == "" || keys["InventorySet"] == "" || keys["InventoryActivate"] == keys["InventorySet"] || !strings.HasPrefix(keys["InventorySet"], "op-1") {
		t.Fatalf("keys %v: want two different keys derived from the op id", keys)
	}
}

// The first delivery activated the level and set it with no compare; the ack
// was lost. The redelivery finds the level held, so it compares against the
// cloud's last value: a different input under the same op id. It must be judged
// on the stale compare (a retryable stale_expect), never refused for its key.
func TestStockSetRedeliveryAfterActivationIsJudgedOnItsCompareNotItsKey(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Locations: 2})
	v := e.shop.VariantIDs(1)[0]
	l2 := e.shop.Locations()[1]
	e.shop.DeactivateLevel(v, l2)
	if a := e.outbox(stockOp("op-1", v+"@"+l2, nil, `{"available": 7}`))["op-1"]; a.Error != nil {
		t.Fatalf("first delivery: %+v", a)
	}
	expect := int64(0) // what the cloud last saw before the first delivery
	a := e.outbox(stockOp("op-1", v+"@"+l2, &expect, `{"available": 7}`))["op-1"]
	if a.Error == nil || a.Error.Code != "stale_expect" || !a.Error.Retryable {
		t.Fatalf("redelivery: %+v, want a retryable stale_expect", a)
	}
}

func TestStockSetRefusals(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	before := e.shop.Fingerprint()
	cases := map[string]channel.OutboxOp{
		"negative":            stockOp("a", v, nil, `{"available": -1}`),
		"missing":             stockOp("b", v, nil, `{}`),
		"not json":            stockOp("c", v, nil, `"nope"`),
		"hostile id":          stockOp("d", "../../admin", nil, `{"available": 1}`),
		"order id as variant": stockOp("e", "gid://shopify/Order/5001", nil, `{"available": 1}`),
		"bad op id":           stockOp("bad id", v, nil, `{"available": 1}`),
		"unknown variant":     stockOp("f", "gid://shopify/ProductVariant/1", nil, `{"available": 1}`),
		"bad location":        stockOp("g", v, nil, `{"available": 1, "location": "x"}`),
	}
	for name, op := range cases {
		a := e.outbox(op)[op.ID]
		if a.Error == nil || a.Error.Retryable {
			t.Errorf("%s: %+v, want a non-retryable refusal", name, a)
		}
	}
	if e.shop.Fingerprint() != before {
		t.Error("a refused stock.set changed the shop")
	}
	e.shop.SetTracked(v, false)
	if a := e.outbox(stockOp("h", v, nil, `{"available": 1}`))["h"]; a.Error == nil || a.Error.Code != "not_tracked" || a.Error.Retryable {
		t.Errorf("untracked: %+v", a)
	}
}

func TestFulfilmentReachesShopifyOnceAndAlreadyFulfilledIsSuccess(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	id := "gid://shopify/Order/5001"
	payload := fmt.Sprintf(`{"external_order_id": %q, "carrier": "Invented Post", "tracking_number": "IP123456", "tracking_url": "https://track.example/IP123456"}`, id)
	a := e.outbox(fulfilOp("op-1", id, payload))["op-1"]
	if a.Error != nil || a.ExternalID != id {
		t.Fatalf("ack %+v", a)
	}
	c, n, u := e.shop.Tracking(id)
	if c != "Invented Post" || n != "IP123456" || u != "https://track.example/IP123456" || e.shop.Performed("fulfil", id) != 1 {
		t.Fatalf("tracking %q %q %q performed %d", c, n, u, e.shop.Performed("fulfil", id))
	}
	// The host redelivers after a lost ack: success, nothing done twice.
	if a := e.outbox(fulfilOp("op-1", id, payload))["op-1"]; a.Error != nil || e.shop.Performed("fulfil", id) != 1 {
		t.Fatalf("redelivery %+v performed %d", a, e.shop.Performed("fulfil", id))
	}
	// The merchant shipped it in Shopify's own admin: also success.
	id2 := "gid://shopify/Order/5002"
	e.shop.FulfilExternally(id2)
	if a := e.outbox(fulfilOp("op-2", id2, fmt.Sprintf(`{"external_order_id": %q, "carrier": "c"}`, id2)))["op-2"]; a.Error != nil || e.shop.Performed("fulfil", id2) != 0 {
		t.Fatalf("already fulfilled %+v performed %d", a, e.shop.Performed("fulfil", id2))
	}
	// The fulfilment mutation must not be retried blindly by the host: no
	// Idempotency-Key header (the pre-read makes the retry safe instead).
	for _, r := range e.shop.Requests() {
		if r.Op == "FulfillmentCreate" && r.Header.Get("Idempotency-Key") != "" {
			t.Error("FulfillmentCreate carries an Idempotency-Key: the host would re-send a POST that may have acted")
		}
	}
}

func TestFulfilmentRefusals(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.CancelOrder("gid://shopify/Order/5003")
	cases := []struct {
		name string
		op   channel.OutboxOp
		code string
	}{
		{"cancelled order", fulfilOp("a", "gid://shopify/Order/5003", `{"carrier":"c"}`), "order_cancelled"},
		{"unknown order", fulfilOp("b", "gid://shopify/Order/424242", `{"carrier":"c"}`), "not_found"},
		{"hostile id", fulfilOp("c", "../../admin", `{"carrier":"c"}`), "invalid_payload"},
		{"payload names another order", fulfilOp("d", "gid://shopify/Order/5001", `{"external_order_id":"gid://shopify/Order/5002"}`), "invalid_payload"},
		{"not json", fulfilOp("e", "gid://shopify/Order/5001", `"nope"`), "invalid_payload"},
	}
	for _, c := range cases {
		a := e.outbox(c.op)[c.op.ID]
		if a.Error == nil || a.Error.Code != c.code || a.Error.Retryable {
			t.Errorf("%s: %+v, want non-retryable %s", c.name, a, c.code)
		}
	}
	for _, id := range []string{"gid://shopify/Order/5001", "gid://shopify/Order/5002", "gid://shopify/Order/5003"} {
		if e.shop.Performed("fulfil", id) != 0 {
			t.Errorf("%s was fulfilled by a refused operation", id)
		}
	}
}

func TestFulfilmentDropsATrackingURLThatIsNotHTTPS(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	id := "gid://shopify/Order/5001"
	a := e.outbox(fulfilOp("op-1", id, fmt.Sprintf(`{"external_order_id": %q, "carrier": "c", "tracking_number": "T1", "tracking_url": "javascript:alert(1)"}`, id)))["op-1"]
	_, n, u := e.shop.Tracking(id)
	if a.Error != nil || n != "T1" || u != "" {
		t.Fatalf("%+v %q %q", a, n, u)
	}
}

// cloudUpsert is the listing.upsert the cloud really sends for an imported
// item, copied from ut-cloud internal/channelsync/outbound.go (upsertPayload,
// build): the top-level external_id AND variants[0].external_id are the
// channel LISTING's external id, which for a Shopify item is the VARIANT id
// (internal/channelsync/convert.go). If the cloud's shape changes, change this
// copy: the adapter tests must follow the cloud, not the other way round.
func cloudUpsert(id, variant string, priceMinor int64, currency string, extra ...string) channel.OutboxOp {
	body := fmt.Sprintf(`{"cloud_id":"00000000-0000-4000-8000-000000000001","external_id":%q,"name":"Invented item","description":"d",`+
		`"status":"active","category_path":["A","B"],"variants":[{"external_id":%q,"cloud_id":"00000000-0000-4000-8000-000000000001",`+
		`"sku":"INV-1","barcodes":["5000000000000"],"price_minor":%d,"currency":%q,"tax_included":true}]}`, variant, variant, priceMinor, currency)
	return channel.OutboxOp{ID: id, Kind: channel.OpListingUpsert, ExternalID: variant, Payload: json.RawMessage(body)}
}

func TestPriceEditInTheCloudsRealShapeReachesShopifyAndKeepsTheVariantLink(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	other := e.shop.VariantIDs(1)[1]
	a := e.outbox(cloudUpsert("op-1", v, 3500, "GBP"))["op-1"]
	// The ack's external id is written onto the listing by the cloud: it must
	// stay the VARIANT id, never the product id.
	if a.Error != nil || a.ExternalID != v || a.ExternalID == e.shop.ProductIDs()[0] {
		t.Fatalf("ack %+v", a)
	}
	if e.shop.Price(v) != "35.00" || e.shop.Price(other) != "6.11" {
		t.Fatalf("prices %q %q", e.shop.Price(v), e.shop.Price(other))
	}
	// The write names the variant's own product and only that variant.
	for _, r := range e.shop.Requests() {
		if r.Op == "VariantPrices" && string(r.Variables["productId"]) != `"`+e.shop.ProductIDs()[0]+`"` {
			t.Errorf("wrong product in the write: %s", r.Variables["productId"])
		}
	}
}

func TestPriceAlreadyEqualIsAckedWithoutAWrite(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	e.outbox(cloudUpsert("op-1", v, 3500, "GBP"))
	w := e.shop.Writes()
	a := e.outbox(cloudUpsert("op-2", v, 3500, "GBP"))["op-2"]
	if a.Error != nil || a.ExternalID != v || e.shop.Writes() != w {
		t.Fatalf("%+v writes %d -> %d", a, w, e.shop.Writes())
	}
}

func TestPriceEditRefusals(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	a := e.outbox(cloudUpsert("op-1", v, 3500, "GBP"))["op-1"]
	if a.Error != nil {
		t.Fatal(a)
	}
	w := e.shop.Writes()
	// Another currency is refused, not converted.
	if a := e.outbox(cloudUpsert("cur", v, 100, "EUR"))["cur"]; a.Error == nil || a.Error.Code != "currency_mismatch" || a.Error.Retryable {
		t.Fatalf("%+v", a)
	}
	// Zero and negative prices never reach the shop (a free live product).
	for _, n := range []int64{0, -1} {
		id := fmt.Sprintf("p%d", n)
		if a := e.outbox(cloudUpsert(id, v, n, "GBP"))[id]; a.Error == nil || a.Error.Code != "invalid_price" || a.Error.Retryable {
			t.Errorf("price %d: %+v", n, a)
		}
	}
	// A variant that does not exist, or whose id is not a variant gid.
	missing := "gid://shopify/ProductVariant/999999999"
	if a := e.outbox(cloudUpsert("miss", missing, 100, "GBP"))["miss"]; a.Error == nil || a.Error.Code != "not_found" || a.Error.Retryable {
		t.Fatalf("%+v", a)
	}
	asProduct := e.shop.ProductIDs()[0] // the old, wrong convention
	for i, op := range []channel.OutboxOp{
		cloudUpsert("prod", asProduct, 100, "GBP"),
		cloudUpsert("bad", "../../x", 100, "GBP"),
		{ID: "mismatch", Kind: channel.OpListingUpsert, ExternalID: v, Payload: json.RawMessage(fmt.Sprintf(
			`{"variants":[{"external_id":%q,"price_minor":100,"currency":"GBP"}]}`, e.shop.VariantIDs(2)[0]))},
		{ID: "two", Kind: channel.OpListingUpsert, ExternalID: v, Payload: json.RawMessage(fmt.Sprintf(
			`{"variants":[{"external_id":%q,"price_minor":100,"currency":"GBP"},{"external_id":%q,"price_minor":100,"currency":"GBP"}]}`, v, e.shop.VariantIDs(1)[1]))},
		{ID: "empty", Kind: channel.OpListingUpsert, ExternalID: v, Payload: json.RawMessage(`{"variants":[]}`)},
		{ID: "str", Kind: channel.OpListingUpsert, ExternalID: v, Payload: json.RawMessage(`"nope"`)},
	} {
		a := e.outbox(op)[op.ID]
		if a.Error == nil || a.Error.Code != "invalid_payload" || a.Error.Retryable {
			t.Errorf("case %d (%s): %+v", i, op.ID, a)
		}
	}
	if e.shop.Price(v) != "35.00" || e.shop.Price(e.shop.VariantIDs(2)[0]) != "7.10" || e.shop.Writes() != w {
		t.Error("a refused price edit changed a price")
	}
}

// A price above the variant's compare-at is pushed, not refused (README row
// says what to look for).
func TestPriceAboveCompareAtIsStillPushed(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	if a := e.outbox(cloudUpsert("op-1", v, 99999, "GBP"))["op-1"]; a.Error != nil || e.shop.Price(v) != "999.99" {
		t.Fatalf("%+v %q", a, e.shop.Price(v))
	}
}

// The cloud sends the whole listing record. Only the variant price is pushed;
// the rest is ignored and counted once per ask, on success only.
func TestUpsertWithOtherFieldsPushesThePriceAndCountsIgnoredFields(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v, v1b := e.shop.VariantIDs(1)[0], e.shop.VariantIDs(1)[1]
	count := func(env *env) int {
		n := 0
		for _, l := range env.em.Host.Logs {
			if l.Code == "listing.fields_ignored" {
				n++
			}
		}
		return n
	}
	// Two ops in the one ask: the code is logged once, not per op.
	acks := e.outbox(cloudUpsert("op-1", v, 4200, "GBP"), cloudUpsert("op-1b", v1b, 100, "GBP"))
	if acks["op-1"].Error != nil || acks["op-1b"].Error != nil || e.shop.Price(v) != "42.00" {
		t.Fatalf("%+v", acks)
	}
	if n := count(e); n != 1 {
		t.Fatalf("listing.fields_ignored logged %d times, want once", n)
	}
	// A later ask logs again (the once is per ask).
	e.outbox(cloudUpsert("op-1c", v1b, 200, "GBP"))
	if n := count(e); n != 2 {
		t.Fatalf("a second ask logged %d in all, want 2", n)
	}
	// An op that FAILS (currency, zero price, unknown variant) logs nothing.
	e3 := newEnv(t, fakeshop.Options{})
	v3 := e3.shop.VariantIDs(1)[0]
	e3.outbox(cloudUpsert("f1", v3, 100, "EUR"), cloudUpsert("f2", v3, 0, "GBP"), cloudUpsert("f3", "gid://shopify/ProductVariant/999999999", 100, "GBP"))
	if n := count(e3); n != 0 {
		t.Errorf("fields_ignored logged %d times for ops that failed", n)
	}
	// Price only: nothing ignored, nothing logged.
	e2 := newEnv(t, fakeshop.Options{})
	v2 := e2.shop.VariantIDs(1)[0]
	e2.outbox(channel.OutboxOp{ID: "op-2", Kind: channel.OpListingUpsert, ExternalID: v2,
		Payload: json.RawMessage(fmt.Sprintf(`{"variants":[{"external_id":%q,"price_minor":100,"currency":"GBP"}]}`, v2))})
	if n := count(e2); n != 0 {
		t.Error("logged for a price-only upsert")
	}
}

// A create (no external id) is refused for good and sends nothing: the adapter
// cannot create products (ut-docs#4774).
func TestCreateUpsertIsRefusedNonRetryablyAndSendsNothing(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	for i, body := range []string{
		`{"name":"New thing","variants":[{"external_id":"","price_minor":100,"currency":"GBP"}]}`,
		`{"name":"New thing"}`, `"nope"`, ``,
	} {
		id := fmt.Sprintf("c-%d", i)
		a := e.outbox(channel.OutboxOp{ID: id, Kind: channel.OpListingUpsert, ExternalID: "", Payload: json.RawMessage(body)})[id]
		if a.Error == nil || a.Error.Code != "create_not_supported" || a.Error.Retryable {
			t.Errorf("case %d: %+v", i, a)
		}
	}
	if len(e.shop.Requests()) != 0 {
		t.Errorf("a refused create made %d requests", len(e.shop.Requests()))
	}
}

// listing.unpublish is not done (a draft is an item push, ut-docs#4774) even
// though items is "both", so it is never acted on.
func TestUnpublishIsNotActedOnAndIsDeadLetteredWithALoggedCode(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p := e.shop.ProductIDs()[0]
	status := e.shop.Status(p)
	a := e.outbox(channel.OutboxOp{ID: "op-1", Kind: channel.OpListingUnpublish, ExternalID: p})["op-1"]
	if a.Error == nil || a.Error.Retryable || a.Error.Code != "unsupported_op" {
		t.Fatalf("%+v", a)
	}
	if len(e.shop.Requests()) != 0 || e.shop.Status(p) != status {
		t.Fatalf("unpublish touched the shop: %d requests, status %q", len(e.shop.Requests()), e.shop.Status(p))
	}
	logged := false
	for _, l := range e.em.Host.Logs {
		logged = logged || l.Code == "outbox.unsupported_op"
	}
	if !logged {
		t.Error("the dead-lettered op left no log code")
	}
}

func TestUnsupportedOperationsAreDeadLetteredWithoutARequest(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	var ops []channel.OutboxOp
	for i, k := range []string{channel.OpImageUpsert, "listing.delete", "product.delete", ""} {
		ops = append(ops, channel.OutboxOp{ID: fmt.Sprintf("u-%d", i), Kind: k, ExternalID: "gid://shopify/Order/5001", Payload: json.RawMessage(`{"reason":"other","restock":false}`)})
	}
	acks := e.outbox(ops...)
	for _, op := range ops {
		if a := acks[op.ID]; a.Error == nil || a.Error.Code != "unsupported_op" || a.Error.Retryable {
			t.Errorf("%q: %+v", op.Kind, a)
		}
	}
	if len(e.shop.Requests()) != 0 {
		t.Error("an unsupported operation made a request")
	}
}

func TestOutboxFailuresAreClassified(t *testing.T) {
	// Every call is throttled: the host gives up after six attempts (-5) and
	// the operation is retried later, not dead-lettered.
	e := newEnv(t, fakeshop.Options{GraphQLThrottleEvery: 1})
	v := e.shop.VariantIDs(1)[0]
	if a := e.outbox(stockOp("op-1", v, nil, `{"available": 1}`))["op-1"]; a.Error == nil || !a.Error.Retryable || a.Error.Code != "rate_limited" {
		t.Fatalf("%+v", a)
	}
	// A revoked token is not retried forever.
	e2 := newEnv(t, fakeshop.Options{})
	e2.em.Credential = "wrong"
	if a := e2.outbox(stockOp("op-1", e2.shop.VariantIDs(1)[0], nil, `{"available": 1}`))["op-1"]; a.Error == nil || a.Error.Retryable {
		t.Fatalf("%+v", a)
	}
}

func (e *env) hostCalls() (n int) {
	for _, c := range e.em.Host.Calls {
		n += c
	}
	return n
}

func staleOps(e *env, n int) {
	v := e.shop.VariantIDs(1)[0]
	for i := 0; i < n; i++ { // the shop holds 11: expect 4 is stale every time
		e.em.Host.Outbox = append(e.em.Host.Outbox, stockOp(fmt.Sprintf("stale-%03d", i), v, i64(4), `{"available": 3}`))
	}
}

func ordersAsks(e *env) (n int) {
	for _, r := range e.shop.Requests() {
		if r.Op == "Orders" {
			n++
		}
	}
	return n
}

// askCost is the requested GraphQL cost of every request since request #from.
func askCost(e *env, from int) (c int) {
	for _, s := range e.em.Seen()[from:] {
		c += s.CostHint
	}
	return c
}

// A worst-case outbox ask (the drain plus the order pull a stale op asks for)
// must finish inside the host's 30 s deadline at Shopify's throttle: bucket of
// 1,000 points plus 50 a second, so about 1,000 + 50*25 points at most; the
// budgets keep it far below.
const worstAskCost = 2000

func TestManyStaleOpsPullOrdersOnceAndStayInsideTheBudgets(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	staleOps(e, 60)
	for ask := 0; ask < 40 && len(e.em.Host.Acks) < 60; ask++ {
		from, calls, before, ordersBefore := len(e.em.Seen()), 0, e.hostCalls(), ordersAsks(e)
		e.ask(channel.AskOutbox, struct{}{})
		calls = e.em.HTTPCalls()
		if n := ordersAsks(e) - ordersBefore; n > 1 {
			t.Errorf("orders were pulled %d times in one ask, want at most once", n)
		}
		if calls > app.DrainHTTPBudget+15 {
			t.Errorf("%d HTTP calls in one ask: a few tens, not hundreds", calls)
		}
		if c := e.hostCalls() - before; c > app.DrainHostBudget+60 {
			t.Errorf("%d host calls in one ask", c)
		}
		if c := askCost(e, from); c > worstAskCost {
			t.Errorf("one ask requested %d points: it cannot finish inside the 30 s deadline", c)
		}
	}
	if n := len(e.em.Host.Acks); n != 60 {
		t.Fatalf("%d of 60 acked after resuming", n)
	}
	for id, a := range e.em.Host.Acks {
		if a.Error == nil || a.Error.Code != "stale_expect" || !a.Error.Retryable {
			t.Fatalf("%s: %+v", id, a)
		}
	}
	if len(e.em.Host.Outbox) != 0 {
		t.Fatalf("%d operations left un-acked", len(e.em.Host.Outbox))
	}
}

func TestTheDrainStopsShortOfItsWorkAndEveryStartedOpIsAcked(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	staleOps(e, 100)
	e.ask(channel.AskOutbox, struct{}{})
	first := len(e.em.Host.Acks)
	if first == 0 || first >= 100 {
		t.Fatalf("first ask acked %d of 100: it must do some and stop short", first)
	}
	if len(e.em.Host.Outbox)+first != 100 {
		t.Fatalf("%d acked + %d pending != 100: an op was lost", first, len(e.em.Host.Outbox))
	}
	// Everything the shop was asked to do was acknowledged: an op that was
	// started is finished and acked, one that was not stays pending.
	for _, r := range e.shop.Requests() {
		if r.Op == "InventorySet" && r.Variables["key"] != nil {
			var key string
			_ = json.Unmarshal(r.Variables["key"], &key)
			if _, ok := e.em.Host.Acks[key]; !ok {
				t.Errorf("%s was performed and not acknowledged", key)
			}
		}
	}
}

func TestTheDrainAlsoStopsOnTheHostCallBudgetWhenNoHTTPIsMade(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	for i := 0; i < 400; i++ { // refused before any request: only host calls (ack + log each)
		e.em.Host.Outbox = append(e.em.Host.Outbox, channel.OutboxOp{ID: fmt.Sprintf("bad-%04d", i), Kind: "image.upsert"})
	}
	before := e.hostCalls()
	e.ask(channel.AskOutbox, struct{}{})
	n := len(e.em.Host.Acks)
	if n == 0 || n >= 400 {
		t.Fatalf("acked %d of 400 in one ask: the host-call budget should stop it", n)
	}
	// Logs and acks are both counted, so the ask stays near the budget.
	if c := e.hostCalls() - before; c > app.DrainHostBudget+10 {
		t.Errorf("%d host calls in one ask: the budget is %d", c, app.DrainHostBudget)
	}
	for i := 0; i < 20 && len(e.em.Host.Acks) < 400; i++ {
		e.ask(channel.AskOutbox, struct{}{})
	}
	if n := len(e.em.Host.Acks); n != 400 {
		t.Fatalf("%d of 400 acked after resuming", n)
	}
}

func TestAnAckFailureStopsTheDrainInsteadOfRepeatingTheOp(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	staleOps(e, 5)
	e.em.Host.Deny["outbox_ack"] = true
	e.ask(channel.AskOutbox, struct{}{})
	performed := 0
	for _, r := range e.shop.Requests() {
		if r.Op == "InventorySet" {
			performed++
		}
	}
	if performed > 1 {
		t.Fatalf("%d attempts at a write whose ack failed: the drain must stop, not hand the same op out again", performed)
	}
	if len(e.em.Host.Outbox) != 5 {
		t.Fatalf("%d ops pending, want all 5 (nothing was acknowledged)", len(e.em.Host.Outbox))
	}
}

func TestTheDrainAlsoStopsOnTheRequestBudgetWhenRequestsAreCheap(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	for i := 0; i < 100; i++ { // one 5-point request each (the shop's currency), refused: wrong currency
		e.em.Host.Outbox = append(e.em.Host.Outbox, cloudUpsert(fmt.Sprintf("cur-%03d", i), v, 100, "EUR"))
	}
	e.ask(channel.AskOutbox, struct{}{})
	if n := len(e.em.Host.Acks); n == 0 || n >= 100 || e.em.HTTPCalls() > app.DrainHTTPBudget {
		t.Fatalf("acked %d of 100 with %d requests: the request budget (%d) should stop the drain", n, e.em.HTTPCalls(), app.DrainHTTPBudget)
	}
}

// Shopify refuses a fulfillment whose fulfilment orders sit at different
// locations: a multi-location order needs one fulfillmentCreate per location.
func TestFulfilmentOfAMultiLocationOrderIsOneCreatePerLocation(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	id := "gid://shopify/Order/5001"
	e.shop.AddFulfilmentOrder(id, 2)
	e.shop.AddFulfilmentOrder(id, 3)
	payload := fmt.Sprintf(`{"external_order_id": %q, "carrier": "Invented Post", "tracking_number": "IP1"}`, id)
	a := e.outbox(fulfilOp("op-1", id, payload))["op-1"]
	if a.Error != nil || a.ExternalID != id {
		t.Fatalf("ack %+v", a)
	}
	creates := 0
	for _, r := range e.shop.Requests() {
		if r.Op == "FulfillmentCreate" {
			creates++
		}
	}
	if creates != 3 || e.shop.Performed("fulfil", id) != 3 {
		t.Fatalf("creates %d performed %d, want one per location (3)", creates, e.shop.Performed("fulfil", id))
	}
	// Redelivery after success sends nothing more.
	if a := e.outbox(fulfilOp("op-1", id, payload))["op-1"]; a.Error != nil || e.shop.Performed("fulfil", id) != 3 {
		t.Fatalf("redelivery %+v performed %d", a, e.shop.Performed("fulfil", id))
	}
}

func TestAFailedLocationGroupIsAckedAndNotResentWhenTheOpIsRetried(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	id := "gid://shopify/Order/5001"
	bad := e.shop.AddFulfilmentOrder(id, 2)
	e.shop.RefuseFulfilmentOrder(bad)
	payload := fmt.Sprintf(`{"external_order_id": %q, "carrier": "c"}`, id)
	a := e.outbox(fulfilOp("op-1", id, payload))["op-1"]
	if a.Error == nil || a.Error.Code != "rejected_by_channel" || a.Error.Retryable {
		t.Fatalf("ack %+v", a)
	}
	if e.shop.Performed("fulfil", id) != 1 {
		t.Fatalf("the good location should be fulfilled once, got %d", e.shop.Performed("fulfil", id))
	}
	// A retry (an operator re-queues it) re-reads: the fulfilled location is
	// not sent again.
	e.outbox(fulfilOp("op-2", id, payload))
	if e.shop.Performed("fulfil", id) != 1 {
		t.Fatalf("a fulfilled location was re-sent: %d", e.shop.Performed("fulfil", id))
	}
}

// If the from-memory stock names are wrong, Shopify answers every stock.set
// with a GraphQL schema error. That must dead-letter (not retry forever) and
// leave a stable log code for the operator.
func TestAGraphQLSchemaErrorOnStockSetDeadLettersWithAStableLogCode(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.SchemaError("InventorySet")
	v := e.shop.VariantIDs(1)[0]
	a := e.outbox(stockOp("op-1", v, i64(11), `{"available": 3}`))["op-1"]
	if a.Error == nil || a.Error.Code != "rejected_by_channel" || a.Error.Retryable {
		t.Fatalf("ack %+v", a)
	}
	n := 0
	for _, r := range e.shop.Requests() {
		if r.Op == "InventorySet" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the schema error was sent %d times, want once (no retry loop)", n)
	}
	code := ""
	for _, l := range e.em.Host.Logs {
		if l.Code == "outbox.graphql_error" {
			code = fmt.Sprint(l.Fields["code"])
		}
	}
	if code != "undefinedField" {
		t.Errorf("graphql error log code %q, want undefinedField", code)
	}
}
