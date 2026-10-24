package fakeshop

import (
	"encoding/json"
	"testing"
)

func rawVars(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]json.RawMessage{"input": b}
}

// The fake must refuse what the real Admin API 2026-10 refused in the owner's
// development-store run (ut-docs#4920), and accept the shapes that worked.
func TestRealAPIRefusal(t *testing.T) {
	const set = `mutation InventorySet($input: InventorySetQuantitiesInput!, $key: String!) { inventorySetQuantities(input: $input) @idempotent(key: $key) { userErrors { code } } }`
	line := func(extra map[string]any) map[string]any {
		m := map[string]any{"inventoryItemId": "gid://shopify/InventoryItem/1", "locationId": "gid://shopify/Location/1", "quantity": 1}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name string
		req  gqlRequest
		code string
	}{
		{"null compare is allowed", gqlRequest{Query: set, Variables: rawVars(t, map[string]any{"quantities": []any{line(map[string]any{"changeFromQuantity": nil})}})}, ""},
		{"integer compare is allowed", gqlRequest{Query: set, Variables: rawVars(t, map[string]any{"quantities": []any{line(map[string]any{"changeFromQuantity": 5})}})}, ""},
		{"missing changeFromQuantity", gqlRequest{Query: set, Variables: rawVars(t, map[string]any{"quantities": []any{line(nil)}})}, "INVALID_FIELD_ARGUMENTS"},
		{"ignoreCompareQuantity does not exist", gqlRequest{Query: set, Variables: rawVars(t, map[string]any{"ignoreCompareQuantity": true, "quantities": []any{line(map[string]any{"changeFromQuantity": nil})}})}, "INVALID_VARIABLE"},
		{"activate without @idempotent", gqlRequest{Query: `mutation A($item: ID!, $loc: ID!) { inventoryActivate(inventoryItemId: $item, locationId: $loc) { userErrors { field } } }`}, "BAD_REQUEST"},
		{"activate with @idempotent", gqlRequest{Query: `mutation A($item: ID!, $loc: ID!, $key: String!) { inventoryActivate(inventoryItemId: $item, locationId: $loc) @idempotent(key: $key) { userErrors { field } } }`}, ""},
		{"customer needs read_customers", gqlRequest{Query: `query O($id: ID!) { order(id: $id) { id customer { id } } }`}, "ACCESS_DENIED"},
		{"customer without a space", gqlRequest{Query: `query O($id: ID!) { order(id: $id) { id customer{ id } } }`}, "ACCESS_DENIED"},
		{"customer on a new line", gqlRequest{Query: "query O($id: ID!) { order(id: $id) { id customer\n{ id } } }"}, "ACCESS_DENIED"},
		{"notifyCustomer is not the object", gqlRequest{Query: `mutation C($n: Boolean) { orderCancel(notifyCustomer: $n) { userErrors { field } } }`}, ""},
		{"order without customer", gqlRequest{Query: `query O($id: ID!) { order(id: $id) { id shippingAddress { name } } }`}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, code := realAPIRefusal(c.req)
			if code != c.code || (code == "") != (msg == "") {
				t.Fatalf("code %q msg %q, want code %q", code, msg, c.code)
			}
		})
	}
}

func TestKeyMismatch(t *testing.T) {
	const q = `mutation M($input: X!, $key: String!) { m(input: $input) @idempotent(key: $key) { userErrors { code } } }`
	mk := func(op, key string, v int) gqlRequest {
		k, _ := json.Marshal(key)
		in, _ := json.Marshal(map[string]any{"quantity": v})
		return gqlRequest{OperationName: op, Query: q, Variables: map[string]json.RawMessage{"key": k, "input": in}}
	}
	seen := map[string]string{}
	for _, c := range []struct {
		name string
		req  gqlRequest
		want bool
	}{
		{"first use", mk("InventorySet", "op-1", 1), false},
		{"a replay of the same input", mk("InventorySet", "op-1", 1), false},
		{"the same key with other input", mk("InventorySet", "op-1", 2), true},
		{"another operation may reuse the key", mk("InventoryActivate", "op-1", 2), false},
		{"another key", mk("InventorySet", "op-1:new", 2), false},
	} {
		if got := keyMismatch(seen, c.req); got != c.want {
			t.Errorf("%s: mismatch = %v, want %v", c.name, got, c.want)
		}
	}
}
