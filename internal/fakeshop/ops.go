package fakeshop

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type obj = map[string]any

func ms(amount int64, cur string) obj {
	return obj{"shopMoney": obj{"amount": minor(amount), "currencyCode": cur}}
}

// page slices a list: the cursor is "o:<offset>" in base64, as opaque to the
// adapter as Shopify's own.
func page(total, first int, after string) (start, end int, endCursor string, hasNext bool) {
	if after != "" {
		if b, err := base64.StdEncoding.DecodeString(after); err == nil && strings.HasPrefix(string(b), "o:") {
			fmt.Sscanf(string(b)[2:], "%d", &start)
		}
	}
	if start > total {
		start = total
	}
	end = start + first
	if end > total {
		end = total
	}
	endCursor = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("o:%d", end)))
	return start, end, endCursor, end < total
}

func pi(hasNext bool, end string) obj { return obj{"hasNextPage": hasNext, "endCursor": end} }

func (s *Shop) vars(req gqlRequest, name string, into any) {
	if raw, ok := req.Variables[name]; ok {
		_ = json.Unmarshal(raw, into)
	}
}

func (s *Shop) str(req gqlRequest, name string) string {
	var v string
	s.vars(req, name, &v)
	return v
}

func (s *Shop) num(req gqlRequest, name string, def int) int {
	v := def
	s.vars(req, name, &v)
	if v < 1 {
		v = def
	}
	return v
}

