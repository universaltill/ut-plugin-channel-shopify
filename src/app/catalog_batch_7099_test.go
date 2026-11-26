//go:build !wasip1

package app_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

func prodJSON(extra string, variants string) string {
	return `{"id":"gid://shopify/Product/1","title":"T","descriptionHtml":"","status":"ACTIVE","updatedAt":"2026-11-26T10:00:00Z",` + extra +
		`"variants":{"pageInfo":{"hasNextPage":false},"nodes":[` + variants + `]}}`
}

func variantJSON(id, taxable, media string) string {
	return `{"id":"gid://shopify/ProductVariant/` + id + `","updatedAt":"2026-11-26T10:00:00Z","sku":"S` + id + `","price":"10.00","taxable":` + taxable +
		`,"selectedOptions":[],"inventoryItem":{"tracked":false}` + media + `}`
}

func mapped(t *testing.T, js string, incl bool) map[string]any {
	t.Helper()
	out, reason := app.MapProductJSON(js, incl)
	if reason != "" {
		t.Fatalf("reason %q", reason)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func firstVariant(m map[string]any) map[string]any {
	return m["variants"].([]any)[0].(map[string]any)
}

// ut-docs#7099: a non-taxable variant keeps the shop's price basis (was folded to false and grossed up x1.2).
func TestMapProduct_NonTaxableVariantKeepsShopBasis(t *testing.T) {
	m := mapped(t, prodJSON("", variantJSON("1", "false", "")), true)
	v := firstVariant(m)
	if v["tax_included"] != true {
		t.Fatalf("tax_included %v, want true (shop basis)", v["tax_included"])
	}
	if tx, ok := v["taxable"]; !ok || tx != false {
		t.Fatalf("taxable %v present=%v, want explicit false", tx, ok)
	}
	if v["price_minor"].(float64) != 1000 {
		t.Fatalf("price %v", v["price_minor"])
	}
}

func TestMapProduct_TaxableFlagCarried(t *testing.T) {
	m := mapped(t, prodJSON("", variantJSON("1", "true", "")), false)
	v := firstVariant(m)
	if v["taxable"] != true || v["tax_included"] == true {
		t.Fatalf("variant %v", v)
	}
}

// ut-docs#7099 WP-C.
func TestMapProduct_IsGiftCardKind(t *testing.T) {
	if k := mapped(t, prodJSON(`"isGiftCard":true,`, variantJSON("1", "false", "")), true)["kind"]; k != "gift_card" {
		t.Fatalf("kind %v", k)
	}
	if k, ok := mapped(t, prodJSON(`"isGiftCard":false,`, variantJSON("1", "true", "")), true)["kind"]; ok {
		t.Fatalf("ordinary product has kind %v", k)
	}
}

const media3 = `"media":{"nodes":[` +
	`{"id":"gid://shopify/MediaImage/11","mediaContentType":"IMAGE","alt":"Front","image":{"url":"https://cdn.shopify.com/a.jpg"}},` +
	`{"id":"gid://shopify/Video/12","mediaContentType":"VIDEO"},` +
	`{"id":"gid://shopify/MediaImage/13","mediaContentType":"IMAGE","alt":"","image":{"url":"https://cdn.shopify.com/b.jpg"}},` +
	`{"id":"gid://shopify/MediaImage/14","mediaContentType":"IMAGE","alt":"Back","image":{"url":"http://insecure/c.jpg"}},` +
	`{"id":"gid://shopify/MediaImage/15","mediaContentType":"IMAGE","alt":"Side","image":{"url":"https://cdn.shopify.com/d.jpg"}}]},`

func TestMapProduct_AllMediaWithAlt(t *testing.T) {
	m := mapped(t, prodJSON(media3, variantJSON("1", "true", "")), true)
	imgs := m["images"].([]any)
	if len(imgs) != 3 { // video and the non-https url are skipped
		t.Fatalf("images %v", imgs)
	}
	want := []struct {
		id, alt string
		pos     float64
	}{{"gid://shopify/MediaImage/11", "Front", 1}, {"gid://shopify/MediaImage/13", "", 2}, {"gid://shopify/MediaImage/15", "Side", 3}}
	for i, w := range want {
		g := imgs[i].(map[string]any)
		alt, _ := g["alt"].(string)
		if g["external_id"] != w.id || alt != w.alt || g["position"].(float64) != w.pos {
			t.Errorf("image %d: %v want %+v", i, g, w)
		}
	}
}

func TestMapProduct_MediaCappedAtTen(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`"media":{"nodes":[`)
	for i := 1; i <= 12; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"gid://shopify/MediaImage/` + string(rune('0'+i%10)) + `0` + string(rune('0'+i/10)) + `","mediaContentType":"IMAGE","image":{"url":"https://cdn.shopify.com/x` + string(rune('a'+i)) + `.jpg"}}`)
	}
	sb.WriteString(`]},`)
	m := mapped(t, prodJSON(sb.String(), variantJSON("1", "true", "")), true)
	if n := len(m["images"].([]any)); n != 10 {
		t.Fatalf("images %d", n)
	}
}

func TestMapProduct_VariantMedia(t *testing.T) {
	vm := `,"media":{"nodes":[{"id":"gid://shopify/MediaImage/13"}]}`
	m := mapped(t, prodJSON(media3, variantJSON("1", "true", vm)), true)
	v := firstVariant(m)
	if v["variant_image_index"] != float64(1) {
		t.Fatalf("variant index %v", v["variant_image_index"])
	}
	if m["variant_image_index"] != float64(1) { // single-variant listing: the contract's listing-level field
		t.Fatalf("listing index %v", m["variant_image_index"])
	}
}

func TestMapProduct_VariantMediaMultiVariantNoListingIndex(t *testing.T) {
	vm := `,"media":{"nodes":[{"id":"gid://shopify/MediaImage/13"}]}`
	m := mapped(t, prodJSON(media3, variantJSON("1", "true", vm)+","+variantJSON("2", "true", "")), true)
	if _, ok := m["variant_image_index"]; ok {
		t.Fatalf("multi-variant listing carries listing-level index")
	}
	if firstVariant(m)["variant_image_index"] != float64(1) {
		t.Fatal("variant 1 index missing")
	}
}

// Fallback to the legacy featuredMedia when an older answer has no media list.
func TestMapProduct_FeaturedFallbackWhenNoMedia(t *testing.T) {
	f := `"featuredMedia":{"id":"gid://shopify/MediaImage/11","image":{"url":"https://cdn.shopify.com/a.jpg"}},`
	m := mapped(t, prodJSON(f, variantJSON("1", "true", "")), true)
	if n := len(m["images"].([]any)); n != 1 {
		t.Fatalf("images %d", n)
	}
}
