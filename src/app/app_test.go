//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/conformance"
	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// All data is invented. Tests run natively on the SDK's FakeHost behind the
// conformance emulator (egress rules, credential injection, 429 and
// THROTTLED retry). Nothing here talks to a real Shopify store.

type env struct {
	t    *testing.T
	shop *fakeshop.Shop
	em   *conformance.Emulator
}

func newEnv(t *testing.T, opt fakeshop.Options) *env {
	t.Helper()
	shop := fakeshop.New(opt)
	em := conformance.NewEmulator(t, app.Handlers(), shop.Handler(), shop.Account(), fakeshop.Header, shop.Token())
	return &env{t: t, shop: shop, em: em}
}

func (e *env) ask(typ string, payload any) []byte {
	e.t.Helper()
	r := e.em.Ask(typ, payload)
	if r.Err != nil {
		e.t.Fatalf("%s: %v", typ, r.Err)
	}
	return r.Answer
}

func (e *env) submitted(kind string) []json.RawMessage {
	var out []json.RawMessage
	for _, b := range e.em.Host.Submitted {
		if b.Kind == kind {
			out = append(out, b.Records...)
		}
	}
	return out
}

// importAll runs import asks until the adapter says it is done.
func (e *env) importAll() (asks int) {
	e.t.Helper()
	for asks = 1; asks <= 30; asks++ {
		var ans struct{ Done bool }
		if err := json.Unmarshal(e.ask(channel.AskImport, struct{}{}), &ans); err != nil {
			e.t.Fatal(err)
		}
		if ans.Done {
			return asks
		}
	}
	e.t.Fatal("import never finished")
	return 0
}

// tickAll runs ticks until one reports nothing left to resume.
func (e *env) tick() int {
	e.t.Helper()
	var ans struct{ Orders int }
	if err := json.Unmarshal(e.ask(channel.AskTick, map[string]string{"event": "reconcile"}), &ans); err != nil {
		e.t.Fatal(err)
	}
	return ans.Orders
}

func TestConnectTestReportsTheShopAndWritesNothing(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Locations: 2})
	before := e.shop.Fingerprint()
	var got channel.ConnectTestResult
	if err := json.Unmarshal(e.ask(channel.AskConnectTest, struct{}{}), &got); err != nil {
		t.Fatal(err)
	}
	if got.AccountName != "Invented Shop A" || got.Products != 50 || got.Orders != 3 || len(got.Locations) != 2 || len(got.Scopes) != 9 {
		t.Fatalf("result %+v", got)
	}
	if got.Scopes[0] != "read_products" {
		t.Errorf("scopes %v", got.Scopes)
	}
	if e.shop.Writes() != 0 || e.shop.Fingerprint() != before || len(e.em.Host.Submitted) != 0 || len(e.em.Host.CursorKeys()) != 0 {
		t.Fatal("connect test changed something")
	}
}

func TestOAuthCallbackReturnsTheVerifiedAccountAndScopes(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	var got channel.OAuthCallbackResult
	in := channel.OAuthCallbackInput{Params: map[string]string{"shop": "shop-a.myshopify.com", "scope": "read_products,write_products,read_orders", "state": "s", "timestamp": "1"}}
	if err := json.Unmarshal(e.ask(channel.AskOAuthCallback, in), &got); err != nil {
		t.Fatal(err)
	}
	if got.Account != "shop-a.myshopify.com" || strings.Join(got.Scopes, ",") != "read_products,write_products,read_orders" {
		t.Fatalf("result %+v", got)
	}
	if e.em.Host.Calls[channel.FnHTTP] != 0 {
		t.Fatal("the callback ask makes no network call")
	}
	for _, bad := range []string{"evil.example", "shop-a.myshopify.com.evil.example", "myshopify.com", ".myshopify.com", "a/b.myshopify.com",
		"Shop-A.myshopify.com", "a@b.myshopify.com", "shop-a.myshopify.com:8443", "-a.myshopify.com", ""} {
		in.Params["shop"] = bad
		if r := e.em.Ask(channel.AskOAuthCallback, in); r.Err == nil {
			t.Errorf("account %q accepted", bad)
		}
	}
}