// dispatch answers one operation. Called with s.mu held.
func (s *Shop) dispatch(req gqlRequest) (any, string) {
	if s.schemaErr[req.OperationName] {
		return nil, "undefinedField"
	}
	if n, ok := s.failAfter[req.OperationName]; ok {
		if n <= 0 {
			return nil, "undefinedField"
		}
		s.failAfter[req.OperationName] = n - 1
	}
	switch req.OperationName {
	case "ConnectTest":
		var locs []obj
		for i := range s.locations {
			// Shopify's locations(...) defaults to includeInactive: false, so a
			// deactivated location is not listed at all (the import's
			// qLocations relies on the same default).
			if s.locOff[i] {
				continue
			}
			id, name := s.locations[i], fmt.Sprintf("Invented location %d", i+1)
			if v, ok := s.locID[i]; ok {
				id = v
			}
			if v, ok := s.locName[i]; ok {
				name = v
			}
			locs = append(locs, obj{"id": id, "name": name, "isActive": !s.locOff[i]})
		}
		var scopes []obj
		for _, h := range strings.Split("read_products,write_products,read_inventory,write_inventory,read_locations,read_orders,write_orders,read_merchant_managed_fulfillment_orders,write_merchant_managed_fulfillment_orders", ",") {
			scopes = append(scopes, obj{"handle": h})
		}
		return obj{"shop": obj{"name": "Invented Shop A"}, "currentAppInstallation": obj{"accessScopes": scopes},
			"locations": obj{"nodes": locs}, "productsCount": obj{"count": len(s.products)}, "ordersCount": obj{"count": len(s.orders)}}, ""
	case "ShopCurrency":
		return obj{"shop": obj{"currencyCode": s.opt.Currency, "taxesIncluded": !s.opt.TaxesExcluded}}, ""
	case "Products":
		return s.opProducts(req), ""
	case "MoreVariants":
		for _, p := range s.products {
			if p.ID == s.str(req, "id") {
				return obj{"node": obj{"variants": s.variantPage(p, s.num(req, "first", 50), s.str(req, "after"))}}, ""
			}
		}
		return obj{"node": nil}, ""
	case "Locations":
		first := s.num(req, "first", 10)
		var n []obj
		for i, l := range s.locations {
			if i < first {
				n = append(n, obj{"id": l})
			}
		}
		return obj{"locations": obj{"nodes": n}}, ""
	case "Levels":
		return s.opLevels(req), ""
	case "Orders":
		return s.opOrders(req)
	case "Order":
		for i := range s.orders {
			if s.orders[i].ID == s.str(req, "id") {
				return obj{"order": s.orderJSON(s.orders[i], s.num(req, "lfirst", 20), "")}, ""
			}
		}
		return obj{"order": nil}, ""
	case "OrderLines":
		for i := range s.orders {
			if s.orders[i].ID == s.str(req, "id") {
				j := s.orderJSON(s.orders[i], s.num(req, "first", 20), s.str(req, "after"))
				return obj{"node": obj{"lineItems": j["lineItems"]}}, ""
			}
		}
		return obj{"node": nil}, ""
	case "Contact":
		for _, o := range s.orders {
			if o.ID == s.str(req, "id") {
				return obj{"order": obj{"email": o.Customer.Email, "phone": o.Customer.Phone, "customer": obj{"displayName": o.Customer.Name},
					"shippingAddress": obj{"name": o.Customer.Name, "address1": o.Customer.Address, "address2": nil, "city": "Invented Town", "province": nil, "zip": "AB1 2CD", "country": "United Kingdom"}}}, ""
			}
		}
		return obj{"order": nil}, ""
	case "StockTarget":
		return s.opStockTarget(req), ""
	case "InventorySet":
		return s.opInventorySet(req), ""
	case "InventoryActivate":
		item, loc := s.str(req, "item"), s.str(req, "loc")
		known := false
		for _, l := range s.locations {
			known = known || l == loc
		}
		if !known {
			return obj{"inventoryActivate": obj{"userErrors": []obj{{"field": []string{"locationId"}}}}}, ""
		}
		for _, p := range s.products {
			for _, v := range p.Variants {
				if v.ItemID == item {
					if s.levels[v.ID+"@"+loc] == nil {
						s.levels[v.ID+"@"+loc] = &level{Qty: 0, UpdatedAt: s.bump()}
						s.acts["activate:"+v.ID]++
					}
					return obj{"inventoryActivate": obj{"userErrors": []obj{}}}, ""
				}
			}
		}
		return obj{"inventoryActivate": obj{"userErrors": []obj{{"field": []string{"inventoryItemId"}}}}}, ""
	case "VariantPrice":
		if v, pr := s.findVariant(s.str(req, "id")); v != nil {
			switch s.lookup {
			case "foreign": // answers with another variant than the one asked for
				return obj{"productVariant": obj{"id": gid("ProductVariant", 999999), "product": obj{"id": pr.ID}, "price": v.Price}}, ""
			case "noproduct": // a variant whose product is gone
				return obj{"productVariant": obj{"id": v.ID, "product": nil, "price": v.Price}}, ""
			}
			return obj{"productVariant": obj{"id": v.ID, "sku": v.SKU, "barcode": nullIfEmpty(v.Barcode), "product": obj{"id": pr.ID, "description": pr.Desc, "descriptionHtml": htmlOf(*pr), "title": pr.Title, "status": pr.Status, "updatedAt": pr.UpdatedAt, "variantsCount": obj{"count": len(pr.Variants)},
				"productType": pr.ProductType, "category": taxonomyJSON(*pr), "collections": collectionsJSON(*pr)}, "price": v.Price}}, ""
		}
		return obj{"productVariant": nil}, ""
	case "ProductType":
		return s.opProductType(req), ""
	case "ProductImage":
		return s.opProductImage(req), ""
	case "ProductCreateMedia":
		return s.opCreateMedia(req), ""
	case "ProductReorderMedia":
		return s.opReorderMedia(req), ""
	case "MetafieldsSet":
		return s.opMetafieldsSet(req), ""
	case "VariantPrices":
		return s.opPrices(req), ""
	case "FulfilmentOrders":
		for _, o := range s.orders {
			if o.ID == s.str(req, "id") {
				var fos []obj
				for _, f := range o.FOs {
					fos = append(fos, obj{"id": f.ID, "status": f.Status, "assignedLocation": obj{"location": obj{"id": f.Loc}}})
				}
				var cancelled any
				if o.CancelledAt != "" {
					cancelled = o.CancelledAt
				}
				return obj{"order": obj{"cancelledAt": cancelled, "displayFulfillmentStatus": o.Ful, "fulfillmentOrders": obj{"nodes": fos}}}, ""
			}
		}
		return obj{"order": nil}, ""
	case "FulfillmentCreate":
		return s.opFulfil(req), ""
	case "RefundState":
		return s.opRefundState(req)
	case "SuggestedRefund":
		return s.opSuggestedRefund(req)
	case "RefundCreate":
		return s.opRefundCreate(req)
	case "OrderCancel":
		return s.opOrderCancel(req)
	case "CancelJob":
		return s.opCancelJob(req)
	}
	return nil, "UNKNOWN_OPERATION"
}

func (s *Shop) variantPage(p product, first int, after string) obj {
	st, en, ec, more := page(len(p.Variants), first, after)
	var nodes []obj
	for _, v := range p.Variants[st:en] {
		var opts []obj
		for _, o := range v.Options {
			opts = append(opts, obj{"name": o[0], "value": o[1]})
		}
		var bc any
		if v.Barcode != "" {
			bc = v.Barcode
		}
		var cmp any
		if v.CompareAt != "" {
			cmp = v.CompareAt
		}
		nodes = append(nodes, obj{"id": v.ID, "sku": v.SKU, "barcode": bc, "price": v.Price, "compareAtPrice": cmp, "taxable": v.Taxable,
			"selectedOptions": opts, "inventoryItem": obj{"tracked": v.Tracked}})
	}
	return obj{"pageInfo": pi(more, ec), "nodes": nodes}
}

