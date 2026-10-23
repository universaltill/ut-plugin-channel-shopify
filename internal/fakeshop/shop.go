// Package fakeshop is an invented Shopify store for tests: the subset of the
// Admin GraphQL API the adapter speaks (products and variants with
// pagination, inventory levels, orders with refunds, fulfilment orders,
// throttling, variable checking), HMAC-signed webhooks and a stub of the
// OAuth endpoints. It is test-only code and never part of the wasm module.
//
// It does NOT parse GraphQL. It dispatches on the request's operationName,
// reads the variables, and checks the text only for what the contract demands
// of an adapter: every `$variable` the text uses is declared and supplied and
// nothing is supplied that is not declared (so an interpolated value or a
// stray variable is a GraphQL error). Orders always carry the buyer's name,
// email, phone and address, selected or not, to prove the adapter never
// copies them.
//
// Everything is invented data. The server speaks only to a caller presenting
// the token in Header, and has NO delete mutation: the contract has none.
package fakeshop

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Header carries the access token on every call (manifest
// channel.auth.inject.header).
const Header = "X-Shopify-Access-Token"

// WebhookHeader carries the base64 HMAC-SHA256 of a webhook body (manifest
// channel.webhooks.verify.header).
const WebhookHeader = "X-Shopify-Hmac-Sha256"

// APIPath is the one endpoint of the Admin GraphQL API version in use.
const APIPath = "/admin/api/2026-10/graphql.json"

// Base is the time every seeded record is dated from.
var Base = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// Options configure a Shop. Zero values get safe defaults.
type Options struct {
	Account  string // default shop-a.myshopify.com
	Token    string // access token the API accepts
	ClientID string
	Secret   string // client secret: signs the OAuth callback and webhooks
	Products int    // seeded products, default 50
	// Locations is the number of stock locations, default 1.
	Locations int
	// Currency is the shop currency, default GBP. TaxesExcluded makes the
	// shop's prices exclude tax (default: included).
	Currency      string
	TaxesExcluded bool
	// ThrottleEvery answers every Nth call HTTP 429; GraphQLThrottleEvery
	// answers every Nth call 200 with a THROTTLED error (0 = never).
	ThrottleEvery        int
	GraphQLThrottleEvery int
}

type hostile struct{ endless, foreign, echo, huge bool }

type variant struct {
	ID, SKU, Barcode, Price, ItemID string
	Taxable                         bool
	Options                         [][2]string
	Tracked                         bool
}

type product struct {
	ID, Title, Desc, Status, UpdatedAt string
	Variants                           []variant
}

type level struct {
	Qty       int64
	UpdatedAt string
}

type orderLine struct {
	ID, Name, SKU, VariantID string
	Qty, CurQty              int64
	Unit, Discount           int64 // minor units; Discount is for the whole line
	// CodeAlloc is the share of an ORDER-level discount code allocated to this
	// line (Shopify's discountAllocations). Discount excludes it.
	CodeAlloc int64
	Rate      float64
}

// net is the line's price after its own discount and its share of any order
// level discount code.
func (l orderLine) net() int64 { return l.Qty*l.Unit - l.Discount - l.CodeAlloc }

type refundLine struct {
	LineID      string
	Qty         int64
	RestockType string
}

// Customer is what the shop knows of the buyer. The adapter must never copy
// any of it into a submitted order.
type Customer struct {
	ID, Country, Name, Email, Phone, Address string
}

// ShipLine is one more shipping line of an order.
type ShipLine struct {
	Amount int64
	Rate   float64
}

// fulfilmentOrder is Shopify's FulfillmentOrder: assigned to one location, and
// a fulfillment may only hold fulfilment orders of one location.
type fulfilmentOrder struct{ ID, Status, Loc string }

// Order is an order as the shop holds it (amounts in minor units).
type Order struct {
	ID, Name, CreatedAt, UpdatedAt, CancelledAt string
	TaxesIncluded                               bool
	Fin, Ful                                    string
	Lines                                       []orderLine
	Ship                                        int64
	ShipRate                                    float64
	// ExtraShip are further shipping lines beyond the first; TaxTotal, when
	// set, is the order-level tax (Shopify rounds it once per order, so it can
	// differ from the sum of the per-line taxes by a cent); Unaccounted is an
	// amount in the total that no line, shipping line or tax explains (a duty
	// or a tip).
	ExtraShip          []ShipLine
	TaxTotal           *int64
	Unaccounted        int64
	Total, RefundTotal int64
	Refunds            [][]refundLine
	FOs                []fulfilmentOrder
	Customer           Customer
	Tracking           string
}

// Request is one request the server saw.
type Request struct {
	Method, Path, Host string
	Header             http.Header
	Op                 string
	Query              string
	Variables          map[string]json.RawMessage
}

// Shop is the server. Use Handler (or the Shop itself) to serve it.
type Shop struct {
	mu        sync.Mutex
	opt       Options
	h         hostile
	products  []product
	locations []string
	levels    map[string]*level // variantID + "@" + locationID
	orders    []Order
	reqs      []Request
	writes    int
	calls     int
	idem      map[string]string // idempotency key -> cached mutation answer
	acts      map[string]int    // "<kind>:<id>" -> times performed
	failFO    map[string]bool   // fulfilment orders whose fulfillment is refused
	schemaErr map[string]bool   // operations answered with a schema error
	codes     map[string]bool
	tracking  map[string][3]string
}

// New returns a seeded shop.
func New(o Options) *Shop {
	if o.Account == "" {
		o.Account = "shop-a.myshopify.com"
	}
	if o.Token == "" {
		o.Token = "invented-access-token-1"
	}
	if o.ClientID == "" {
		o.ClientID = "invented-client-id"
	}
	if o.Secret == "" {
		o.Secret = "invented-client-secret"
	}
	if o.Products == 0 {
		o.Products = 50
	}
	if o.Locations == 0 {
		o.Locations = 1
	}
	if o.Currency == "" {
		o.Currency = "GBP"
	}
	s := &Shop{opt: o, levels: map[string]*level{}, idem: map[string]string{}, acts: map[string]int{}, codes: map[string]bool{}, tracking: map[string][3]string{}}
	for i := 0; i < o.Locations; i++ {
		s.locations = append(s.locations, gid("Location", 7001+i))
	}
	for i := 1; i <= o.Products; i++ {
		p := seedProduct(i)
		s.products = append(s.products, p)
		for _, v := range p.Variants {
			for _, l := range s.locations {
				s.levels[v.ID+"@"+l] = &level{Qty: int64(10 + i%7), UpdatedAt: p.UpdatedAt}
			}
		}
	}
	s.orders = seedOrders(s.products, o)
	return s
}

func gid(t string, n int) string { return fmt.Sprintf("gid://shopify/%s/%d", t, n) }

func seedProduct(i int) product {
	p := product{
		ID: gid("Product", 1000+i), Title: fmt.Sprintf("Invented item %d", i), Desc: "Invented plain text",
		Status: "ACTIVE", UpdatedAt: Base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
	}
	if i%3 == 0 {
		p.Status = "DRAFT"
	}
	sizes := []string{"S", "M", "L"}[:1+i%3]
	for j, size := range sizes {
		v := variant{
			ID: gid("ProductVariant", 2000+i*10+j), SKU: fmt.Sprintf("INV-%04d-%s", i, size),
			Barcode: fmt.Sprintf("50000%08d", i*10+j), Price: fmt.Sprintf("%d.%02d", 5+i, 10+j),
			ItemID: gid("InventoryItem", 3000+i*10+j), Taxable: true, Tracked: true,
			Options: [][2]string{{"Size", size}},
		}
		if len(sizes) == 1 {
			v.Options = [][2]string{{"Title", "Default Title"}}
		}
		p.Variants = append(p.Variants, v)
	}
	return p
}

func minor(n int64) string { return fmt.Sprintf("%d.%02d", n/100, n%100) }

func seedOrders(ps []product, o Options) []Order {
	var out []Order
	for n := 1; n <= 3 && n <= len(ps); n++ {
		v := ps[n-1].Variants[0]
		unit := parseMinor(v.Price)
		ts := Base.Add(time.Duration(n) * time.Hour).Format(time.RFC3339)
		ord := Order{
			ID: gid("Order", 5000+n), Name: fmt.Sprintf("#%d", 1000+n), CreatedAt: ts, UpdatedAt: ts,
			TaxesIncluded: !o.TaxesExcluded, Fin: "PAID", Ful: "UNFULFILLED", Ship: 399, ShipRate: 0.2,
			Lines: []orderLine{{ID: gid("LineItem", 6000+n), Name: ps[n-1].Title, SKU: v.SKU, VariantID: v.ID, Qty: int64(n), CurQty: int64(n), Unit: unit, Rate: 0.2}},
			FOs:   []fulfilmentOrder{{ID: gid("FulfillmentOrder", 8000+n), Status: "OPEN", Loc: gid("Location", 1)}},
			Customer: Customer{ID: gid("Customer", 9000+n), Country: "GB", Name: "Invented Person", Email: "invented@example.invalid",
				Phone: "+440000000000", Address: "1 Invented Street"},
		}
		if n == 2 {
			ord.Lines[0].Discount = 100
		}
		ord.Total = totalOf(ord)
		out = append(out, ord)
	}
	return out
}

func parseMinor(s string) int64 {
	a, b, _ := strings.Cut(s, ".")
	x, _ := strconv.ParseInt(a, 10, 64)
	y, _ := strconv.ParseInt((b + "00")[:2], 10, 64)
	return x*100 + y
}

// taxOf is the tax on an amount at rate: contained in it when included.
func taxOf(amount int64, rate float64, included bool) int64 {
	if included {
		return int64(float64(amount)*rate/(1+rate) + 0.5)
	}
	return int64(float64(amount)*rate + 0.5)
}

// lineTaxes is the sum of the per-line (and per-shipping-line) taxes.
func lineTaxes(o Order) int64 {
	var t int64
	for _, l := range o.Lines {
		t += taxOf(l.net(), l.Rate, o.TaxesIncluded)
	}
	t += taxOf(o.Ship, o.ShipRate, o.TaxesIncluded)
	for _, x := range o.ExtraShip {
		t += taxOf(x.Amount, x.Rate, o.TaxesIncluded)
	}
	return t
}

// orderTax is the order-level tax (totalTaxSet).
func orderTax(o Order) int64 {
	if o.TaxTotal != nil {
		return *o.TaxTotal
	}
	return lineTaxes(o)
}

// totalOf is what the buyer paid: lines after discount, shipping, and the tax
// added on top when prices exclude tax.
func totalOf(o Order) int64 {
	var t int64
	for _, l := range o.Lines {
		t += l.net()
	}
	t += o.Ship
	for _, x := range o.ExtraShip {
		t += x.Amount
	}
	if !o.TaxesIncluded {
		t += orderTax(o)
	}
	return t + o.Unaccounted
}

// --- accessors for tests --------------------------------------------------

func (s *Shop) Account() string { return s.opt.Account }
func (s *Shop) Token() string   { return s.opt.Token }
func (s *Shop) Secret() string  { return s.opt.Secret }

// Handler serves the shop.
func (s *Shop) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// ServeHTTP makes a Shop an http.Handler.
func (s *Shop) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.serve(w, r) }

