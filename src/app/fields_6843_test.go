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

// ut-docs#6843 (ADR-0202 amendment 2026-11-23, contract 8.2a): description, sku
// and the first barcode go to Shopify when, and only when, they are in changed.

// fieldsOp is the cloud's upsert for product 3's variant (price 8.10 in line).
// extra is raw JSON for the variant's sku/barcodes and the top-level description.
func fieldsOp(id, variant, desc, sku string, barcodes []string, changed string) channel.OutboxOp {
	if changed != "" {
		changed = `,"changed":` + changed
	}
	bc, _ := json.Marshal(barcodes)
	body := fmt.Sprintf(`{"cloud_id":"c","external_id":%q,"name":"Invented item 3","description":%s,"status":"active"%s,`+
		`"variants":[{"external_id":%q,"cloud_id":"c","sku":%s,"barcodes":%s,"price_minor":810,"currency":"GBP","tax_included":true}]}`,
		variant, jsonString(desc), changed, variant, jsonString(sku), bc)
	return channel.OutboxOp{ID: id, Kind: channel.OpListingUpsert, ExternalID: variant, Payload: json.RawMessage(body)}
}

func ch(field, was, at string) string {
	return fmt.Sprintf(`%q:{"was":%s,"at":%q}`, field, jsonString(was), at)
}

func TestSKUBarcodeAndDescriptionAreWrittenTogetherFromChanged(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "Old text")
	oldSKU, oldBar := e.shop.SKU(v), e.shop.Barcode(v)
	changed := "{" + ch("description", "Old text", earlier) + "," + ch("sku", oldSKU, earlier) + "," + ch("barcodes", oldBar, earlier) + "}"
	op := fieldsOp("f1", v, "Line one\nLine two", " NEW-SKU ", []string{"5999999999991", "5888888888881"}, changed)
	if a := e.outbox(op)["f1"]; a.Error != nil || a.ExternalID != v {
		t.Fatalf("ack %+v", a)
	}
	if got := e.shop.SKU(v); got != "NEW-SKU" {
		t.Errorf("sku %q", got)
	}
	if got := e.shop.Barcode(v); got != "5999999999991" {
		t.Errorf("barcode %q: only the FIRST barcode is pushed", got)
	}
	if got := e.shop.DescriptionHTML(p); got != "<p>Line one</p><p>Line two</p>" {
		t.Errorf("descriptionHtml %q", got)
	}
	if countOp(e, "VariantPrices") != 1 || countOp(e, "ProductType") != 1 {
		t.Errorf("%d variant updates, %d product updates: want one each", countOp(e, "VariantPrices"), countOp(e, "ProductType"))
	}
	if e.shop.Price(v) != "8.10" || e.shop.Performed("price", v) != 0 {
		t.Errorf("price touched: it is not in changed")
	}
	// A duplicate delivery changes nothing further.
	w := e.shop.Writes()
	op.ID = "f2"
	if a := e.outbox(op)["f2"]; a.Error != nil || e.shop.Writes() != w {
		t.Fatalf("resend %+v writes %d -> %d", a, w, e.shop.Writes())
	}
}

func TestEmptyChangedAcksWithNoRequestEvenWhenFieldsArePresent(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	n := len(e.shop.Requests())
	a := e.outbox(fieldsOp("f1", v, "x", "SKU-X", []string{"1234"}, "{}"))["f1"]
	if a.Error != nil || a.ExternalID != v || len(e.shop.Requests()) != n {
		t.Fatalf("ack %+v, %d requests", a, len(e.shop.Requests())-n)
	}
}

func TestOnlyTheFieldsInChangedAreWritten(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	oldSKU, oldBar, oldDesc := e.shop.SKU(v), e.shop.Barcode(v), e.shop.Description(p)
	a := e.outbox(fieldsOp("f1", v, "new text", "NEW-SKU", []string{"999"}, "{"+ch("sku", oldSKU, earlier)+"}"))["f1"]
	if a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.SKU(v) != "NEW-SKU" || e.shop.Barcode(v) != oldBar || e.shop.Description(p) != oldDesc {
		t.Errorf("sku %q barcode %q description %q", e.shop.SKU(v), e.shop.Barcode(v), e.shop.Description(p))
	}
	if countOp(e, "ProductType") != 0 {
		t.Errorf("a product update for a sku-only change")
	}
}

