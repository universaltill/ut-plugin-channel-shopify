//go:build !wasip1

package app_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// ut-docs#7084: release 0.8.0 declares the product topics and pulls images,
// and changes NOTHING about permissions or scopes (a change would make every
// existing connection reconnect_required).
func TestManifest080DeclaresProductTopicsAndPullImagesAndKeepsScopes(t *testing.T) {
	raw, err := os.ReadFile("../../manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version     string   `json:"version"`
		Permissions []string `json:"permissions"`
		Channel     struct {
			Auth struct {
				Scopes []string `json:"scopes"`
			} `json:"auth"`
			Capabilities map[string]string `json:"capabilities"`
			PushFields   []string          `json:"push_fields"`
			Webhooks     struct {
				Topics []string `json:"topics"`
			} `json:"webhooks"`
		} `json:"channel"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "0.8.0" {
		t.Errorf("version %s", m.Version)
	}
	if !reflect.DeepEqual(m.Permissions, []string{"net:{account}"}) {
		t.Errorf("permissions %v", m.Permissions)
	}
	wantScopes := []string{"read_products", "write_products", "read_inventory", "write_inventory", "read_locations", "read_orders", "write_orders",
		"read_merchant_managed_fulfillment_orders", "write_merchant_managed_fulfillment_orders"}
	if !reflect.DeepEqual(m.Channel.Auth.Scopes, wantScopes) {
		t.Errorf("scopes %v", m.Channel.Auth.Scopes)
	}
	topics := map[string]bool{}
	for _, tp := range m.Channel.Webhooks.Topics {
		topics[tp] = true
	}
	for _, tp := range []string{"products/create", "products/update", "products/delete", "orders/create", "orders/updated", "app/uninstalled", "customers/redact", "shop/redact", "customers/data_request"} {
		if !topics[tp] {
			t.Errorf("topic %s missing", tp)
		}
	}
	if m.Channel.Capabilities["images"] != "pull" {
		t.Errorf("capabilities.images = %q, want pull (inbound only)", m.Channel.Capabilities["images"])
	}
	if !reflect.DeepEqual(m.Channel.PushFields, []string{"price", "name", "description", "sku", "barcodes"}) {
		t.Errorf("push_fields %v: image must stay out (outbound off)", m.Channel.PushFields)
	}
}
