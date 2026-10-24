package fakeshop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// realAPIRefusal makes the fake refuse what the real Admin API 2026-10 refused
// in the owner's development-store run (ut-docs#4920), so a regression to any
// of these shapes fails here and not only on a real store. It returns the
// message and the extensions code Shopify answered with, or "" when the
// request is fine.
//
//   - inventoryActivate must carry @idempotent (BAD_REQUEST).
//   - InventorySetQuantitiesInput has no ignoreCompareQuantity field, and every
//     quantity must include changeFromQuantity (null = no compare).
//   - the fake's token has no read_customers scope, so selecting `customer`
//     on an order is ACCESS_DENIED.
//
// customerRe matches the selection of the customer object in any spelling.
var customerRe = regexp.MustCompile(`\bcustomer\s*[{(]`)

func realAPIRefusal(req gqlRequest) (msg, code string) {
	q := req.Query
	if strings.Contains(q, "inventoryActivate(") && !strings.Contains(q, "@idempotent") {
		return "The @idempotent directive is required for this mutation but was not provided.", "BAD_REQUEST"
	}
	if strings.Contains(q, "inventorySetQuantities(") {
		var in struct {
			Fields     map[string]json.RawMessage
			Quantities []map[string]json.RawMessage `json:"quantities"`
		}
		raw := req.Variables["input"]
		if json.Unmarshal(raw, &in.Fields) == nil {
			if _, ok := in.Fields["ignoreCompareQuantity"]; ok {
				return "Variable $input of type InventorySetQuantitiesInput! was provided invalid value for ignoreCompareQuantity (Field is not defined on InventorySetQuantitiesInput)", "INVALID_VARIABLE"
			}
		}
		_ = json.Unmarshal(raw, &in)
		for _, line := range in.Quantities {
			if _, ok := line["changeFromQuantity"]; !ok {
				return "InventoryQuantityInput must include the following argument: changeFromQuantity.", "INVALID_FIELD_ARGUMENTS"
			}
		}
	}
	if customerRe.MatchString(q) {
		return "Access denied for customer field. Required access: `read_customers` access scope.", "ACCESS_DENIED"
	}
	return "", ""
}

// keyMismatch models @idempotent as the real API answered it on the owner's
// development store (ut-docs#4920): a key is remembered per operation, the same
// key with the same input is a replay, and the same key with DIFFERENT input is
// refused (IDEMPOTENCY_KEY_PARAMETER_MISMATCH). Another operation may reuse a
// key. It reports a mismatch; the caller holds the lock.
func keyMismatch(seen map[string]string, req gqlRequest) bool {
	if !strings.Contains(req.Query, "@idempotent") {
		return false
	}
	var key string
	if json.Unmarshal(req.Variables["key"], &key) != nil || key == "" {
		return false
	}
	names := make([]string, 0, len(req.Variables))
	for k := range req.Variables {
		if k != "key" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	h := sha256.New()
	for _, k := range names {
		var v any
		_ = json.Unmarshal(req.Variables[k], &v)
		b, _ := json.Marshal(v) // maps marshal with sorted keys: one canonical form
		h.Write([]byte(k))
		h.Write(b)
	}
	fp, id := hex.EncodeToString(h.Sum(nil)), req.OperationName+"|"+key
	if prev, ok := seen[id]; ok {
		return prev != fp
	}
	seen[id] = fp
	return false
}