// Hostile switches one mode on by the conformance suite's name for it.
func (s *Shop) Hostile(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch mode {
	case "endless_pages":
		s.h.endless = true
	case "foreign_links":
		s.h.foreign = true
	case "echo_headers":
		s.h.echo = true
	case "huge_body":
		s.h.huge = true
	default:
		panic("fakeshop: unknown hostile mode " + mode)
	}
}

// Requests returns a copy of every request seen.
func (s *Shop) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Writes counts mutations (GraphQL is POST for reads too, so the method says
// nothing).
func (s *Shop) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// Performed counts how often an action ("fulfil", "inventory") really changed
// something on the shop.
func (s *Shop) Performed(kind, id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acts[kind+":"+id]
}

// Tracking is the (company, number, url) of the order's fulfilment.
func (s *Shop) Tracking(orderID string) (company, number, link string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tracking[orderID]
	return t[0], t[1], t[2]
}

// Products returns the products' ids in order.
func (s *Shop) ProductIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.products {
		out = append(out, p.ID)
	}
	return out
}

// VariantIDs returns the variant ids of product i (1-based).
func (s *Shop) VariantIDs(i int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, v := range s.products[i-1].Variants {
		out = append(out, v.ID)
	}
	return out
}

// Locations returns the location ids.
func (s *Shop) Locations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.locations...)
}

