//go:build !wasip1

package conformance_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/conformance"
	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// TestShopifyAdapterPassesTheSuite wires the suite into CI: the adapter's
// handlers, its own fake Shopify, the manifest's credential header.
func TestShopifyAdapterPassesTheSuite(t *testing.T) {
	conformance.Run(t, shopifyTarget())
}

// isMutation tells a GraphQL write from a read: Shopify takes both as POST.
func isMutation(s conformance.Seen) bool {
	var b struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(s.Body, &b) != nil {
		return s.Method != "GET" && s.Method != "HEAD"
	}
	return strings.HasPrefix(strings.TrimSpace(b.Query), "mutation")
}

func shopifyTarget() conformance.Target {
	probe := fakeshop.New(fakeshop.Options{})
	return conformance.Target{
		Handlers: app.Handlers(),
		NewShop:  func() conformance.Shop { return fakeshop.New(fakeshop.Options{}) },
		Account:  probe.Account(), CredentialHeader: fakeshop.Header, Credential: probe.Token(),
		IsWrite: isMutation, MaxCostHint: 1000, ExpectedProducts: 50,
		StockVariant: probe.VariantIDs(1)[0],
		WriteBack:    writeBack(probe),
	}
}

// writeBack wires checks 5 and 6 to the fake: a stock level and the three
// order write-backs, each with its own order of the seed.
func writeBack(probe *fakeshop.Shop) *conformance.WriteBack {
	variant, loc := probe.VariantIDs(1)[0], probe.Locations()[0]
	fake := func(s conformance.Shop) *fakeshop.Shop { return s.(*fakeshop.Shop) }
	const o1, o3, l3 = "gid://shopify/Order/5001", "gid://shopify/Order/5003", "gid://shopify/LineItem/6003"
	return &conformance.WriteBack{
		Stock: &conformance.StockRig{ExternalID: variant, Start: probe.Stock(variant, loc),
			Get: func(s conformance.Shop) int64 { return fake(s).Stock(variant, loc) },
			Set: func(s conformance.Shop, n int64) { fake(s).SetStock(variant, loc, n) }},
		Orders: []conformance.OrderRig{
			{Name: "fulfilment.create", Performed: func(s conformance.Shop) int { return fake(s).Performed("fulfil", o1) },
				LoseAnswer: func(s conformance.Shop) { fake(s).LoseAnswer("FulfillmentCreate", 1) },
				Op: channel.OutboxOp{ID: "w-fulfil", Kind: channel.OpFulfilmentCreate, ExternalID: o1,
					Payload: json.RawMessage(`{"external_order_id":"` + o1 + `","carrier":"Invented Post","tracking_number":"IP1"}`)}},
			{Name: "order.cancel", Performed: func(s conformance.Shop) int { return fake(s).Performed("cancel", o3) },
				LoseAnswer: func(s conformance.Shop) { fake(s).LoseAnswer("OrderCancel", 1) },
				Op: channel.OutboxOp{ID: "w-cancel", Kind: channel.OpOrderCancel, ExternalID: o3,
					Payload: json.RawMessage(`{"external_order_id":"` + o3 + `","reason":"out_of_stock","restock":true}`)}},
			{Name: "refund.create", Performed: func(s conformance.Shop) int { return fake(s).Performed("refund", o3) },
				LoseAnswer: func(s conformance.Shop) { fake(s).LoseAnswer("RefundCreate", 1) },
				Op: channel.OutboxOp{ID: "w-refund", Kind: channel.OpRefundCreate, ExternalID: o3,
					Payload: json.RawMessage(`{"external_order_id":"` + o3 + `","reason":"customer_request","restock":true,"currency":"GBP","max_amount_minor":1620,` +
						`"lines":[{"line_external_id":"` + l3 + `","quantity":2,"refunded_to":2,"restocked_to":2}]}`)}},
		},
	}
}

// createRefusal is the Shopify-specific rule for a manifest that declares items
// "both" while the adapter cannot create products (ut-docs#4774): a create
// (listing.upsert without an external id) is refused for good with
// create_not_supported and puts NOTHING on the wire. It lives here, not in
// suite.go, which stays a copy of the demo repo's suite.
func createRefusal(t testing.TB, tg conformance.Target) []string {
	t.Helper()
	var v []string
	shop := tg.NewShop()
	em := conformance.NewEmulator(t, tg.Handlers, shop, tg.Account, tg.CredentialHeader, tg.Credential)
	em.Host.Outbox = append(em.Host.Outbox, channel.OutboxOp{ID: "create-1", Kind: channel.OpListingUpsert, ExternalID: "",
		Payload: json.RawMessage(`{"name":"Owner published","variants":[{"external_id":"","price_minor":500,"currency":"GBP"}]}`)})
	if r := em.Ask(channel.AskOutbox, struct{}{}); r.Err != nil {
		return append(v, "outbox ask: "+r.Err.Error())
	}
	a, ok := em.Host.Acks["create-1"]
	switch {
	case !ok:
		v = append(v, "a create was not answered")
	case a.Error == nil:
		v = append(v, "a create was acknowledged as success")
	case a.Error.Code != "create_not_supported" || a.Error.Retryable:
		v = append(v, "a create must be refused non-retryably with create_not_supported, got "+a.Error.Code)
	}
	for _, s := range em.Seen() {
		if isMutation(s) {
			v = append(v, "a refused create sent a write: "+s.URL)
		}
	}
	return v
}

func TestTheRealAdapterRefusesACreateAndWritesNothing(t *testing.T) {
	if got := createRefusal(t, shopifyTarget()); len(got) != 0 {
		t.Fatal(got)
	}
}

// The self-test: an adapter that creates blindly (a write, then success) fails it.
func TestCreateRefusalCheckCatchesAnAdapterThatCreatesBlindly(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskOutbox: func(channel.Event) (any, error) {
		ops, _ := channel.OutboxNext(10)
		for _, op := range ops {
			post("VariantPrices", priceMutation, map[string]any{"productId": "gid://shopify/Product/1001", "variants": []map[string]any{{"id": "gid://shopify/ProductVariant/2010", "price": "5.00"}}}, 10)
			_ = channel.OutboxAck(op.ID, channel.Ack{ExternalID: "gid://shopify/Product/9"})
		}
		return nil, nil
	}})
	got := createRefusal(t, tg)
	wantViolation(t, got, "acknowledged as success")
	wantViolation(t, got, "sent a write")
}
