//go:build !wasip1

package conformance_test

import (
	"encoding/json"
	"strings"
	"testing"

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
	}
}