func TestAnOlderCloudWithoutChangedNeverWritesTheThreeFields(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	oldSKU, oldBar := e.shop.SKU(v), e.shop.Barcode(v)
	e.shop.SetPrice(v, "1.00")
	if a := e.outbox(fieldsOp("f1", v, "new text", "NEW-SKU", []string{"999"}, ""))["f1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.SKU(v) != oldSKU || e.shop.Barcode(v) != oldBar || e.shop.DescriptionHTML(p) != "" || e.shop.Price(v) != "8.10" {
		t.Errorf("sku %q barcode %q html %q price %s", e.shop.SKU(v), e.shop.Barcode(v), e.shop.DescriptionHTML(p), e.shop.Price(v))
	}
}

func TestValuesShopifyAlreadyHoldsAreNotWrittenAgain(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "<p>Same   text</p>") // normalises equal; merchant formatting survives
	changed := "{" + ch("description", "Older", earlier) + "," + ch("sku", "OLD", earlier) + "," + ch("barcodes", "OLD", earlier) + "}"
	op := fieldsOp("f1", v, "Same text", e.shop.SKU(v), []string{e.shop.Barcode(v)}, changed)
	w := e.shop.Writes()
	if a := e.outbox(op)["f1"]; a.Error != nil || a.ExternalID != v {
		t.Fatalf("%+v", a)
	}
	if e.shop.Writes() != w || e.shop.DescriptionHTML(p) != "<p>Same   text</p>" {
		t.Errorf("%d writes, html %q", e.shop.Writes()-w, e.shop.DescriptionHTML(p))
	}
}

func TestShopifyEditedFieldsLaterSoTheyAreNotOverwritten(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "Merchant wrote this")
	e.shop.SetSKU(v, "MERCHANT-SKU")
	e.shop.SetBarcode(v, "7777777777777")
	changed := "{" + ch("description", "Old text", earlier) + "," + ch("sku", "OLD", earlier) + "," + ch("barcodes", "OLD", earlier) + "}"
	w := e.shop.Writes()
	if a := e.outbox(fieldsOp("f1", v, "UT text", "UT-SKU", []string{"1111111111111"}, changed))["f1"]; a.Error != nil || a.ExternalID != v {
		t.Fatalf("%+v", a)
	}
	if e.shop.Writes() != w || e.shop.Description(p) != "Merchant wrote this" || e.shop.SKU(v) != "MERCHANT-SKU" || e.shop.Barcode(v) != "7777777777777" {
		t.Errorf("%d writes, description %q sku %q barcode %q", e.shop.Writes()-w, e.shop.Description(p), e.shop.SKU(v), e.shop.Barcode(v))
	}
}

func TestOurChangeIsNewerSoItOverwritesAnOlderShopifyEdit(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "Merchant wrote this")
	e.shop.SetSKU(v, "MERCHANT-SKU")
	changed := "{" + ch("description", "Old text", later) + "," + ch("sku", "OLD", later) + "}"
	if a := e.outbox(fieldsOp("f1", v, "UT text", "UT-SKU", nil, changed))["f1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Description(p) != "UT text" || e.shop.SKU(v) != "UT-SKU" {
		t.Errorf("description %q sku %q", e.shop.Description(p), e.shop.SKU(v))
	}
}

func TestDescriptionIsEscapedAndNeverCarriesItemMarkup(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "")
	text := "Fish & chips <script>alert(1)</script>\r\n\r\n  <b>bold</b> 1 > 0  "
	if a := e.outbox(fieldsOp("f1", v, text, "S", nil, "{"+ch("description", "", earlier)+"}"))["f1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	got := e.shop.DescriptionHTML(p)
	want := "<p>Fish &amp; chips &lt;script&gt;alert(1)&lt;/script&gt;</p><p>&lt;b&gt;bold&lt;/b&gt; 1 &gt; 0</p>"
	if got != want || strings.Contains(got, "<script") || strings.Contains(got, "<b>") {
		t.Fatalf("descriptionHtml %q, want %q", got, want)
	}
}