func TestWebhookIsParsedWithoutTenantContext(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	order := "gid://shopify/Order/5001"
	cases := []struct{ topic, event string }{
		{"orders/create", channel.EventOrderChanged}, {"orders/updated", channel.EventOrderChanged},
		{"app/uninstalled", channel.EventUninstalled}, {"customers/redact", channel.EventCustomerRedact},
		{"shop/redact", channel.EventShopRedact}, {"customers/data_request", channel.EventDataRequest},
		{"something/else", channel.EventOther},
	}
	for _, c := range cases {
		body := e.shop.WebhookBody(order)
		var got channel.WebhookResult
		in := channel.WebhookInput{Headers: e.shop.WebhookHeaders("wh-"+strings.ReplaceAll(c.topic, "/", "-"), c.topic, body), Body: body}
		if err := json.Unmarshal(e.ask(channel.AskWebhook, in), &got); err != nil {
			t.Fatal(err)
		}
		if got.Event != c.event || got.Account != "shop-a.myshopify.com" || got.DedupeID != "wh-"+strings.ReplaceAll(c.topic, "/", "-") {
			t.Errorf("%s: %+v", c.topic, got)
		}
		wantRes := c.event == channel.EventOrderChanged
		if wantRes != (len(got.ResourceIDs) == 1 && got.ResourceIDs[0] == order) || (!wantRes && len(got.ResourceIDs) != 0) {
			t.Errorf("%s: resource ids %v", c.topic, got.ResourceIDs)
		}
		// Not a word of the body's personal data comes back.
		b, _ := json.Marshal(got)
		for _, pii := range []string{"invented@example.invalid", "Invented", "Street", "+44"} {
			if strings.Contains(string(b), pii) {
				t.Errorf("%s: the answer contains %q", c.topic, pii)
			}
		}
	}
	for _, fn := range []string{channel.FnHTTP, channel.FnCursorGet, channel.FnCursorSet, channel.FnSubmit, channel.FnOutboxNext} {
		if e.em.Host.Calls[fn] != 0 {
			t.Errorf("the webhook ask called %s: it has no tenant context", fn)
		}
	}
}

func TestWebhookRefusesWhatIsNotOfThisChannel(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	body := e.shop.WebhookBody("gid://shopify/Order/5001")
	good := e.shop.WebhookHeaders("wh-1", "orders/updated", body)
	with := func(k, v string) map[string]string {
		h := map[string]string{}
		for a, b := range good {
			h[a] = b
		}
		if v == "" {
			delete(h, k)
		} else {
			h[k] = v
		}
		return h
	}
	cases := map[string]channel.WebhookInput{
		"no shop domain":         {Headers: with("X-Shopify-Shop-Domain", ""), Body: body},
		"foreign shop domain":    {Headers: with("X-Shopify-Shop-Domain", "shop-a.myshopify.com.evil.example"), Body: body},
		"no webhook id":          {Headers: with("X-Shopify-Webhook-Id", ""), Body: body},
		"no topic":               {Headers: with("X-Shopify-Topic", ""), Body: body},
		"order topic, no order":  {Headers: good, Body: []byte(`{"id": 1}`)},
		"order topic, bad order": {Headers: good, Body: []byte(`{"admin_graphql_api_id":"gid://shopify/Order/1/../../x"}`)},
		"order topic, not json":  {Headers: good, Body: []byte("not json")},
	}
	for name, in := range cases {
		if r := e.em.Ask(channel.AskWebhook, in); r.Err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Header names are case-insensitive on the wire.
	lower := map[string]string{"x-shopify-topic": "orders/create", "x-shopify-shop-domain": "shop-a.myshopify.com", "x-shopify-webhook-id": "wh-2"}
	var got channel.WebhookResult
	if err := json.Unmarshal(e.ask(channel.AskWebhook, channel.WebhookInput{Headers: lower, Body: body}), &got); err != nil || got.DedupeID != "wh-2" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestImportSubmitsEveryProductWithVariantsAndStock(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.importAll()
	listings := e.submitted(channel.KindListing)
	if len(listings) != 50 {
		t.Fatalf("%d listings", len(listings))
	}
	for _, b := range e.em.Host.Submitted {
		if len(b.Records) > channel.MaxBatchRecords {
			t.Fatalf("batch of %d", len(b.Records))
		}
	}
	byID := map[string]channel.Listing{}
	variants := 0
	for _, raw := range listings {
		var l channel.Listing
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		byID[l.ExternalID] = l
		variants += len(l.Variants)
	}
	p1 := byID[e.shop.ProductIDs()[0]] // product 1: 2 variants, active
	if p1.Title != "Invented item 1" || p1.Status != "active" || len(p1.Variants) != 2 || p1.ExternalVersion != "2026-10-22T09:00:01Z" {
		t.Fatalf("product 1 %+v", p1)
	}
	v := p1.Variants[0]
	if v.ExternalID != e.shop.VariantIDs(1)[0] || v.SKU != "INV-0001-S" || v.PriceMinor != 610 || v.Currency != "GBP" || !v.TaxIncluded || !v.Tracked ||
		v.Options["Size"] != "S" || len(v.Barcodes) != 1 || v.CompareAtMinor != 0 {
		t.Fatalf("variant %+v", v)
	}
	if p3 := byID[e.shop.ProductIDs()[2]]; p3.Status != "draft" || len(p3.Variants) != 1 || len(p3.Variants[0].Options) != 0 {
		t.Fatalf("product 3 (draft, default option dropped) %+v", p3)
	}
	obs := e.submitted(channel.KindStockObserved)
	if len(obs) != variants {
		t.Fatalf("%d stock observations for %d variants", len(obs), variants)
	}
	var o channel.StockObserved
	_ = json.Unmarshal(obs[0], &o)
	if o.Variant != e.shop.VariantIDs(1)[0] || o.Location != e.shop.Locations()[0] || o.Available != 11 || o.ExternalID == "" {
		t.Fatalf("observation %+v", o)
	}
	if e.shop.Writes() != 0 {
		t.Fatal("import wrote to the shop")
	}
}

func TestImportTwiceSubmitsTheSameRecords(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Locations: 2})
	e.importAll()
	first := e.em.Host.Submitted
	var variants int
	for _, raw := range e.submitted(channel.KindListing) {
		var l channel.Listing
		_ = json.Unmarshal(raw, &l)
		variants += len(l.Variants)
	}
	// Two locations: every tracked variant is observed at both.
	if n := len(e.submitted(channel.KindStockObserved)); n != 2*variants {
		t.Fatalf("%d observations for %d variants at 2 locations", n, variants)
	}
	e.em.Host.Submitted = nil
	e.importAll()
	second := e.em.Host.Submitted
	flat := func(bs []channel.SubmittedBatch) string {
		var sb strings.Builder
		for _, b := range bs {
			sb.WriteString(b.Kind)
			for _, r := range b.Records {
				sb.Write(r)
			}
		}
		return sb.String()
	}
	if len(first) == 0 || flat(first) != flat(second) {
		t.Fatal("a second import is not byte-identical to the first: the host would see changes")
	}
}

