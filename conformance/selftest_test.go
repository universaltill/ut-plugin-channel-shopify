//go:build !wasip1

package conformance_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/conformance"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// A suite that cannot fail proves nothing. Each case below is a deliberately
// broken adapter built on the real handlers; the check named must report it.

func broken(over channel.Handlers) conformance.Target {
	tg := shopifyTarget()
	h := app.Handlers()
	for k, v := range over {
		h[k] = v
	}
	tg.Handlers = h
	return tg
}

func wantViolation(t *testing.T, got []string, substr string) {
	t.Helper()
	for _, g := range got {
		if strings.Contains(g, substr) {
			return
		}
	}
	t.Fatalf("no violation containing %q in %q", substr, got)
}

const endpoint = "https://{account}/admin/api/2026-10/graphql.json"

func post(op, query string, vars map[string]any, cost int) {
	b, _ := json.Marshal(map[string]any{"operationName": op, "query": query, "variables": vars})
	_, _ = channel.HTTP(channel.HTTPRequest{Method: "POST", URL: endpoint, Body: b, CostHint: cost})
}

const priceMutation = `mutation VariantPrices($productId: ID!, $variants: [ProductVariantsBulkInput!]!) { productVariantsBulkUpdate(productId: $productId, variants: $variants) { userErrors { code } } }`

func TestSuiteCatchesAConnectTestThatWrites(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskConnectTest: func(channel.Event) (any, error) {
		post("VariantPrices", priceMutation, map[string]any{"productId": "gid://shopify/Product/1001", "variants": []map[string]any{{"id": "gid://shopify/ProductVariant/2010", "price": "1.00"}}}, 10)
		_ = channel.CursorSet("seen", []byte("1"))
		return channel.ConnectTestResult{AccountName: "x"}, nil
	}})
	got := conformance.Check1ConnectReadsAndWritesNothing(t, tg)
	wantViolation(t, got, "sent POST")
	wantViolation(t, got, "write request")
	wantViolation(t, got, "changed during the connect test")
	wantViolation(t, got, "wrote a cursor")
}

func TestSuiteDoesNotMistakeAGraphQLReadForAWrite(t *testing.T) {
	if got := conformance.Check1ConnectReadsAndWritesNothing(t, shopifyTarget()); len(got) != 0 {
		t.Fatalf("the real connect test is a read: %v", got)
	}
}

func TestSuiteCatchesAConnectTestThatSubmits(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskConnectTest: func(channel.Event) (any, error) {
		post("ConnectTest", `query ConnectTest { shop { name } }`, nil, 5)
		_, _ = channel.Submit(channel.KindCategory, []channel.Category{{Identity: channel.Identity{ExternalID: "c"}, Name: "n"}})
		return channel.ConnectTestResult{AccountName: "x"}, nil
	}})
	wantViolation(t, conformance.Check1ConnectReadsAndWritesNothing(t, tg), "submitted")
}

func TestSuiteCatchesAnImportThatIsNotDeterministic(t *testing.T) {
	n := 0
	real := app.Handlers()[channel.AskImport]
	tg := broken(channel.Handlers{channel.AskImport: func(ev channel.Event) (any, error) {
		n++
		out, err := real(ev)
		// A "version" that depends on the run: the host would see a change on every import.
		_, _ = channel.Submit(channel.KindListing, []channel.Listing{{Identity: channel.Identity{ExternalID: "gid://shopify/Product/1", ExternalVersion: fmt.Sprint("run-", n)}, Title: "t"}})
		return out, err
	}})
	got := conformance.Check2ImportIsDeterministic(t, tg)
	wantViolation(t, got, "second import produced a record the first did not")
}

func TestSuiteCatchesAnImportThatLosesProducts(t *testing.T) {
	tg := shopifyTarget()
	tg.ExpectedProducts = 51
	wantViolation(t, conformance.Check2ImportIsDeterministic(t, tg), "imported 50 products, the shop has 51")
}

