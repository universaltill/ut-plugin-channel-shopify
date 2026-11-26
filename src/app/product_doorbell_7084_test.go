//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// ut-docs#7084: a product webhook is a doorbell. The plugin reads ONLY the
// product id from the body and answers the neutral event product_changed; the
// host then re-reads the product through channel.fetch.ask.

func (e *env) productWebhook(topic, id string, body []byte) (channel.WebhookResult, error) {
	e.t.Helper()
	r := e.em.Ask(channel.AskWebhook, channel.WebhookInput{Headers: e.shop.WebhookHeaders(id, topic, body), Body: body})
	if r.Err != nil {
		return channel.WebhookResult{}, r.Err
	}
	var got channel.WebhookResult
	if err := json.Unmarshal(r.Answer, &got); err != nil {
		e.t.Fatal(err)
	}
	return got, nil
}

func TestProductWebhooksAreProductChangedWithTheIDOnly(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p := e.shop.ProductIDs()[0]
	num := strings.TrimPrefix(p, "gid://shopify/Product/")
	for _, topic := range []string{"products/create", "products/update"} {
		// A product body as Shopify sends it, with content we must never read.
		body := []byte(fmt.Sprintf(`{"id":%s,"admin_graphql_api_id":%q,"title":"Invented title","body_html":"<p>x</p>","variants":[{"price":"1.00"}]}`, num, p))
		got, err := e.productWebhook(topic, "wh-"+strings.ReplaceAll(topic, "/", "-"), body)
		if err != nil {
			t.Fatalf("%s: %v", topic, err)
		}
		if got.Event != channel.EventProductChanged || len(got.ResourceIDs) != 1 || got.ResourceIDs[0] != p || got.Account != "shop-a.myshopify.com" {
			t.Fatalf("%s: %+v", topic, got)
		}
	}
	// A delete body carries the numeric id only: the gid is built from it.
	got, err := e.productWebhook("products/delete", "wh-del", []byte(fmt.Sprintf(`{"id":%s}`, num)))
	if err != nil || got.Event != channel.EventProductChanged || len(got.ResourceIDs) != 1 || got.ResourceIDs[0] != p {
		t.Fatalf("delete: %v %+v", err, got)
	}
}