func TestImportFollowsMoreVariantsAndRefusesOverTheLimits(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 6})
	e.shop.AddVariants(1, 59)                                                                               // product 1: 2+59 = 61 variants, past one page of 50, within 100
	e.shop.AddVariants(2, 120)                                                                              // product 2: past max_variants 100
	e.shop.SetOptions(e.shop.VariantIDs(4)[0], [][2]string{{"A", "1"}, {"B", "2"}, {"C", "3"}, {"D", "4"}}) // product 4: past max_options 3
	e.importAll()
	got := map[string]int{}
	for _, raw := range e.submitted(channel.KindListing) {
		var l channel.Listing
		_ = json.Unmarshal(raw, &l)
		got[l.ExternalID] = len(l.Variants)
	}
	ids := e.shop.ProductIDs()
	if got[ids[0]] != 61 {
		t.Errorf("product 1 has %d variants, want 61 (the second page was not followed)", got[ids[0]])
	}
	if _, ok := got[ids[1]]; ok {
		t.Error("a product over the variant limit was truncated and imported: it must be refused")
	}
	if _, ok := got[ids[3]]; ok {
		t.Error("a product over the option limit was imported")
	}
	if len(got) != 4 { // 6 products, 2 refused
		t.Errorf("%d listings, want 4", len(got))
	}
	var refused int
	for _, l := range e.em.Host.Logs {
		if strings.HasPrefix(l.Code, "import.refused.") {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("%d refusals logged, want 2 (with a reason, not silently)", refused)
	}
}

func TestImportSkipsUntrackedAndKeepsNeverDeleting(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 2})
	e.shop.SetTracked(e.shop.VariantIDs(1)[0], false)
	e.importAll()
	var obs int
	for _, raw := range e.submitted(channel.KindStockObserved) {
		var o channel.StockObserved
		_ = json.Unmarshal(raw, &o)
		if o.Variant == e.shop.VariantIDs(1)[0] {
			t.Error("an untracked variant has no stock to observe")
		}
		obs++
	}
	if obs != 4 {
		t.Errorf("%d observations, want 4 (5 variants, 1 untracked)", obs)
	}
}

func TestImportStopsInsideTheBudgetAndResumesFromTheCursor(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 200})
	var ans struct {
		Done     bool
		Imported int64
	}
	if err := json.Unmarshal(e.ask(channel.AskImport, struct{}{}), &ans); err != nil {
		t.Fatal(err)
	}
	// The cost budget (not the page cap) ends the ask: 700 requested points a
	// page, so ceil(budget/700) pages of 4 products.
	if want := int64(4 * ((app.ImportCostBudget + 699) / 700)); ans.Done || ans.Imported != want {
		t.Fatalf("first ask %+v, want %d imported and not done", ans, want)
	}
	if e.em.HTTPCalls() > conformance.MaxHTTPCallsPerAsk {
		t.Fatalf("%d calls", e.em.HTTPCalls())
	}
	e.importAll()
	seen := map[string]bool{}
	for _, raw := range e.submitted(channel.KindListing) {
		var l channel.Listing
		_ = json.Unmarshal(raw, &l)
		seen[l.ExternalID] = true
	}
	if len(seen) != 200 {
		t.Fatalf("%d distinct products after resuming, want 200", len(seen))
	}
}

