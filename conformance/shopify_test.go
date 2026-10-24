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
		WriteBack: writeBack(probe),
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