// Price is a variant's price as the shop shows it ("35.00").
func (s *Shop) Price(variantID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, _ := s.findVariant(variantID); v != nil {
		return v.Price
	}
	return ""
}

// Status is a product's status (ACTIVE, DRAFT, ARCHIVED).
func (s *Shop) Status(productID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.products {
		if p.ID == productID {
			return p.Status
		}
	}
	return ""
}

// ProductCount is the number of products the shop holds (never decreases).
func (s *Shop) ProductCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.products)
}

// Stock is the available quantity at a variant's location.
func (s *Shop) Stock(variantID, loc string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.levels[variantID+"@"+loc]; l != nil {
		return l.Qty
	}
	return -1
}

// HasLevel reports whether the variant is stocked at the location.
func (s *Shop) HasLevel(variantID, loc string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.levels[variantID+"@"+loc] != nil
}

// DeactivateLevel removes a variant's stock level at a location, so the
// adapter has to activate it.
func (s *Shop) DeactivateLevel(variantID, loc string) {
	s.mu.Lock()
	delete(s.levels, variantID+"@"+loc)
	s.mu.Unlock()
}

// SetStock changes a quantity as if the merchant did it in Shopify's admin.
func (s *Shop) SetStock(variantID, loc string, n int64) {
	s.mu.Lock()
	if l := s.levels[variantID+"@"+loc]; l != nil {
		l.Qty = n
		l.UpdatedAt = s.bump()
	}
	s.mu.Unlock()
}