func TestImportSurvivesThrottling(t *testing.T) {
	e := newEnv(t, fakeshop.Options{ThrottleEvery: 3, GraphQLThrottleEvery: 4})
	e.importAll()
	if n := len(e.submitted(channel.KindListing)); n != 50 {
		t.Fatalf("%d listings", n)
	}
}

func TestEveryRequestCarriesACostHintUnderShopifysLimit(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.importAll()
	e.tick()
	e.ask(channel.AskConnectTest, struct{}{})
	for _, s := range e.em.Seen() {
		if s.CostHint <= 0 || s.CostHint > 1000 {
			t.Errorf("cost hint %d for %.60s", s.CostHint, s.Body)
		}
	}
}

func TestPrivateIDsAndPIIAreNotPassedToTheLogs(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.importAll()
	e.tick()
	for _, l := range e.em.Host.Logs {
		b, _ := json.Marshal(l)
		for _, pii := range []string{"invented@example.invalid", "Invented Person", "Invented Street", "+440000000000"} {
			if strings.Contains(string(b), pii) {
				t.Errorf("log %s contains %q", l.Code, pii)
			}
		}
	}
}

func TestAShopWithMoreLocationsThanTheImportReadsIsLoggedNotSilentlyCut(t *testing.T) {
	logged := func(e *env) bool {
		for _, l := range e.em.Host.Logs {
			if l.Code == "import.locations_truncated" {
				return true
			}
		}
		return false
	}
	e := newEnv(t, fakeshop.Options{Locations: app.MaxLocations})
	e.importAll()
	if logged(e) {
		t.Error("exactly the cap is not a truncation")
	}
	e = newEnv(t, fakeshop.Options{Locations: app.MaxLocations + 1})
	e.importAll()
	if !logged(e) {
		t.Error("a 21st location was dropped without a reason code in the log")
	}
}

func TestCursorsAreOpaqueButBounded(t *testing.T) {
	for _, ok := range []string{"eyJsYXN0X2lkIjoxLCJsYXN0X3ZhbHVlIjoiMSJ9", strings.Repeat("a", 900), "abc.def~ghi:jkl", "a+b/c=="} {
		if !app.CursorOK(ok) {
			t.Errorf("cursor %.40q refused", ok)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 1001), "a b", "a\"b", "a\nb", "a'b", "a;b", "a{b}"} {
		if app.CursorOK(bad) {
			t.Errorf("cursor %.40q accepted", bad)
		}
	}
}

func TestTheQueriesAskForOneMoreThanTheCapsTheyEnforce(t *testing.T) {
	all := strings.Join(app.AllQueries(), "\n")
	for _, want := range []string{
		fmt.Sprintf("refunds(first: %d)", app.MaxRefunds+1),
		fmt.Sprintf("refundLineItems(first: %d)", app.MaxRefundLines+1),
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no query contains %q: an overflow would go unseen", want)
		}
	}
	if app.RefundsAsked != fmt.Sprint(app.MaxRefunds+1) || app.LinesAsked != fmt.Sprint(app.MaxRefundLines+1) {
		t.Error("asked != cap + 1")
	}
}

// The Shopify names written from memory are defined once, in queries.go, and
// used from there: a correction after the dev-store run is one line.
func TestFromMemoryShopifyNamesLiveInOnePlace(t *testing.T) {
	all := strings.Join(app.AllQueries(), "\n")
	for _, n := range []string{app.FLineAllocations, app.FOrderTax, app.FShippingLines, app.DIdempotent} {
		if !strings.Contains(all, n) {
			t.Errorf("%q is not in the query texts", n)
		}
	}
	for _, f := range []string{"src/app/outbox.go", "src/app/orders.go", "src/app/catalogue.go"} {
		b, err := os.ReadFile("../../" + f)
		if err != nil {
			t.Fatal(err)
		}
		for _, lit := range []string{`"changeFromQuantity"`, `"ignoreCompareQuantity"`, "@idempotent", "totalDiscountSet", "discountedTotalSet"} {
			if strings.Contains(string(b), lit) {
				t.Errorf("%s spells the from-memory name %s itself: use the constant in queries.go", f, lit)
			}
		}
	}
}
