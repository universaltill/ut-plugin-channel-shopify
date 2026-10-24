//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// These tests pin the security decisions of ADR-0169 section 5 and the
// contract. Each one has a mutation that must make it fail; the review record
// lists what was tried.

var mutationName = regexp.MustCompile(`^mutation (\w+)`)

var allowedMutations = map[string]bool{
	"InventorySet": true, "InventoryActivate": true, "VariantPrices": true, "ProductType": true, "FulfillmentCreate": true,
	// ut-docs#4774: a cancel keeps the order and a refund adds a refund; neither deletes.
	"OrderCancel": true, "RefundCreate": true,
}

// destructiveNames are the only mutations whose text may mention removing,
// cancelling or refunding; every other mutation naming one is a finding.
var destructiveNames = map[string]bool{"OrderCancel": true, "RefundCreate": true}

// exercise drives every ask the adapter has, with good and hostile input.
func exercise(e *env) {
	e.importAll()
	e.tick()
	e.ask(channel.AskConnectTest, struct{}{})
	e.ask(channel.AskContact, channel.ContactInput{OrderExternalID: "gid://shopify/Order/5001"})
	e.ask(channel.AskFetch, channel.FetchInput{ResourceIDs: []string{"gid://shopify/Order/5001", "gid://shopify/Order/5002"}})
	v, loc := e.shop.VariantIDs(1)[0], e.shop.Locations()[0]
	_ = loc
	e.em.Host.Outbox = append(e.em.Host.Outbox,
		stockOp("s1", v, i64(11), `{"available": 2}`),
		fulfilOp("f1", "gid://shopify/Order/5001", `{"external_order_id":"gid://shopify/Order/5001","carrier":"c","tracking_number":"T"}`),
		channel.OutboxOp{ID: "p1", Kind: channel.OpListingUpsert, ExternalID: v, Payload: json.RawMessage(fmt.Sprintf(`{"variants":[{"external_id":%q,"price_minor":900,"currency":"GBP"}]}`, v))},
		channel.OutboxOp{ID: "u1", Kind: channel.OpListingUnpublish, ExternalID: e.shop.ProductIDs()[1]},
		channel.OutboxOp{ID: "d1", Kind: "listing.delete", ExternalID: e.shop.ProductIDs()[1]},
		channel.OutboxOp{ID: "c0", Kind: channel.OpOrderCancel, ExternalID: "gid://shopify/Order/5001", Payload: json.RawMessage(`{"reason":"other"}`)},
		channel.OutboxOp{ID: "c1", Kind: channel.OpOrderCancel, ExternalID: "gid://shopify/Order/5003", Payload: json.RawMessage(`{"reason":"other","restock":true}`)},
		channel.OutboxOp{ID: "r1", Kind: channel.OpRefundCreate, ExternalID: "gid://shopify/Order/5002",
			Payload: json.RawMessage(`{"external_order_id":"gid://shopify/Order/5002","reason":"other","restock":true,"currency":"GBP","max_amount_minor":660,"lines":[{"line_external_id":"gid://shopify/LineItem/6002","quantity":1,"refunded_to":1,"restocked_to":1}]}`)},
	)
	e.ask(channel.AskOutbox, struct{}{})
}

func TestNoOperationDeletesAnything(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	count, orders := e.shop.ProductCount(), len(e.shop.Orders())
	exercise(e)
	if e.shop.ProductCount() != count || len(e.shop.Orders()) != orders {
		t.Fatal("something was deleted")
	}
	for _, s := range e.em.Seen() {
		if strings.EqualFold(s.Method, "DELETE") {
			t.Errorf("DELETE %s", s.URL)
		}
	}
	for _, q := range app.AllQueries() {
		if regexp.MustCompile(`(?i)delete|destroy`).MatchString(q) {
			t.Errorf("an operation text mentions delete or destroy: %.80s", q)
		}
		if m := mutationName.FindStringSubmatch(q); m != nil {
			if !allowedMutations[m[1]] {
				t.Errorf("mutation %s is not on the allow-list", m[1])
			}
			if regexp.MustCompile(`(?i)remove|cancel|refund`).MatchString(q) && !destructiveNames[m[1]] {
				t.Errorf("mutation %s removes, cancels or refunds but is not one of the two allowed (OrderCancel, RefundCreate): %.80s", m[1], q)
			}
		}
	}
	for _, r := range e.shop.Requests() {
		if m := mutationName.FindStringSubmatch(r.Query); m != nil && !allowedMutations[m[1]] {
			t.Errorf("mutation %s is not on the allow-list", m[1])
		}
	}
}