// SetPrice changes a price as if the merchant did it in Shopify's admin.
func (s *Shop) SetPrice(variantID, price string) {
	s.mu.Lock()
	if v, p := s.findVariant(variantID); v != nil {
		v.Price = price
		p.UpdatedAt = s.bump()
	}
	s.mu.Unlock()
}

// SetTracked turns inventory tracking on or off for a variant.
func (s *Shop) SetTracked(variantID string, tracked bool) {
	s.mu.Lock()
	if v, _ := s.findVariant(variantID); v != nil {
		v.Tracked = tracked
	}
	s.mu.Unlock()
}

// Orders returns a copy of the orders.
func (s *Shop) Orders() []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Order(nil), s.orders...)
}

// MutateOrder lets a test change an order as the merchant would; UpdatedAt is
// advanced afterwards.
func (s *Shop) MutateOrder(id string, f func(*Order)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.orders {
		if s.orders[i].ID == id {
			f(&s.orders[i])
			s.orders[i].UpdatedAt = s.bump()
		}
	}
}

// AddOrders appends n extra single-line orders (for pagination tests).
func (s *Shop) AddOrders(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.products[0].Variants[0]
	for i := 0; i < n; i++ {
		k := 100 + len(s.orders)
		ts := Base.Add(time.Duration(10+len(s.orders)) * time.Hour).Format(time.RFC3339)
		o := Order{ID: gid("Order", 5000+k), Name: fmt.Sprintf("#%d", 1000+k), CreatedAt: ts, UpdatedAt: ts, TaxesIncluded: !s.opt.TaxesExcluded,
			Fin: "PAID", Ful: "UNFULFILLED", Ship: 100, ShipRate: 0.2,
			Lines: []orderLine{{ID: gid("LineItem", 6000+k), Name: "Invented", SKU: v.SKU, VariantID: v.ID, Qty: 1, CurQty: 1, Unit: parseMinor(v.Price), Rate: 0.2}},
			FOs:   []fulfilmentOrder{{ID: gid("FulfillmentOrder", 8000+k), Status: "OPEN", Loc: gid("Location", 1)}}}
		o.Total = totalOf(o)
		s.orders = append(s.orders, o)
	}
}

// bump returns a fresh, strictly increasing timestamp after the seed data.
func (s *Shop) bump() string {
	n := len(s.reqs) + s.writes
	return Base.Add(48*time.Hour + time.Duration(n)*time.Second).Format(time.RFC3339)
}

func (s *Shop) findVariant(id string) (*variant, *product) {
	for pi := range s.products {
		for vi := range s.products[pi].Variants {
			if s.products[pi].Variants[vi].ID == id {
				return &s.products[pi].Variants[vi], &s.products[pi]
			}
		}
	}
	return nil, nil
}