func TestProductWebhookRefusesABodyThatIsNotAProduct(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	for name, body := range map[string]string{
		"not json":         `nope`,
		"empty":            `{}`,
		"order gid":        `{"admin_graphql_api_id":"gid://shopify/Order/5001"}`,
		"path trick":       `{"admin_graphql_api_id":"gid://shopify/Product/../../admin"}`,
		"string id":        `{"id":"12 or 1"}`,
		"float id":         `{"id":1.5}`,
		"negative id":      `{"id":-4}`,
		"leading zero id":  `{"id":0123}`,
		"too long id":      `{"id":123456789012345678901234}`,
		"null":             `null`,
		"gid and bad id":   `{"admin_graphql_api_id":"gid://shopify/Product/abc"}`,
		"id as an object ": `{"id":{"a":1}}`,
	} {
		if _, err := e.productWebhook("products/update", "wh-bad", []byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProductWebhookReadsNothingButTheIDAndLogsNothing(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p := e.shop.ProductIDs()[0]
	body := []byte(fmt.Sprintf(`{"admin_graphql_api_id":%q,"title":"Secret Title 77","email":"invented@example.invalid"}`, p))
	got, err := e.productWebhook("products/update", "wh-1", body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "Secret Title 77") || strings.Contains(string(b), "invented@example.invalid") {
		t.Fatal("body content reached the answer")
	}
	for _, l := range e.em.Host.Logs {
		if strings.Contains(fmt.Sprint(l), "Secret Title 77") || strings.Contains(fmt.Sprint(l), "products/update") {
			t.Fatalf("log line %+v carries the body or the topic", l)
		}
	}
}

func (e *env) fetchIDs(ids ...string) (map[string]any, error) {
	e.t.Helper()
	r := e.em.Ask(channel.AskFetch, channel.FetchInput{Event: channel.EventProductChanged, ResourceIDs: ids})
	if r.Err != nil {
		return nil, r.Err
	}
	var out map[string]any
	if err := json.Unmarshal(r.Answer, &out); err != nil {
		e.t.Fatal(err)
	}
	return out, nil
}

func (e *env) listings() []channel.Listing {
	e.t.Helper()
	var out []channel.Listing
	for _, raw := range e.submitted(channel.KindListing) {
		var l channel.Listing
		if err := json.Unmarshal(raw, &l); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func TestFetchOfProductsIsOneBatchedReadByIDAndSubmitsListings(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	ids := e.shop.ProductIDs()[:3]
	e.shop.SetTitle(ids[1], "Renamed in the shop")
	before := e.shop.Fingerprint()
	if _, err := e.fetchIDs(ids...); err != nil {
		t.Fatal(err)
	}
	if e.shop.Fingerprint() != before || e.shop.Writes() != 0 {
		t.Fatal("a fetch wrote to the shop")
	}
	reads := 0
	for _, r := range e.shop.Requests() {
		if r.Op == "ProductsByID" {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("3 ids took %d product reads, want one batched call", reads)
	}
	ls := e.listings()
	if len(ls) != 3 {
		t.Fatalf("%d listings", len(ls))
	}
	var seen bool
	for _, l := range ls {
		if l.ExternalID == ids[1] && l.Title == "Renamed in the shop" {
			seen = true
		}
		if len(l.Variants) == 0 || l.Variants[0].PriceMinor == 0 || l.Variants[0].Currency != "GBP" {
			t.Fatalf("listing not mapped like an import row: %+v", l)
		}
	}
	if !seen {
		t.Fatal("the re-read title is not in the listing")
	}
}

// The re-read row is byte-identical to the row a full import submits.
func TestFetchedProductEqualsTheImportedRow(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.importAll()
	var imported []json.RawMessage
	imported = append(imported, e.submitted(channel.KindListing)...)
	e2 := newEnv(t, fakeshop.Options{})
	if _, err := e2.fetchIDs(e2.shop.ProductIDs()[0]); err != nil {
		t.Fatal(err)
	}
	got := e2.submitted(channel.KindListing)
	if len(got) != 1 {
		t.Fatalf("%d rows", len(got))
	}
	var want json.RawMessage
	for _, r := range imported {
		var l channel.Listing
		_ = json.Unmarshal(r, &l)
		if l.ExternalID == e2.shop.ProductIDs()[0] {
			want = r
		}
	}
	if string(got[0]) != string(want) {
		t.Fatalf("fetch row differs from the import row:\n%s\n%s", got[0], want)
	}
}

func TestFetchSkipsAGoneProductAndNotAnID(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	good := e.shop.ProductIDs()[0]
	gone := "gid://shopify/Product/999999"
	if _, err := e.fetchIDs(good, gone, "gid://shopify/Product/../../admin", "nonsense", "gid://shopify/Variant/1"); err != nil {
		t.Fatal(err)
	}
	if ls := e.listings(); len(ls) != 1 || ls[0].ExternalID != good {
		t.Fatalf("listings %+v", ls)
	}
	for _, l := range e.em.Host.Logs {
		if strings.Contains(fmt.Sprint(l.Fields), "999999") {
			t.Fatalf("a product id reached a log line: %+v", l)
		}
	}
	for _, r := range e.shop.Requests() {
		if strings.Contains(string(mustJSON(r.Variables)), "../") {
			t.Fatal("a malformed id reached a request")
		}
	}
}

func TestFetchOfOrdersAndProductsInOneAsk(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	if _, err := e.fetchIDs("gid://shopify/Order/5001", e.shop.ProductIDs()[0]); err != nil {
		t.Fatal(err)
	}
	if len(e.submitted(channel.KindOrder)) != 1 || len(e.submitted(channel.KindListing)) != 1 {
		t.Fatalf("orders %d listings %d", len(e.submitted(channel.KindOrder)), len(e.submitted(channel.KindListing)))
	}
}

func TestFetchOfManyProductsBatchesAndDefersOverBudget(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 60})
	all := e.shop.ProductIDs()
	out, err := e.fetchIDs(all[:25]...)
	if err != nil {
		t.Fatal(err)
	}
	if out["deferred"] == true || len(e.listings()) != 25 {
		t.Fatalf("25 ids: %v, %d listings", out, len(e.listings()))
	}
	e2 := newEnv(t, fakeshop.Options{Products: 60})
	out, err = e2.fetchIDs(e2.shop.ProductIDs()[:60]...)
	if err != nil {
		t.Fatal(err)
	}
	n := len(e2.listings())
	left, _ := out["deferred_ids"].([]any)
	if out["deferred"] != true || n+len(left) != 60 || len(left) == 0 {
		t.Fatalf("60 ids: deferred=%v read=%d deferred_ids=%d", out["deferred"], n, len(left))
	}
	if n > app.MaxProductFetch {
		t.Fatalf("read %d over the cap %d", n, app.MaxProductFetch)
	}
}

func TestFetchOfProductsDoesNotReadStockOrWrite(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	if _, err := e.fetchIDs(e.shop.ProductIDs()[:2]...); err != nil {
		t.Fatal(err)
	}
	if len(e.submitted(channel.KindStockObserved)) != 0 || e.levelRequests() != 0 {
		t.Fatal("a product fetch observed stock")
	}
}
