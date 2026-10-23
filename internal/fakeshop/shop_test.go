package fakeshop

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func call(t *testing.T, s *Shop, token string, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", APIPath, bytes.NewBufferString(body))
	r.Header.Set(Header, token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestTheShopNeedsTheTokenAndChecksVariables(t *testing.T) {
	s := New(Options{})
	q := `query Locations($first: Int!) { locations(first: $first, sortKey: ID) { nodes { id } } }`
	good := `{"operationName":"Locations","query":` + jsonStr(q) + `,"variables":{"first":5}}`
	if code, _ := call(t, s, "wrong", good); code != 401 {
		t.Errorf("wrong token answered %d", code)
	}
	if code, out := call(t, s, s.Token(), good); code != 200 || out["data"] == nil {
		t.Errorf("good call: %d %v", code, out)
	}
	for name, body := range map[string]string{
		"undeclared variable": `{"operationName":"Locations","query":` + jsonStr(q) + `,"variables":{"first":5,"extra":1}}`,
		"missing required":    `{"operationName":"Locations","query":` + jsonStr(q) + `,"variables":{}}`,
		"interpolated":        `{"operationName":"Locations","query":` + jsonStr(strings.Replace(q, "$first", "5", 1)) + `,"variables":{"first":5}}`,
		"wrong name":          `{"operationName":"Other","query":` + jsonStr(q) + `,"variables":{"first":5}}`,
	} {
		if _, out := call(t, s, s.Token(), body); out["errors"] == nil {
			t.Errorf("%s: no GraphQL error", name)
		}
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestTheShopHasNoDeleteAndOnlyOneEndpoint(t *testing.T) {
	s := New(Options{})
	for _, m := range []string{"DELETE", "PUT", "GET"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(m, APIPath, nil)
		r.Header.Set(Header, s.Token())
		s.Handler().ServeHTTP(w, r)
		if w.Code == 200 {
			t.Errorf("%s answered 200", m)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/admin/api/2026-10/products/1.json", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("a REST path answered %d", w.Code)
	}
	if _, out := call(t, s, s.Token(), `{"operationName":"ProductDelete","query":"mutation ProductDelete($id: ID!) { productDelete(input: {id: $id}) { deletedProductId } }","variables":{"id":"x"}}`); out["errors"] == nil {
		t.Error("the shop answered a delete mutation")
	}
}

func TestSeededOrdersSatisfyTheContractFormula(t *testing.T) {
	for _, excl := range []bool{false, true} {
		for _, o := range New(Options{TaxesExcluded: excl}).Orders() {
			if o.Total != totalOf(o) || o.Total <= 0 {
				t.Errorf("order %s total %d", o.ID, o.Total)
			}
		}
	}
}

func TestPagingWalksEveryProductOnce(t *testing.T) {
	s := New(Options{Products: 11})
	after, seen := "", 0
	for i := 0; i < 20; i++ {
		st, en, ec, more := page(11, 4, after)
		seen += en - st
		if !more {
			break
		}
		after = ec
	}
	if seen != 11 || len(s.products) != 11 {
		t.Fatalf("saw %d", seen)
	}
}

func TestSignaturesVerifyTheShopifyWay(t *testing.T) {
	s := New(Options{})
	body := []byte(`{"id":1}`)
	if s.SignWebhook(body) != s.SignWebhook(body) || s.SignWebhook(body) == s.SignWebhook([]byte(`{"id":2}`)) {
		t.Error("webhook signature is not a function of the body")
	}
	q := url.Values{"shop": {s.Account()}, "timestamp": {"1"}, "code": {"c"}}
	sig := s.SignQuery(q)
	q.Set("hmac", sig)
	if CanonicalSortedQuery(q, "hmac") != "code=c&shop=shop-a.myshopify.com&timestamp=1" || s.SignQuery(q) != sig {
		t.Errorf("canonical %q", CanonicalSortedQuery(q, "hmac"))
	}
}