// Fingerprint is a digest of everything the shop holds. It changes when the
// adapter writes anything (conformance check 1).
func (s *Shop) Fingerprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lv := map[string]level{}
	for k, v := range s.levels {
		lv[k] = *v
	}
	b, _ := json.Marshal(struct {
		P []product
		O []Order
		L map[string]level
		T map[string][3]string
	}{s.products, s.orders, lv, s.tracking})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- webhooks and OAuth ---------------------------------------------------

// SignWebhook returns the signature header value for a body (base64 HMAC).
func (s *Shop) SignWebhook(body []byte) string {
	m := hmac.New(sha256.New, []byte(s.opt.Secret))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// WebhookBody builds an invented webhook payload in Shopify's shape: a REST
// style order with the buyer's details in it, as the real topic carries.
func (s *Shop) WebhookBody(orderGID string) []byte {
	n := strings.TrimPrefix(orderGID, "gid://shopify/Order/")
	b, _ := json.Marshal(map[string]any{
		"id": json.Number(n), "admin_graphql_api_id": orderGID, "name": "#1001", "email": "invented@example.invalid",
		"phone": "+440000000000", "customer": map[string]any{"first_name": "Invented", "last_name": "Person"},
		"shipping_address": map[string]any{"address1": "1 Invented Street", "name": "Invented Person"},
	})
	return b
}

// WebhookHeaders are the headers Shopify sends with a webhook.
func (s *Shop) WebhookHeaders(id, topic string, body []byte) map[string]string {
	return map[string]string{
		"X-Shopify-Topic": topic, "X-Shopify-Shop-Domain": s.opt.Account, "X-Shopify-Webhook-Id": id,
		"X-Shopify-API-Version": "2026-10", WebhookHeader: s.SignWebhook(body), "Content-Type": "application/json",
	}
}

// CanonicalSortedQuery is the contract's canonical string for
// auth.callback_verify (mode sorted-query-without-param).
func CanonicalSortedQuery(q url.Values, skip string) string {
	enc := strings.NewReplacer("%", "%25", "&", "%26", "=", "%3D")
	keys := make([]string, 0, len(q))
	for k := range q {
		if k != skip {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		pairs = append(pairs, enc.Replace(k)+"="+enc.Replace(q.Get(k)))
	}
	return strings.Join(pairs, "&")
}

// SignQuery signs an install callback query (hex HMAC-SHA256 of the
// canonical string, Shopify's scheme).
func (s *Shop) SignQuery(q url.Values) string {
	m := hmac.New(sha256.New, []byte(s.opt.Secret))
	m.Write([]byte(CanonicalSortedQuery(q, "hmac")))
	return hex.EncodeToString(m.Sum(nil))
}

// --- serving --------------------------------------------------------------

func (s *Shop) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/admin/oauth/authorize":
		s.authorize(w, r)
	case "/admin/oauth/access_token":
		s.token(w, r)
	case APIPath:
		s.graphql(w, r)
	default:
		writeJSON(w, 404, map[string]string{"errors": "Not Found"})
	}
}

func (s *Shop) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") != s.opt.ClientID || err != nil || redirect.Scheme != "https" || q.Get("state") == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	code := fmt.Sprintf("code-%d", len(s.codes)+1)
	s.codes[code] = true
	s.mu.Unlock()
	cb := url.Values{"code": {code}, "shop": {s.opt.Account}, "state": {q.Get("state")}, "scope": {q.Get("scope")},
		"timestamp": {strconv.FormatInt(time.Now().Unix(), 10)}}
	cb.Set("hmac", s.SignQuery(cb))
	redirect.RawQuery = cb.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// token exchanges a single-use code for the offline access token (no expiry,
