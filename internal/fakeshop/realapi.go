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
//   - Product.category is an object (TaxonomyCategory): selecting it bare is
//     refused; collections needs first or last; productUpdate takes
//     product: ProductUpdateInput! (the old input: argument is gone), the
//     input's category is a taxonomy ID (so a name there is refused), and
//     its userErrors are UserError (field, message): no code.
//
// customerRe matches the selection of the customer object in any spelling.
var (
	bareCategoryRe     = regexp.MustCompile(`\bcategory\s*[^{\s(]`)
	collectionsNoArgRe = regexp.MustCompile(`\bcollections\s*\{`)
	userErrCodeRe      = regexp.MustCompile(`userErrors\s*\{[^}]*\bcode\b`)
)
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
	if bareCategoryRe.MatchString(q) {
		return "Field must have selections (field 'category' returns TaxonomyCategory but has no selections. Did you mean 'category { ... }'?)", "selectionMismatch"
	}
	if collectionsNoArgRe.MatchString(q) {
		return "you must provide one of first or last", "BAD_REQUEST"
	}
	if strings.Contains(q, "productUpdate(") {
		if !strings.Contains(q, "productUpdate(product:") {
			return "Field 'productUpdate' doesn't accept argument 'input'", "argumentNotAccepted"
		}
		if userErrCodeRe.MatchString(q) {
			return "Field 'code' doesn't exist on type 'UserError'", "undefinedField"
		}
		var in map[string]json.RawMessage
		if json.Unmarshal(req.Variables["product"], &in) == nil {
			for k, raw := range in {
				switch k {
				case "id", "productType", "title", "status", "descriptionHtml":
					var sv string
					if json.Unmarshal(raw, &sv) != nil {
						return "Variable $product of type ProductUpdateInput! was provided invalid value for " + k + " (Could not coerce value to String)", "INVALID_VARIABLE"
					}
				default:
					return "Variable $product of type ProductUpdateInput! was provided invalid value for " + k + " (the adapter only ever sets productType, title, status and descriptionHtml)", "INVALID_VARIABLE"
				}
			}
		}
	}
	if strings.Contains(q, "productVariantsBulkUpdate(") {
		// ProductVariantsBulkInput: the SKU lives under inventoryItem, not at the top.
		var in []map[string]json.RawMessage
		if json.Unmarshal(req.Variables["variants"], &in) == nil {
			for _, v := range in {
				for k := range v {
					switch k {
					case "id", "price", "barcode", "inventoryItem":
					default:
						return "Variable $variants of type [ProductVariantsBulkInput!]! was provided invalid value for " + k + " (Field is not defined on ProductVariantsBulkInput)", "INVALID_VARIABLE"
					}
				}
			}
		}
	}
	if strings.Contains(q, "productCreateMedia(") {
		// CreateMediaInput: originalSource, alt, mediaContentType; the payload has mediaUserErrors.
		var in []map[string]json.RawMessage
		if json.Unmarshal(req.Variables["media"], &in) == nil {
			for _, m := range in {
				for k := range m {
					switch k {
					case "originalSource", "alt", "mediaContentType":
					default:
						return "Variable $media of type [CreateMediaInput!]! was provided invalid value for " + k + " (Field is not defined on CreateMediaInput)", "INVALID_VARIABLE"
					}
				}
			}
		}
	}
	if strings.Contains(q, "productReorderMedia(") {
		var in []map[string]json.RawMessage
		if json.Unmarshal(req.Variables["moves"], &in) == nil {
			for _, m := range in {
				for k, raw := range m {
					var sv string
					if (k != "id" && k != "newPosition") || json.Unmarshal(raw, &sv) != nil {
						return "Variable $moves of type [MoveInput!]! was provided invalid value for " + k + " (MoveInput takes id and newPosition as strings)", "INVALID_VARIABLE"
					}
				}
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