func TestEveryRequestIsTheOneAccountBoundEndpoint(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	exercise(e)
	if len(e.em.Seen()) < 20 {
		t.Fatalf("only %d requests", len(e.em.Seen()))
	}
	for _, s := range e.em.Seen() {
		if s.URL != "https://"+e.shop.Account()+fakeshop.APIPath || s.Method != "POST" || s.Denied {
			t.Errorf("%s %s denied=%v", s.Method, s.URL, s.Denied)
		}
		for k := range s.Header {
			if strings.EqualFold(k, fakeshop.Header) {
				t.Error("the adapter set the credential header")
			}
		}
	}
}

// No operation selects the customer object: it needs read_customers, which the
// Shopify app does not hold, so one query that does makes every order read fail
// ACCESS_DENIED on a real store (ut-docs#4920). The fake enforces the same.
func TestNoQuerySelectsTheCustomerObject(t *testing.T) {
	re := regexp.MustCompile(`\bcustomer\s*[{(]`)
	for _, q := range app.AllQueries() {
		if re.MatchString(q) {
			t.Errorf("a query selects the customer object: %.120s", q)
		}
	}
}

func TestQueriesAreConstantsAndValuesTravelAsVariables(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	exercise(e)
	known := map[string]bool{}
	for _, q := range app.AllQueries() {
		known[q] = true
		if strings.Contains(q, "gid://") || regexp.MustCompile(`\d{4,}`).MatchString(q) || strings.Contains(q, `"`) && !strings.Contains(q, `names: ["available"]`) {
			t.Errorf("a query text carries a literal value: %.100s", q)
		}
	}
	ops := map[string]bool{}
	for _, r := range e.shop.Requests() {
		if !known[r.Query] {
			t.Errorf("a request used query text that is not one of the constants: %.100s", r.Query)
		}
		ops[r.Op] = true
	}
	for _, want := range []string{"ConnectTest", "Products", "Locations", "Levels", "Orders", "Order", "Contact", "StockTarget", "InventorySet", "VariantPrice", "VariantPrices", "FulfilmentOrders", "FulfillmentCreate",
		"OrderCancel", "RefundState", "SuggestedRefund", "RefundCreate"} {
		if !ops[want] {
			t.Errorf("operation %s was never exercised", want)
		}
	}
}

func TestIDsAreValidatedByTypeBeforeAnyRequest(t *testing.T) {
	for _, c := range []struct {
		typ, id string
		ok      bool
	}{
		{"Order", "gid://shopify/Order/5001", true}, {"Order", "gid://shopify/Order/5001/", false}, {"Order", "gid://shopify/Order/", false},
		{"Order", "gid://shopify/Order/12345678901234567890", true}, {"Order", "gid://shopify/Order/123456789012345678901", false},
		{"Order", "gid://shopify/Product/5001", false}, {"Order", "gid://evil/Order/5001", false}, {"Order", "gid://shopify/Order/5001?x=1", false},
		{"Order", "gid://shopify/Order/5001#f", false}, {"Order", " gid://shopify/Order/5001", false}, {"Order", "gid://shopify/Order/50a1", false},
		{"Order", "gid://shopify/Order/5001\n", false}, {"Order", "https://evil.example/gid://shopify/Order/1", false}, {"Order", "", false},
		{"ProductVariant", "gid://shopify/ProductVariant/1", true}, {"ProductVariant", "gid://shopify/Product/1", false},
		{"Location", "gid://shopify/Location/7001", true}, {"Nonsense", "gid://shopify/Nonsense/1", false},
	} {
		if got := app.OKGID(c.typ, c.id); got != c.ok {
			t.Errorf("OKGID(%s, %q) = %v, want %v", c.typ, c.id, got, c.ok)
		}
	}
}

// TestThePluginNamesNoHostAndHoldsNoSecret reads the shipped source: the only
// URL in it is the manifest's own placeholder, and nothing that looks like a
// credential, client id or real shop is in it.
func TestThePluginNamesNoHostAndHoldsNoSecret(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	files2, _ := filepath.Glob("../main.go")
	for _, f := range append(files, files2...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, m := range regexp.MustCompile(`https?://[^\s"'`+"`"+`]*`).FindAllString(src, -1) {
			if m != "https://{account}" && !strings.HasPrefix(m, "https://{account}/") {
				t.Errorf("%s names a URL: %s", f, m)
			}
		}
		for _, bad := range []string{"shpat_", "shpss_", "client_secret", "client_id", "taskrunner", "X-Shopify-Access-Token", "Authorization", "Bearer"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s mentions %q: the plugin never sees or sets a credential", f, bad)
			}
		}
	}
}