// no refresh token: Shopify's offline tokens do not expire).
func (s *Shop) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	f := r.PostForm
	if f.Get("client_id") != s.opt.ClientID || f.Get("client_secret") != s.opt.Secret {
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.codes[f.Get("code")] {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	delete(s.codes, f.Get("code"))
	writeJSON(w, 200, map[string]any{"access_token": s.opt.Token, "scope": "read_products,write_products"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

type gqlRequest struct {
	OperationName string                     `json:"operationName"`
	Query         string                     `json:"query"`
	Variables     map[string]json.RawMessage `json:"variables"`
}

var (
	headerRe = regexp.MustCompile(`^\s*(query|mutation)\s+(\w+)\s*(?:\(([^)]*)\))?`)
	declRe   = regexp.MustCompile(`\$(\w+)\s*:\s*([^,)]+)`)
	useRe    = regexp.MustCompile(`\$(\w+)`)
)

// checkVariables is the fake's whole GraphQL validation.
func checkVariables(req gqlRequest) string {
	m := headerRe.FindStringSubmatch(req.Query)
	if m == nil {
		return "not an operation"
	}
	if m[2] != req.OperationName {
		return "operationName does not match the operation"
	}
	declared, required := map[string]bool{}, map[string]bool{}
	for _, d := range declRe.FindAllStringSubmatch(m[3], -1) {
		declared[d[1]] = true
		if strings.HasSuffix(strings.TrimSpace(d[2]), "!") {
			required[d[1]] = true
		}
	}
	used := map[string]bool{}
	for _, u := range useRe.FindAllStringSubmatch(req.Query[len(m[0]):], -1) {
		used[u[1]] = true
	}
	for v := range used {
		if !declared[v] {
			return "variable $" + v + " is used but not declared"
		}
	}
	for v := range declared {
		if !used[v] {
			return "variable $" + v + " is declared but not used"
		}
	}
	for v := range req.Variables {
		if !declared[v] {
			return "variable " + v + " supplied but not declared"
		}
	}
	for v := range required {
		if x, ok := req.Variables[v]; !ok || string(x) == "null" {
			return "variable $" + v + " is required"
		}
	}
	return ""
}

func (s *Shop) graphql(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	var req gqlRequest
	bad := r.Method != http.MethodPost || json.Unmarshal(body, &req) != nil
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.EscapedPath(), Host: r.Host, Header: r.Header.Clone(), Op: req.OperationName, Query: req.Query, Variables: req.Variables})
	s.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(Header)), []byte(s.opt.Token)) != 1 {
		writeJSON(w, 401, map[string]string{"errors": "[API] Invalid API key or access token (unrecognized login or wrong password)"})
		return
	}
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if s.opt.ThrottleEvery > 0 && n%s.opt.ThrottleEvery == 0 {
		w.Header().Set("Retry-After", "0")
		writeJSON(w, 429, map[string]string{"errors": "Throttled"})
		return
	}
	if bad {
		writeJSON(w, 400, map[string]string{"errors": "bad request"})
		return
	}
	if msg := checkVariables(req); msg != "" {
		writeJSON(w, 200, map[string]any{"errors": []map[string]any{{"message": msg, "extensions": map[string]string{"code": "VARIABLE_MISMATCH"}}}})
		return
	}
	if s.opt.GraphQLThrottleEvery > 0 && n%s.opt.GraphQLThrottleEvery == 0 {
		writeJSON(w, 200, map[string]any{
			"errors":     []map[string]any{{"message": "Throttled", "extensions": map[string]string{"code": "THROTTLED"}}},
			"extensions": costExt(0, 0),
		})
		return
	}
	isMut := strings.HasPrefix(strings.TrimSpace(req.Query), "mutation")
	s.mu.Lock()
	if isMut {
		s.writes++
	}
	data, gerr := s.dispatch(req)
	s.mu.Unlock()
	if gerr != "" {
		writeJSON(w, 200, map[string]any{"errors": []map[string]any{{"message": gerr, "extensions": map[string]string{"code": gerr}}}, "extensions": costExt(0, 0)})
		return
	}
	if s.h.huge && req.OperationName == "Products" {
		data = map[string]any{"shop": map[string]any{"currencyCode": s.opt.Currency}, "pad": strings.Repeat("x", 9<<20)}
	}
	writeJSON(w, 200, map[string]any{"data": data, "extensions": costExt(0, 0)})
}

func costExt(requested, actual int) map[string]any {
	return map[string]any{"cost": map[string]any{"requestedQueryCost": requested, "actualQueryCost": actual,
		"throttleStatus": map[string]any{"maximumAvailable": 1000.0, "currentlyAvailable": 900.0, "restoreRate": 50.0}}}
}

// SetGraphQLThrottle changes Options.GraphQLThrottleEvery while the shop runs
// (0 = never). Call it between asks, not during one.
func (s *Shop) SetGraphQLThrottle(every int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opt.GraphQLThrottleEvery = every
}