func (s *Shop) opProducts(req gqlRequest) obj {
	first := s.num(req, "first", 10)
	shop := obj{"currencyCode": s.opt.Currency, "taxesIncluded": !s.opt.TaxesExcluded}
	after := s.str(req, "after")
	if s.h.endless {
		// Never ends: every page is full and says there is more.
		var nodes []obj
		for i := 0; i < first; i++ {
			n := len(after)*100 + i + 100000
			nodes = append(nodes, obj{"id": gid("Product", n), "title": "Endless", "description": "", "status": "ACTIVE",
				"updatedAt": Base.Format(time.RFC3339), "variants": obj{"pageInfo": pi(false, ""), "nodes": []obj{}}})
		}
		return obj{"shop": shop, "products": obj{"pageInfo": pi(true, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("endless:%d", len(s.reqs))))), "nodes": nodes}}
	}
	st, en, ec, more := page(len(s.products), first, after)
	var nodes []obj
	for _, p := range s.products[st:en] {
		title, id := p.Title, p.ID
		if s.h.echo {
			title = fmt.Sprintf("%v", s.reqs[len(s.reqs)-1].Header)
		}
		if s.h.foreign {
			id = "gid://shopify/Product/../../admin"
		}
		nodes = append(nodes, obj{"id": id, "title": title, "description": p.Desc, "descriptionHtml": htmlOf(p), "status": p.Status, "updatedAt": p.UpdatedAt,
			"productType": p.ProductType, "category": taxonomyJSON(p), "collections": collectionsJSON(p), "featuredMedia": featuredJSON(p),
			"variants": s.variantPage(p, s.num(req, "vfirst", 50), "")})
	}
	return obj{"shop": shop, "products": obj{"pageInfo": pi(more, ec), "nodes": nodes}}
}

func (s *Shop) opLevels(req gqlRequest) obj {
	loc := s.str(req, "id")
	known := false
	for _, l := range s.locations {
		known = known || l == loc
	}
	if !known {
		return obj{"location": nil}
	}
	type row struct {
		v variant
		l *level
	}
	var rows []row
	for _, p := range s.products {
		for _, v := range p.Variants {
			if l := s.levels[v.ID+"@"+loc]; l != nil {
				rows = append(rows, row{v, l})
			}
		}
	}
	if s.h.endless {
		var nodes []obj
		for i := 0; i < s.num(req, "first", 50); i++ {
			nodes = append(nodes, obj{"updatedAt": Base.Format(time.RFC3339), "item": obj{"tracked": true, "variant": obj{"id": gid("ProductVariant", 900000+i)}}, "quantities": []obj{{"quantity": 1}}})
		}
		return obj{"location": obj{"inventoryLevels": obj{"pageInfo": pi(true, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("endless:%d", len(s.reqs))))), "nodes": nodes}}}
	}
	st, en, ec, more := page(len(rows), s.num(req, "first", 50), s.str(req, "after"))
	var nodes []obj
	for _, r := range rows[st:en] {
		nodes = append(nodes, obj{"updatedAt": r.l.UpdatedAt, "item": obj{"tracked": r.v.Tracked, "variant": obj{"id": r.v.ID}},
			"quantities": []obj{{"quantity": r.l.Qty}}})
	}
	return obj{"location": obj{"inventoryLevels": obj{"pageInfo": pi(more, ec), "nodes": nodes}}}
}

var sinceRe = regexp.MustCompile(`^updated_at:>='(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)'$`)

func (s *Shop) opOrders(req gqlRequest) (any, string) {
	since := ""
	if q := s.str(req, "query"); q != "" {
		m := sinceRe.FindStringSubmatch(q)
		if m == nil {
			return nil, "INVALID_SEARCH_QUERY"
		}
		since = m[1]
	}
	var list []Order
	for _, o := range s.orders {
		if o.UpdatedAt >= since {
			list = append(list, o)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt < list[j].UpdatedAt })
	if s.h.foreign {
		o := list[0]
		o.ID = "gid://shopify/Order/1/../../x?y=1"
		return obj{"orders": obj{"pageInfo": pi(false, "a b"), "nodes": []obj{s.orderJSON(o, 20, "")}}}, ""
	}
	st, en, ec, more := page(len(list), s.num(req, "first", 10), s.str(req, "after"))
	var nodes []obj
	for _, o := range list[st:en] {
		nodes = append(nodes, s.orderJSON(o, s.num(req, "lfirst", 20), ""))
	}
	return obj{"orders": obj{"pageInfo": pi(more, ec), "nodes": nodes}}, ""
}

