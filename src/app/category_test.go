//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
)

func importedListings(e *env) map[string]channel.Listing {
	e.t.Helper()
	e.importAll()
	out := map[string]channel.Listing{}
	for _, raw := range e.submitted(channel.KindListing) {
		var l channel.Listing
		if err := json.Unmarshal(raw, &l); err != nil {
			e.t.Fatal(err)
		}
		out[l.ExternalID] = l
	}
	return out
}

// ut-docs#4944: the till item has ONE category. Import takes the merchant's
// product type, else the standard category's name, else the first collection,
// else nothing. The other collections stay in Shopify.
func TestImportTakesOneCategoryByTheAcceptedRule(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	ids := e.shop.ProductIDs()
	e.shop.SetCategory(ids[0], "Shirts", "Clothing Tops", "Summer", "Sale")
	e.shop.SetCategory(ids[1], "", "Clothing Tops", "Summer")
	e.shop.SetCategory(ids[2], "  ", " ", "Summer", "Sale")
	e.shop.SetCategory(ids[3], "", "")
	got := importedListings(e)
	for i, want := range [][]string{{"Shirts"}, {"Clothing Tops"}, {"Summer"}, nil} {
		if g := got[ids[i]].CategoryPath; fmt.Sprint(g) != fmt.Sprint(want) {
			t.Errorf("product %d category %v, want %v", i+1, g, want)
		}
	}
	if g := got[ids[4]].CategoryPath; len(g) != 0 {
		t.Errorf("an uncategorised product got %v", g)
	}
}

func TestImportCarriesCompareAtAndNothingWhenUnset(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	vs := e.shop.VariantIDs(1)
	e.shop.SetCompareAt(vs[0], "45.00")
	got := importedListings(e)[e.shop.ProductIDs()[0]]
	if got.Variants[0].CompareAtMinor != 4500 || got.Variants[1].CompareAtMinor != 0 {
		t.Fatalf("compare-at %d %d", got.Variants[0].CompareAtMinor, got.Variants[1].CompareAtMinor)
	}
}

func upsertWithCategory(id, variant, path string) channel.OutboxOp {
	body := fmt.Sprintf(`{"cloud_id":"c","external_id":%q,"name":"n","status":"active","category_path":%s,`+
		`"variants":[{"external_id":%q,"cloud_id":"c","price_minor":610,"currency":"GBP","tax_included":true}]}`, variant, path, variant)
	return channel.OutboxOp{ID: id, Kind: channel.OpListingUpsert, ExternalID: variant, Payload: json.RawMessage(body)}
}

func countOp(e *env, op string) (n int) {
	for _, r := range e.shop.Requests() {
		if r.Op == op {
			n++
		}
	}
	return
}

// Push fills a product that has NO category at all with the item's category,
// as productType only: the leaf of the path, collections untouched.
func TestUpsertFillsAnUncategorisedProductWithTheLeafAsProductTypeOnly(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := e.shop.ProductIDs()[0], e.shop.VariantIDs(1)[0]
	if a := e.outbox(upsertWithCategory("op-1", v, `["Clothing","Shirts"]`))["op-1"]; a.Error != nil || a.ExternalID != v {
		t.Fatalf("ack %+v", a)
	}
	if got := e.shop.ProductType(p); got != "Shirts" {
		t.Fatalf("productType %q", got)
	}
	// The write carries its own idempotency key, apart from the price write.
	keys := map[string]bool{}
	for _, s := range e.em.Seen() {
		if k := s.Header["Idempotency-Key"]; k != "" {
			keys[k] = true
		}
	}
	if !keys["op-1:type"] {
		t.Errorf("idempotency keys %v", keys)
	}
	// Same category again: nothing is written.
	w := e.shop.Writes()
	if a := e.outbox(upsertWithCategory("op-2", v, `["Clothing","Shirts"]`))["op-2"]; a.Error != nil || e.shop.Writes() != w {
		t.Fatalf("resend %+v writes %d -> %d", a, w, e.shop.Writes())
	}
}