func TestSuiteCatchesAnAdapterThatReachesAnotherHost(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskTick: func(channel.Event) (any, error) {
		_, _ = channel.HTTP(channel.HTTPRequest{URL: "https://evil.example/collect"})
		_, _ = channel.HTTP(channel.HTTPRequest{URL: "http://{account}/plain"})
		return nil, nil
	}})
	wantViolation(t, conformance.Check9HostileChannelAndLimits(t, tg), "left {account}")
}

func TestSuiteCatchesADeleteMethodADeleteMutationAndACredentialHeader(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskOutbox: func(channel.Event) (any, error) {
		_, _ = channel.HTTP(channel.HTTPRequest{Method: "DELETE", URL: "https://{account}/admin/api/2026-10/products/1.json"})
		post("ProductDelete", `mutation ProductDelete($id: ID!) { productDelete(input: {id: $id}) { userErrors { field } } }`, map[string]any{"id": "gid://shopify/Product/1001"}, 10)
		_, _ = channel.HTTP(channel.HTTPRequest{Method: "POST", URL: endpoint, Headers: map[string]string{fakeHeader: "forged"}, Body: []byte(`{}`), CostHint: 5})
		return nil, nil
	}})
	got := conformance.Check7NoOperationDeletes(t, tg)
	wantViolation(t, got, "DELETE")
	wantViolation(t, got, "operation that deletes")
	wantViolation(t, conformance.Check9HostileChannelAndLimits(t, tg), "credential header itself")
}

const fakeHeader = "X-Shopify-Access-Token"

func TestSuiteCatchesAnIdThatReachesARequest(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskContact: func(ev channel.Event) (any, error) {
		var in channel.ContactInput
		_ = ev.Decode(&in)
		// Not validated: the hostile id goes straight into the variables.
		post("Contact", `query Contact($id: ID!) { order(id: $id) { email } }`, map[string]any{"id": in.OrderExternalID}, 5)
		return nil, nil
	}})
	wantViolation(t, conformance.Check9HostileChannelAndLimits(t, tg), "hostile id reached a request body")
}

func TestSuiteCatchesAnIdThatBecomesAPathTrick(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskContact: func(ev channel.Event) (any, error) {
		var in channel.ContactInput
		_ = ev.Decode(&in)
		_, _ = channel.HTTP(channel.HTTPRequest{URL: "https://{account}/admin/api/2026-10/orders/" + in.OrderExternalID + ".json"})
		return nil, nil
	}})
	wantViolation(t, conformance.Check9HostileChannelAndLimits(t, tg), "path segment")
}

func TestSuiteCatchesARequestWithoutACostHint(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskTick: func(channel.Event) (any, error) {
		post("Locations", `query Locations($first: Int!) { locations(first: $first) { nodes { id } } }`, map[string]any{"first": 1}, 0)
		return nil, nil
	}})
	wantViolation(t, conformance.Check9HostileChannelAndLimits(t, tg), "cost hint")
}

func TestSuiteCatchesARunawayImport(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskImport: func(channel.Event) (any, error) {
		for { // ignores every error and never stops
			post("Locations", `query Locations($first: Int!) { locations(first: $first) { nodes { id } } }`, map[string]any{"first": 1}, 5)
		}
	}})
	wantViolation(t, conformance.Check9HostileChannelAndLimits(t, tg), "killed by the host")
}

func TestSuiteCatchesAnAdapterThatLeaksWhatItIsGiven(t *testing.T) {
	tg := broken(channel.Handlers{channel.AskTick: func(channel.Event) (any, error) {
		channel.Log(channel.LevelInfo, "debug.token", map[string]any{"token": shopifyTarget().Credential})
		return nil, nil
	}})
	wantViolation(t, conformance.Check9HostileChannelAndLimits(t, tg), "credential reached adapter output")
}