// firstOf reads the `first: N` of the named connection in a query text.
func firstOf(query, field string) int {
	m := regexp.MustCompile(field + `\(first: (\d+)\)`).FindStringSubmatch(query)
	if m == nil {
		return 1 << 30
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func (s *Shop) orderJSON(o Order, lfirst int, lafter string) obj {
	cur := s.opt.Currency
	var taxLines = func(amount int64, rate float64) []obj {
		return []obj{{"rate": rate, "priceSet": ms(taxOf(amount, rate, o.TaxesIncluded), cur)}}
	}
	st, en, ec, more := page(len(o.Lines), lfirst, lafter)
	refunded := map[string]int64{}
	var refunds []obj
	// Shopify returns at most `first` of a connection: honour the query's own
	// numbers so an adapter that reads too few is caught.
	q := s.reqs[len(s.reqs)-1].Query
	maxRefunds, maxRefundLines := firstOf(q, "refunds"), firstOf(q, "refundLineItems")
	for ri, rf := range o.Refunds {
		if ri >= maxRefunds {
			break
		}
		var rl []obj
		for li, l := range rf {
			if li >= maxRefundLines {
				break
			}
			refunded[l.LineID] += l.Qty
			var lineGID string
			lineGID = l.LineID
			rl = append(rl, obj{"quantity": l.Qty, "restockType": l.RestockType, "lineItem": obj{"id": lineGID}})
		}
		refunds = append(refunds, obj{"refundLineItems": obj{"nodes": rl}})
	}
	var lines []obj
	for _, l := range o.Lines[st:en] {
		var vr any
		if l.VariantID != "" {
			vr = obj{"id": l.VariantID}
		}
		// Shopify's discountAllocations: every discount taken off the line, the
		// line's own and its share of an order-level code, as separate entries.
		allocs := []obj{}
		for _, a := range []int64{l.Discount, l.CodeAlloc} {
			if a > 0 {
				allocs = append(allocs, obj{"allocatedAmountSet": ms(a, cur)})
			}
		}
		lines = append(lines, obj{"id": l.ID, "name": l.Name, "sku": l.SKU, "quantity": l.Qty, "currentQuantity": l.CurQty, "variant": vr,
			"originalUnitPriceSet": ms(l.Unit, cur), "discountAllocations": allocs, "taxLines": taxLines(l.net(), l.Rate)})
	}
	var cancelled any
	if o.CancelledAt != "" {
		cancelled = o.CancelledAt
	}
	shipping := []obj{{"discountedPriceSet": ms(o.Ship, cur), "taxLines": taxLines(o.Ship, o.ShipRate)}}
	for _, x := range o.ExtraShip {
		shipping = append(shipping, obj{"discountedPriceSet": ms(x.Amount, cur), "taxLines": taxLines(x.Amount, x.Rate)})
	}
	name := o.Name
	if s.h.echo {
		name = fmt.Sprintf("%v", s.reqs[len(s.reqs)-1].Header)
	}
	return obj{
		"id": o.ID, "name": name, "createdAt": o.CreatedAt, "updatedAt": o.UpdatedAt, "cancelledAt": cancelled, "currencyCode": cur,
		"taxesIncluded": o.TaxesIncluded, "displayFinancialStatus": o.Fin, "displayFulfillmentStatus": o.Ful,
		"totalPriceSet": ms(o.Total, cur), "totalRefundedSet": ms(o.RefundTotal, cur),
		"shippingLines": obj{"nodes": shipping},
		"totalTaxSet":   ms(orderTax(o), cur),
		// The shop sends the buyer's details whether asked or not: the
		// adapter must not copy any of them.
		"email": o.Customer.Email, "phone": o.Customer.Phone,
		"customer":        obj{"id": o.Customer.ID, "displayName": o.Customer.Name, "email": o.Customer.Email, "phone": o.Customer.Phone},
		"shippingAddress": obj{"countryCodeV2": o.Customer.Country, "name": o.Customer.Name, "address1": o.Customer.Address, "phone": o.Customer.Phone},
		"refunds":         refunds,
		"lineItems":       obj{"pageInfo": pi(more, ec), "nodes": lines},
	}
}

func (s *Shop) opStockTarget(req gqlRequest) obj {
	var locs []obj
	for i, l := range s.locations {
		if i < 3 {
			locs = append(locs, obj{"id": l})
		}
	}
	if v, _ := s.findVariant(s.str(req, "id")); v != nil {
		var ls []obj
		for _, l := range s.locations {
			if lv := s.levels[v.ID+"@"+l]; lv != nil {
				ls = append(ls, obj{"location": obj{"id": l}, "quantities": []obj{{"quantity": lv.Qty}}})
			}
		}
		return obj{"node": obj{"inventoryItem": obj{"id": v.ItemID, "tracked": v.Tracked, "inventoryLevels": obj{"nodes": ls}}}, "locations": obj{"nodes": locs}}
	}
	return obj{"node": nil, "locations": obj{"nodes": locs}}
}

func (s *Shop) opInventorySet(req gqlRequest) obj {
	key := s.str(req, "key")
	if cached, ok := s.idem[key]; ok {
		var out obj
		_ = json.Unmarshal([]byte(cached), &out)
		return out
	}
	fail := func(code string) obj {
		return obj{"inventorySetQuantities": obj{"userErrors": []obj{{"code": code}}}}
	}
	var in struct {
		Name       string
		Reason     string
		Quantities []struct {
			InventoryItemID    string `json:"inventoryItemId"`
			LocationID         string `json:"locationId"`
			Quantity           int64  `json:"quantity"`
			ChangeFromQuantity *int64 `json:"changeFromQuantity"`
		}
	}
	s.vars(req, "input", &in)
	if in.Name != "available" || len(in.Quantities) != 1 || in.Quantities[0].Quantity < 0 {
		return fail("INVALID_QUANTITY_NEGATIVE")
	}
	q := in.Quantities[0]
	for _, p := range s.products {
		for _, v := range p.Variants {
			if v.ItemID != q.InventoryItemID {
				continue
			}
			lv := s.levels[v.ID+"@"+q.LocationID]
			switch {
			case lv == nil:
				return fail("ITEM_NOT_STOCKED_AT_LOCATION")
			case !v.Tracked:
				return fail("ITEM_NOT_TRACKED")
			case q.ChangeFromQuantity != nil && *q.ChangeFromQuantity != lv.Qty:
				return fail("CHANGE_FROM_QUANTITY_STALE")
			}
			lv.Qty, lv.UpdatedAt = q.Quantity, s.bump()
			s.acts["inventory:"+v.ID]++
			ok := obj{"inventorySetQuantities": obj{"userErrors": []obj{}}}
			b, _ := json.Marshal(ok)
			s.idem[key] = string(b)
			return ok
		}
	}
	return fail("INVALID_ITEM")
}

var priceRe = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,3})?$`)

func (s *Shop) opPrices(req gqlRequest) obj {
	fail := func() obj {
		return obj{"productVariantsBulkUpdate": obj{"userErrors": []obj{{"code": "PRODUCT_VARIANT_DOES_NOT_EXIST"}}}}
	}
	var in []struct {
		ID, Price string
		Barcode   *string
		Inventory *struct{ SKU *string } `json:"inventoryItem"`
	}
	s.vars(req, "variants", &in)
	var p *product
	for i := range s.products {
		if s.products[i].ID == s.str(req, "productId") {
			p = &s.products[i]
		}
	}
	if p == nil || len(in) == 0 {
		return fail()
	}
	for _, x := range in { // all or nothing, like Shopify
		ok := false
		for _, v := range p.Variants {
			ok = ok || v.ID == x.ID
		}
		if !ok || (x.Price != "" && !priceRe.MatchString(x.Price)) {
			return fail()
		}
	}
	for _, x := range in {
		for i := range p.Variants {
			if p.Variants[i].ID == x.ID {
				if x.Price != "" {
					p.Variants[i].Price = x.Price
					s.acts["price:"+x.ID]++
				}
				if x.Barcode != nil {
					p.Variants[i].Barcode = *x.Barcode
					s.acts["barcode:"+x.ID]++
				}
				if x.Inventory != nil && x.Inventory.SKU != nil {
					p.Variants[i].SKU = *x.Inventory.SKU
					s.acts["sku:"+x.ID]++
				}
			}
		}
	}
	p.UpdatedAt = s.bump()
	return obj{"productVariantsBulkUpdate": obj{"userErrors": []obj{}}}
}

func (s *Shop) opFulfil(req gqlRequest) obj {
	fail := obj{"fulfillmentCreate": obj{"userErrors": []obj{{"field": []string{"fulfillment"}}}}}
	var in struct {
		TrackingInfo struct{ Company, Number, URL string }
		Lines        []struct {
			FulfillmentOrderID string `json:"fulfillmentOrderId"`
		} `json:"lineItemsByFulfillmentOrder"`
	}
	s.vars(req, "fulfillment", &in)
	if len(in.Lines) == 0 {
		return fail
	}
	// Validate everything first: like Shopify, one bad entry refuses the
	// whole fulfillment, and every entry must be at one location.
	var order *Order
	loc := ""
	for _, l := range in.Lines {
		found := false
		for i := range s.orders {
			o := &s.orders[i]
			for j := range o.FOs {
				f := o.FOs[j]
				if f.ID != l.FulfillmentOrderID {
					continue
				}
				found = true
				if (order != nil && order != o) || o.CancelledAt != "" || s.failFO[f.ID] ||
					(f.Status != "OPEN" && f.Status != "IN_PROGRESS") || (loc != "" && loc != f.Loc) {
					return fail
				}
				order, loc = o, f.Loc
			}
		}
		if !found {
			return fail
		}
	}
	for _, l := range in.Lines {
		for j := range order.FOs {
			if order.FOs[j].ID == l.FulfillmentOrderID {
				order.FOs[j].Status = "CLOSED"
			}
		}
	}
	s.acts["fulfil:"+order.ID]++
	s.tracking[order.ID] = [3]string{in.TrackingInfo.Company, in.TrackingInfo.Number, in.TrackingInfo.URL}
	all := true
	for _, f := range order.FOs {
		all = all && f.Status == "CLOSED"
	}
	if all {
		order.Ful = "FULFILLED"
	}
	order.UpdatedAt = s.bump()
	return obj{"fulfillmentCreate": obj{"userErrors": []obj{}}}
}

// AddFulfilmentOrder gives an order one more fulfilment order, assigned to
// the location with that number; it returns the new id.
func (s *Shop) AddFulfilmentOrder(orderID string, location int) string {
	id := ""
	s.MutateOrder(orderID, func(o *Order) {
		id = gid("FulfillmentOrder", 8500+len(o.FOs)+location*10)
		o.FOs = append(o.FOs, fulfilmentOrder{ID: id, Status: "OPEN", Loc: gid("Location", 7000+location)})
	})
	return id
}

// RefuseFulfilmentOrder makes every fulfillment that holds this fulfilment
// order come back with a user error (the location cannot ship, say).
func (s *Shop) RefuseFulfilmentOrder(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFO == nil {
		s.failFO = map[string]bool{}
	}
	s.failFO[id] = true
}

// SchemaError makes the named operation fail like a query Shopify's schema
// does not know: a GraphQL error with an undefinedField code.
func (s *Shop) SchemaError(operation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaErr == nil {
		s.schemaErr = map[string]bool{}
	}
	s.schemaErr[operation] = true
}

// FailAfter lets the named operation succeed n more times, then fail like
// SchemaError does (a mid-sweep failure). A negative n clears it.
func (s *Shop) FailAfter(operation string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAfter == nil {
		s.failAfter = map[string]int{}
	}
	if n < 0 {
		delete(s.failAfter, operation)
		return
	}
	s.failAfter[operation] = n
}

// --- merchant-side edits for tests ---------------------------------------

// FulfilExternally fulfils an order as the merchant would in Shopify's admin.
func (s *Shop) FulfilExternally(orderID string) {
	s.MutateOrder(orderID, func(o *Order) {
		for i := range o.FOs {
			o.FOs[i].Status = "CLOSED"
		}
		o.Ful = "FULFILLED"
	})
}

// CancelOrder cancels an order as the merchant would.
func (s *Shop) CancelOrder(orderID string) {
	s.MutateOrder(orderID, func(o *Order) {
		o.CancelledAt = Base.Add(72 * time.Hour).Format(time.RFC3339)
		for i := range o.FOs {
			o.FOs[i].Status = "CANCELLED"
		}
	})
}

// RefundLine refunds qty units of the order's line index idx; restockType is
// RETURN, CANCEL, LEGACY_RESTOCK or NO_RESTOCK. The refund total grows by the
// units' price.
func (s *Shop) RefundLine(orderID string, idx int, qty int64, restockType string) {
	s.MutateOrder(orderID, func(o *Order) {
		l := o.Lines[idx]
		o.Refunds = append(o.Refunds, []refundLine{{LineID: l.ID, Qty: qty, RestockType: restockType}})
		o.RefundTotal += qty * l.Unit
		if o.RefundTotal >= o.Total {
			o.Fin = "REFUNDED"
		} else {
			o.Fin = "PARTIALLY_REFUNDED"
		}
	})
}

// SetOrderCodeDiscount allocates amount (minor units) of an order-level
// discount code to the order's line idx; the total follows.
func (s *Shop) SetOrderCodeDiscount(orderID string, idx int, amount int64) {
	s.MutateOrder(orderID, func(o *Order) {
		o.Lines[idx].CodeAlloc = amount
		o.Total = totalOf(*o)
	})
}

// RefundLines adds ONE refund with n refund lines of one unit each of line idx.
func (s *Shop) RefundLines(orderID string, idx int, n int) {
	s.MutateOrder(orderID, func(o *Order) {
		var rl []refundLine
		for i := 0; i < n; i++ {
			rl = append(rl, refundLine{LineID: o.Lines[idx].ID, Qty: 1, RestockType: "NO_RESTOCK"})
		}
		o.Refunds = append(o.Refunds, rl)
	})
}

// EditRemoveUnits removes units from the order's line idx as an order edit
// does: currentQuantity drops, quantity stays. On a paid order Shopify also
// refunds the removed units (a refund line, no restock); on an unpaid order
// there is no refund line.
func (s *Shop) EditRemoveUnits(orderID string, idx int, qty int64) {
	s.MutateOrder(orderID, func(o *Order) {
		l := &o.Lines[idx]
		l.CurQty -= qty
		if o.Fin == "PAID" || o.Fin == "PARTIALLY_REFUNDED" {
			o.Refunds = append(o.Refunds, []refundLine{{LineID: l.ID, Qty: qty, RestockType: "NO_RESTOCK"}})
			o.RefundTotal += qty * l.Unit
		}
	})
}

// EditAddUnits adds units to the order's line index idx, as an order edit
// does: quantity and total grow, nothing is lowered.
func (s *Shop) EditAddUnits(orderID string, idx int, qty int64) {
	s.MutateOrder(orderID, func(o *Order) {
		o.Lines[idx].Qty += qty
		o.Lines[idx].CurQty += qty
		o.Total = totalOf(*o)
	})
}

// AddVariants appends n more variants to product i (1-based), stocked at every
// location, for the variant limit and variant paging.
func (s *Shop) AddVariants(i, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &s.products[i-1]
	for k := 0; k < n; k++ {
		num := 20000 + i*1000 + len(p.Variants)
		v := variant{ID: gid("ProductVariant", num), SKU: fmt.Sprintf("INV-%04d-X%d", i, num), Price: "9.99", ItemID: gid("InventoryItem", num+500000),
			Taxable: true, Tracked: true, Options: [][2]string{{"Size", fmt.Sprintf("X%d", num)}}}
		p.Variants = append(p.Variants, v)
		for _, l := range s.locations {
			s.levels[v.ID+"@"+l] = &level{Qty: 1, UpdatedAt: p.UpdatedAt}
		}
	}
}

// SetOptions replaces the options of variant id (for the option limit).
func (s *Shop) SetOptions(variantID string, opts [][2]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, _ := s.findVariant(variantID); v != nil {
		v.Options = opts
	}
}

func taxonomyJSON(p product) any {
	if p.Taxonomy == "" {
		return nil
	}
	return obj{"name": p.Taxonomy}
}

// collectionsJSON answers collections(first: 1): the first collection only.
func collectionsJSON(p product) obj {
	n := []obj{}
	if len(p.Collections) > 0 {
		n = append(n, obj{"title": p.Collections[0]})
	}
	return obj{"nodes": n}
}

// opProductType is productUpdate(product: {id, productType, title, status}).
// It changes the fields that are present and nothing else; an unknown product
// is a userError. A title must be 1..255 characters (Shopify refuses others).
func (s *Shop) opProductType(req gqlRequest) obj {
	var in struct {
		ID          string  `json:"id"`
		ProductType *string `json:"productType"`
		Title       *string `json:"title"`
		Status      *string `json:"status"`
		DescHTML    *string `json:"descriptionHtml"`
	}
	s.vars(req, "product", &in)
	if s.refuseType {
		return obj{"productUpdate": obj{"userErrors": []obj{{"field": []string{"productType"}}}}}
	}
	if in.Title != nil && (strings.TrimSpace(*in.Title) == "" || len([]rune(*in.Title)) > 255) {
		return obj{"productUpdate": obj{"userErrors": []obj{{"field": []string{"title"}}}}}
	}
	for i := range s.products {
		if s.products[i].ID == in.ID {
			if in.ProductType != nil {
				s.products[i].ProductType = *in.ProductType
				s.acts["producttype:"+in.ID]++
			}
			if in.Title != nil {
				s.products[i].Title = *in.Title
				s.acts["title:"+in.ID]++
			}
			if in.DescHTML != nil {
				s.products[i].DescHTML = *in.DescHTML
				s.products[i].Desc = htmlToPlain(*in.DescHTML)
				s.acts["description:"+in.ID]++
			}
			if in.Status != nil {
				s.products[i].Status = *in.Status
				s.acts["status:"+in.ID]++
			}
			s.products[i].UpdatedAt = s.bump()
			return obj{"productUpdate": obj{"userErrors": []obj{}}}
		}
	}
	return obj{"productUpdate": obj{"userErrors": []obj{{"field": []string{"id"}}}}}
}

// htmlToPlain is what Shopify's Product.description gives for descriptionHtml,
// as observed on the dev store 2026-11-23 (ut-docs#6843): every tag is removed
// with NOTHING in its place, so "<p>a</p><p>b</p>" reads "ab"; entities are
// decoded. The adapter therefore reads descriptionHtml, never this.
func htmlToPlain(h string) string {
	h = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(h, "")
	return strings.TrimSpace(html.UnescapeString(h))
}

// htmlOf is what Product.descriptionHtml reads: what was last written, else the
// plain text the seed or the merchant set.
func htmlOf(p product) string {
	if p.DescHTML != "" {
		return p.DescHTML
	}
	return p.Desc
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func featuredJSON(p product) any {
	if len(p.Media) == 0 {
		return nil
	}
	return obj{"id": p.Media[0].ID, "image": obj{"url": p.Media[0].Source}}
}

func (s *Shop) opProductImage(req gqlRequest) obj {
	for _, p := range s.products {
		if p.ID == s.str(req, "id") {
			var mf any
			if p.ImageSHA != "" {
				mf = obj{"value": p.ImageSHA}
			}
			var mref any
			if p.ImageMediaID != "" {
				mref = obj{"value": p.ImageMediaID}
			}
			nodes := []obj{}
			for i, m := range p.Media {
				if i >= 10 { // media(first: 10)
					break
				}
				nodes = append(nodes, obj{"id": m.ID, "status": m.Status})
			}
			return obj{"product": obj{"id": p.ID, "metafield": mf, "mediaRef": mref, "media": obj{"nodes": nodes}}}
		}
	}
	return obj{"product": nil}
}

func (s *Shop) opCreateMedia(req gqlRequest) obj {
	var in []struct {
		Source string `json:"originalSource"`
		Alt    string `json:"alt"`
		Type   string `json:"mediaContentType"`
	}
	s.vars(req, "media", &in)
	bad := obj{"productCreateMedia": obj{"media": []obj{}, "mediaUserErrors": []obj{{"field": []string{"media"}, "code": "INVALID"}}}}
	if s.refuseMedia || len(in) != 1 || !strings.HasPrefix(in[0].Source, "https://") || in[0].Type != "IMAGE" {
		return bad
	}
	for i := range s.products {
		if s.products[i].ID == s.str(req, "productId") {
			s.mediaSeq++
			m := Media{ID: gid("MediaImage", 9000+s.mediaSeq), Source: in[0].Source, Alt: in[0].Alt, Status: "UPLOADED"}
			s.products[i].Media = append(s.products[i].Media, m)
			s.acts["media:"+s.products[i].ID]++
			return obj{"productCreateMedia": obj{"media": []obj{{"id": m.ID}}, "mediaUserErrors": []obj{}}}
		}
	}
	return bad
}

func (s *Shop) opReorderMedia(req gqlRequest) obj {
	var moves []struct {
		ID  string `json:"id"`
		Pos string `json:"newPosition"`
	}
	s.vars(req, "moves", &moves)
	for i := range s.products {
		if s.products[i].ID != s.str(req, "id") {
			continue
		}
		for _, mv := range moves {
			pos, err := strconv.Atoi(mv.Pos)
			from := -1
			for j, m := range s.products[i].Media {
				if m.ID == mv.ID {
					from = j
				}
			}
			if err != nil || from < 0 || pos < 0 || pos >= len(s.products[i].Media) {
				return obj{"productReorderMedia": obj{"mediaUserErrors": []obj{{"field": []string{"moves"}, "code": "INVALID_MOVE"}}}}
			}
			m := s.products[i].Media[from]
			rest := append(append([]Media(nil), s.products[i].Media[:from]...), s.products[i].Media[from+1:]...)
			s.products[i].Media = append(append(append([]Media(nil), rest[:pos]...), m), rest[pos:]...)
		}
		return obj{"productReorderMedia": obj{"mediaUserErrors": []obj{}}}
	}
	return obj{"productReorderMedia": obj{"mediaUserErrors": []obj{{"field": []string{"id"}, "code": "PRODUCT_DOES_NOT_EXIST"}}}}
}

func (s *Shop) opMetafieldsSet(req gqlRequest) obj {
	var in []struct {
		Owner, Namespace, Key, Type, Value string
	}
	var raw []map[string]string
	s.vars(req, "metafields", &raw)
	for _, r := range raw {
		in = append(in, struct{ Owner, Namespace, Key, Type, Value string }{r["ownerId"], r["namespace"], r["key"], r["type"], r["value"]})
	}
	for _, m := range in {
		if m.Namespace != "unitill" || (m.Key != "image_sha256" && m.Key != "image_media_id") || m.Type != "single_line_text_field" {
			return obj{"metafieldsSet": obj{"userErrors": []obj{{"field": []string{"metafields"}, "code": "INVALID_TYPE"}}}}
		}
		for i := range s.products {
			if s.products[i].ID == m.Owner {
				if m.Key == "image_media_id" {
					s.products[i].ImageMediaID = m.Value
				} else {
					s.products[i].ImageSHA = m.Value
				}
			}
		}
	}
	return obj{"metafieldsSet": obj{"userErrors": []obj{}}}
}