func TestInvalidFieldValuesAreRefusedBeforeAnyWrite(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	long := strings.Repeat("x", 256)
	for name, op := range map[string]channel.OutboxOp{
		"sku":  fieldsOp("g1", v, "d", long, nil, "{"+ch("sku", "", earlier)+"}"),
		"bar":  fieldsOp("g2", v, "d", "S", []string{long}, "{"+ch("barcodes", "", earlier)+"}"),
		"desc": fieldsOp("g3", v, strings.Repeat("y", 5001), "S", nil, "{"+ch("description", "", earlier)+"}"),
	} {
		w := e.shop.Writes()
		a := e.outbox(op)[op.ID]
		if a.Error == nil || a.Error.Code != "invalid_payload" || a.Error.Retryable || e.shop.Writes() != w {
			t.Errorf("%s: ack %+v, %d writes", name, a, e.shop.Writes()-w)
		}
	}
}

func TestAnEmptyWantedValueNeverClearsTheChannel(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "Keep me")
	oldSKU, oldBar := e.shop.SKU(v), e.shop.Barcode(v)
	changed := "{" + ch("description", "Keep me", earlier) + "," + ch("sku", oldSKU, earlier) + "," + ch("barcodes", oldBar, earlier) + "}"
	if a := e.outbox(fieldsOp("f1", v, "", "", nil, changed))["f1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Description(p) != "Keep me" || e.shop.SKU(v) != oldSKU || e.shop.Barcode(v) != oldBar {
		t.Errorf("cleared: %q %q %q", e.shop.Description(p), e.shop.SKU(v), e.shop.Barcode(v))
	}
}

// The op as ut-docs 8.2a documents it (hand-built fixture, see its header),
// re-bound to a real fake-shop variant.
func TestContractShapedOpDrivesTheAdapterOnceAndSettles(t *testing.T) {
	op := loadOp(t, "cloud_fields_op.json")
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "Old text")
	e.shop.SetSKU(v, "DEMO-OLD")
	e.shop.SetBarcode(v, "5000000000010")
	op.Payload = []byte(strings.ReplaceAll(string(op.Payload), `"v-0001-0"`, `"`+v+`"`))
	op.ExternalID = v
	var after int
	for cycle := 1; cycle <= 3; cycle++ {
		if a := e.outbox(op)[op.ID]; a.Error != nil || a.ExternalID != v {
			t.Fatalf("cycle %d: %+v", cycle, a)
		}
		if cycle == 1 {
			after = e.shop.Writes()
		} else if e.shop.Writes() != after {
			t.Fatalf("cycle %d: further writes (ping-pong)", cycle)
		}
	}
	if e.shop.SKU(v) != "DEMO-0001-0" || e.shop.Barcode(v) != "5999999999991" ||
		e.shop.DescriptionHTML(p) != "<p>First line</p><p>Second &amp; &lt;b&gt;line&lt;/b&gt;</p>" {
		t.Errorf("sku %q barcode %q html %q", e.shop.SKU(v), e.shop.Barcode(v), e.shop.DescriptionHTML(p))
	}
	if n, _ := countLogs(e, "listing.fields_ignored"); n != 0 {
		t.Errorf("pushed fields counted as ignored")
	}
}

func TestImportReadsSKUBarcodeAndDescriptionAsPlainText(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	ids := e.shop.ProductIDs()
	e.shop.SetDescription(ids[2], "  Line one \n\n <b>Line</b>&amp; two  ")
	vs := e.shop.VariantIDs(3)
	e.shop.SetSKU(vs[0], "  SKU-3 ")
	e.shop.SetBarcode(vs[0], " 4006381333931 ")
	l := importedListings(e)[ids[2]]
	if l.Description != "Line one Line & two" {
		t.Errorf("description %q", l.Description)
	}
	if l.Variants[0].SKU != "SKU-3" || fmt.Sprint(l.Variants[0].Barcodes) != "[4006381333931]" {
		t.Errorf("sku %q barcodes %v", l.Variants[0].SKU, l.Variants[0].Barcodes)
	}
}