func TestAMaliciousShopCannotMakeTheAdapterLeaveItsAccount(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.Hostile("foreign_links")
	exercise(e)
	for _, s := range e.em.Seen() {
		if s.URL != "https://"+e.shop.Account()+fakeshop.APIPath {
			t.Errorf("request to %s", s.URL)
		}
		if strings.Contains(string(s.Body), "../") || strings.Contains(string(s.Body), "x=1") {
			t.Errorf("a hostile id from the shop reached a request: %.120s", s.Body)
		}
	}
	// The hostile products and orders were dropped, not repaired.
	for _, raw := range e.submitted(channel.KindListing) {
		if strings.Contains(string(raw), "..") {
			t.Errorf("hostile id in a listing: %s", raw)
		}
	}
	for _, raw := range e.submitted(channel.KindOrder) {
		if strings.Contains(string(raw), "..") {
			t.Errorf("hostile id in an order: %s", raw)
		}
	}
}

func TestACredentialEchoedByTheShopNeverLeavesTheAdapter(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.Hostile("echo_headers")
	exercise(e)
	all := ""
	for _, b := range e.em.Host.Submitted {
		for _, r := range b.Records {
			all += string(r)
		}
	}
	for k, v := range e.em.Host.Cursors {
		all += k + string(v)
	}
	for _, l := range e.em.Host.Logs {
		b, _ := json.Marshal(l)
		all += string(b)
	}
	for id, a := range e.em.Host.Acks {
		b, _ := json.Marshal(a)
		all += id + string(b)
	}
	if strings.Contains(all, e.shop.Token()) {
		t.Fatal("the token reached adapter output")
	}
}

// The allow-list and the mutations in the code are the same set: a new
// mutation cannot be added without naming it here, and a name here that no code
// sends is a stale permission.
func TestTheMutationAllowListIsExactlyTheMutationsTheCodeHas(t *testing.T) {
	have := map[string]bool{}
	for _, q := range app.AllQueries() {
		if m := mutationName.FindStringSubmatch(q); m != nil {
			have[m[1]] = true
		}
	}
	for n := range have {
		if !allowedMutations[n] {
			t.Errorf("mutation %s is in the code and not on the allow-list", n)
		}
	}
	for n := range allowedMutations {
		if !have[n] {
			t.Errorf("%s is on the allow-list and no query has it", n)
		}
	}
}

// AllQueries is kept by hand, so the allow-list above is exact only over what
// it lists. These two tests read the source, so a mutation (or any operation)
// added to the code and not to AllQueries cannot slip past every other test.
func nonTestSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

func TestEveryQueryDefinitionIsInAllQueries(t *testing.T) {
	n := 0
	for _, src := range nonTestSources(t) {
		n += strings.Count(src, "queryDef{") // any field order: an anonymous or reordered literal must still be counted
	}
	if n != len(app.AllQueries()) {
		t.Fatalf("%d query definitions in the sources, AllQueries lists %d: add the new one to export_test.go so the pinning tests see it", n, len(app.AllQueries()))
	}
}

var mutationInSource = regexp.MustCompile("[`\"]\\s*mutation\\b\\s*(\\w*)") // an anonymous `mutation { ... }` yields an empty name, which is never on the allow-list

func TestEveryMutationNamedInTheSourceIsOnTheAllowList(t *testing.T) {
	seen := 0
	for f, src := range nonTestSources(t) {
		for _, m := range mutationInSource.FindAllStringSubmatch(src, -1) {
			seen++
			if !allowedMutations[m[1]] {
				t.Errorf("%s: mutation %s is not on the allow-list", f, m[1])
			}
		}
		// A query flagged as a mutation must be a mutation text, and the reverse.
		if got, want := strings.Count(src, "Mutation: true"), len(mutationInSource.FindAllString(src, -1)); got != want {
			t.Errorf("%s: %d definitions flagged Mutation: true, %d mutation texts", f, got, want)
		}
	}
	if seen != len(allowedMutations) {
		t.Errorf("%d mutations in the sources, %d on the allow-list", seen, len(allowedMutations))
	}
}