// A product the merchant already categorised (a type, a standard category or a
// collection) is NEVER written: the cloud sends category_path on every price
// edit, so overwriting would undo the merchant's own change.
func TestUpsertNeverOverwritesACategoryTheShopAlreadyHas(t *testing.T) {
	for name, c := range map[string][3]string{"type": {"Old type", "", ""}, "standard": {"", "Tops", ""}, "collection": {"", "", "Summer"}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fakeshop.Options{})
			p, v := e.shop.ProductIDs()[0], e.shop.VariantIDs(1)[0]
			var cols []string
			if c[2] != "" {
				cols = []string{c[2], "Sale"}
			}
			e.shop.SetCategory(p, c[0], c[1], cols...)
			if a := e.outbox(upsertWithCategory("op-1", v, `["Shirts"]`))["op-1"]; a.Error != nil || a.ExternalID != v {
				t.Fatalf("%+v", a)
			}
			if n := countOp(e, "ProductType"); n != 0 || e.shop.ProductType(p) != c[0] || len(e.shop.Collections(p)) != len(cols) {
				t.Fatalf("%d writes, type %q", n, e.shop.ProductType(p))
			}
			if e.shop.Price(v) != "6.10" {
				t.Fatalf("price %q: the price push must still happen", e.shop.Price(v))
			}
		})
	}
}

func TestUpsertWithoutACategoryWritesNoProductType(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := e.shop.ProductIDs()[0], e.shop.VariantIDs(1)[0]
	for i, path := range []string{`[]`, `null`, `[" "]`} {
		if a := e.outbox(upsertWithCategory(fmt.Sprintf("op-%d", i), v, path))[fmt.Sprintf("op-%d", i)]; a.Error != nil {
			t.Fatalf("%+v", a)
		}
	}
	if n := countOp(e, "ProductType"); n != 0 || e.shop.ProductType(p) != "" {
		t.Fatalf("%d writes, type %q", n, e.shop.ProductType(p))
	}
}

// A category change alone (the price is already right) still reaches Shopify.
func TestCategoryOnlyChangeIsPushedWhenThePriceIsAlreadyThere(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := e.shop.ProductIDs()[0], e.shop.VariantIDs(1)[0]
	e.shop.SetPrice(v, "6.10")
	if a := e.outbox(upsertWithCategory("op-1", v, `["Shirts"]`))["op-1"]; a.Error != nil || e.shop.ProductType(p) != "Shirts" {
		t.Fatalf("%+v %q", a, e.shop.ProductType(p))
	}
	if n := countOp(e, "VariantPrices"); n != 0 {
		t.Fatalf("price written %d times", n)
	}
}

// The cloud refuses a category name over 60 characters (and fails the whole
// import row for it): import and push both cut to 60 runes, on a rune boundary,
// trimmed after the cut.
func TestCategoryTextIsCutToTheCloudsLimit(t *testing.T) {
	long := strings.Repeat("é", 59) + " " + strings.Repeat("x", 40)
	e := newEnv(t, fakeshop.Options{})
	p, v := e.shop.ProductIDs()[0], e.shop.VariantIDs(1)[0]
	e.outbox(upsertWithCategory("op-1", v, fmt.Sprintf("[%q]", long)))
	if got := e.shop.ProductType(p); got != strings.Repeat("é", 59) {
		t.Fatalf("pushed type %q (%d runes)", got, len([]rune(got)))
	}
	e2 := newEnv(t, fakeshop.Options{})
	p2 := e2.shop.ProductIDs()[0]
	e2.shop.SetCategory(p2, long, "")
	if got := importedListings(e2)[p2].CategoryPath; len(got) != 1 || got[0] != strings.Repeat("é", 59) {
		t.Fatalf("imported %q", got)
	}
}

// A refused category must not undo the price that was already written and
// acked: the op succeeds, the refusal is logged without text.
func TestARefusedProductTypeDoesNotDeadLetterTheWrittenPrice(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	e.shop.RefuseProductType(true)
	if a := e.outbox(upsertWithCategory("op-1", v, `["Shirts"]`))["op-1"]; a.Error != nil || a.ExternalID != v {
		t.Fatalf("%+v", a)
	}
	if e.shop.Price(v) != "6.10" {
		t.Fatalf("price %q", e.shop.Price(v))
	}
	if n, _ := countLogs(e, "listing.category_rejected"); n != 1 {
		t.Fatalf("category_rejected logged %d times", n)
	}
}
