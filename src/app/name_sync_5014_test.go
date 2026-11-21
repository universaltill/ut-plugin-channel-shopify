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

// ut-docs#5014 (ADR-0202): the item's name, and its active flag, go to Shopify
// too. The cloud tells the adapter WHICH fields changed with the additive
// payload key changed: {field: {was, at}}; absent = an older cloud (price only).

const (
	earlier = "2026-10-23T00:00:00Z" // before any edit the fake shop makes
	later   = "2026-10-25T00:00:00Z" // after every edit the fake shop makes
)

// singleProduct is a product with exactly one variant, made ACTIVE (the seed's
// single-variant products are drafts): product 3.
func singleProduct(t *testing.T, e *env) (product, variant string) {
	t.Helper()
	product, variant = e.shop.ProductIDs()[2], e.shop.VariantIDs(3)[0]
	e.shop.SetStatus(product, "ACTIVE")
	return
}

// namedOp is the cloud's upsert with a name, a status and (optionally) the
// changed key, at the shop's current price (8.10 for product 3).
func namedOp(id, variant, name, status, changed string, price int64) channel.OutboxOp {
	if changed != "" {
		changed = `,"changed":` + changed
	}
	body := fmt.Sprintf(`{"cloud_id":"c","external_id":%q,"name":%s,"status":%q%s,`+
		`"variants":[{"external_id":%q,"cloud_id":"c","price_minor":%d,"currency":"GBP","tax_included":true}]}`,
		variant, jsonString(name), status, changed, variant, price)
	return channel.OutboxOp{ID: id, Kind: channel.OpListingUpsert, ExternalID: variant, Payload: json.RawMessage(body)}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func nameChanged(was, at string) string {
	return fmt.Sprintf(`{"name":{"was":%s,"at":%q}}`, jsonString(was), at)
}

func TestRenameInTheCloudIsPushedAsTheProductTitleOnly(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	a := e.outbox(namedOp("r1", v, "Renamed in UT", "active", nameChanged("Invented item 3", earlier), 810))["r1"]
	if a.Error != nil || a.ExternalID != v {
		t.Fatalf("ack %+v", a)
	}
	if e.shop.Title(p) != "Renamed in UT" || e.shop.Performed("title", p) != 1 {
		t.Fatalf("title %q written %d times", e.shop.Title(p), e.shop.Performed("title", p))
	}
	if countOp(e, "VariantPrices") != 0 || countOp(e, "ProductType") != 1 || e.shop.Price(v) != "8.10" {
		t.Errorf("price writes %d, product updates %d, price %s: the price is not in changed", countOp(e, "VariantPrices"), countOp(e, "ProductType"), e.shop.Price(v))
	}
	if e.shop.Status(p) != "ACTIVE" {
		t.Errorf("status %s: an ordinary upsert never sets the status", e.shop.Status(p))
	}
}

func TestTitleIsTrimmedAndAnUnchangedTitleIsNotWrittenAgain(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	op := namedOp("r1", v, "  Renamed in UT \n", "active", nameChanged("Invented item 3", earlier), 810)
	if a := e.outbox(op)["r1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Title(p) != "Renamed in UT" {
		t.Fatalf("title %q", e.shop.Title(p))
	}
	n := len(e.shop.Requests())
	op.ID = "r2"
	if a := e.outbox(op)["r2"]; a.Error != nil || a.ExternalID != v {
		t.Fatalf("resend %+v", a)
	}
	if e.shop.Performed("title", p) != 1 || countOp(e, "ProductType") != 1 {
		t.Errorf("title written %d times, %d product updates", e.shop.Performed("title", p), countOp(e, "ProductType"))
	}
	if got := len(e.shop.Requests()) - n; got != 1 { // the read only
		t.Errorf("%d requests for a no-op resend, want 1 (the read)", got)
	}
}

func TestShopifyEditedTheTitleLaterSoThePushIsSkippedAndTheEditSurvives(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetTitle(p, "Merchant's own title") // after the cloud's change at "earlier"
	a := e.outbox(namedOp("g1", v, "Renamed in UT", "active", nameChanged("Invented item 3", earlier), 810))["g1"]
	if a.Error != nil || a.ExternalID != v {
		t.Fatalf("%+v", a)
	}
	if e.shop.Title(p) != "Merchant's own title" || countOp(e, "ProductType") != 0 {
		t.Fatalf("title %q, %d product updates: the later Shopify edit must stand", e.shop.Title(p), countOp(e, "ProductType"))
	}
}

func TestOurChangeIsNewerSoTheTitleIsWrittenOverAnOlderShopifyEdit(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetTitle(p, "Merchant's own title")
	if a := e.outbox(namedOp("g1", v, "Renamed in UT", "active", nameChanged("Invented item 3", later), 810))["g1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Title(p) != "Renamed in UT" {
		t.Fatalf("title %q", e.shop.Title(p))
	}
}

func TestAnUnknownChangeTimeNeverSkipsTheWrite(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetTitle(p, "Merchant's own title")
	for i, ch := range []string{`{"name":{"was":"Invented item 3"}}`, `{"name":{"was":"Invented item 3","at":"not a time"}}`, `{"name":{"at":"` + earlier + `"}}`} {
		id := fmt.Sprintf("u%d", i)
		e.shop.SetTitle(p, "Merchant's own title")
		if a := e.outbox(namedOp(id, v, "Renamed in UT", "active", ch, 810))[id]; a.Error != nil {
			t.Fatalf("%s: %+v", ch, a)
		}
		if e.shop.Title(p) != "Renamed in UT" {
			t.Errorf("%s: title %q, want it written (absent or unreadable was/at is not proof)", ch, e.shop.Title(p))
		}
	}
}

func TestAnEmptyChangedMakesNoShopifyRequestAtAll(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	a := e.outbox(namedOp("e1", v, "Whatever", "active", `{}`, 810))["e1"]
	if a.Error != nil || a.ExternalID != v || len(e.shop.Requests()) != 0 {
		t.Fatalf("%+v, %d requests", a, len(e.shop.Requests()))
	}
}

func TestWithoutChangedAnOlderCloudGetsTheOldBehaviourPriceOnly(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	a := e.outbox(namedOp("o1", v, "Renamed in UT", "inactive", "", 999))["o1"]
	if a.Error != nil || e.shop.Price(v) != "9.99" || e.shop.Title(p) != "Invented item 3" || e.shop.Status(p) != "ACTIVE" || countOp(e, "ProductType") != 0 {
		t.Fatalf("%+v price %s title %q status %s", a, e.shop.Price(v), e.shop.Title(p), e.shop.Status(p))
	}
}

func TestAFieldNotInChangedIsNeverWritten(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	// Only the name is in changed; the payload's price (999) differs from Shopify's but is not ours to push.
	if a := e.outbox(namedOp("n1", v, "Renamed in UT", "active", nameChanged("Invented item 3", earlier), 999))["n1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Price(v) != "8.10" || countOp(e, "VariantPrices") != 0 {
		t.Errorf("price %s: written although it is not in changed", e.shop.Price(v))
	}
}

func TestNameAndPriceTogetherAreBothWritten(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	ch := fmt.Sprintf(`{"name":{"was":"Invented item 3","at":%q},"price":{"was":810,"at":%q}}`, earlier, earlier)
	if a := e.outbox(namedOp("b1", v, "Renamed in UT", "active", ch, 999))["b1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Price(v) != "9.99" || e.shop.Title(p) != "Renamed in UT" {
		t.Fatalf("price %s title %q", e.shop.Price(v), e.shop.Title(p))
	}
}

func TestShopifyPriceEditedLaterIsNotOverwritten(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	e.shop.SetPrice(v, "7.77") // after the cloud's change at "earlier"
	ch := fmt.Sprintf(`{"price":{"was":810,"at":%q}}`, earlier)
	if a := e.outbox(namedOp("p1", v, "Invented item 3", "active", ch, 999))["p1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Price(v) != "7.77" || countOp(e, "VariantPrices") != 0 {
		t.Errorf("price %s: the later Shopify edit must stand", e.shop.Price(v))
	}
	// Shopify at the value we want: no write either.
	e.shop.SetPrice(v, "9.99")
	if a := e.outbox(namedOp("p2", v, "Invented item 3", "active", ch, 999))["p2"]; a.Error != nil || countOp(e, "VariantPrices") != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestAMultiVariantProductIsNeverRenamedAndTheSkipIsCounted(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := e.shop.ProductIDs()[0], e.shop.VariantIDs(1)[0] // two variants
	other := e.shop.VariantIDs(1)[1]
	ops := []channel.OutboxOp{
		namedOp("m1", v, "Item (S)", "active", nameChanged("Invented item 1 (S)", earlier), 610),
		namedOp("m2", other, "Item (M)", "active", nameChanged("Invented item 1 (M)", earlier), 611),
	}
	acks := e.outbox(ops...)
	for _, id := range []string{"m1", "m2"} {
		if acks[id].Error != nil {
			t.Fatalf("%s: %+v", id, acks[id])
		}
	}
	if e.shop.Title(p) != "Invented item 1" || countOp(e, "ProductType") != 0 {
		t.Fatalf("title %q, %d product updates", e.shop.Title(p), countOp(e, "ProductType"))
	}
	n, f := countLogs(e, "listing.name_skipped_multi_variant")
	if n != 1 || f["count"] != float64(2) {
		t.Errorf("logged %d times, fields %v, want once per ask with count 2", n, f)
	}
}

func TestInvalidOrOverLongTitlesAreRefusedBeforeAnyWrite(t *testing.T) {
	for name, title := range map[string]string{
		"empty": "", "blank": " \t ", "over long": strings.Repeat("é", 256),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fakeshop.Options{})
			_, v := singleProduct(t, e)
			a := e.outbox(namedOp("i1", v, title, "active", nameChanged("Invented item 3", earlier), 810))["i1"]
			if a.Error == nil || a.Error.Code != "invalid_payload" || a.Error.Retryable || e.shop.Writes() != 0 {
				t.Fatalf("%+v, %d writes", a, e.shop.Writes())
			}
		})
	}
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	long := strings.Repeat("é", 255)
	if a := e.outbox(namedOp("ok", v, long, "active", nameChanged("Invented item 3", earlier), 810))["ok"]; a.Error != nil || e.shop.Title(p) != long {
		t.Fatalf("255 characters must pass: %+v", a)
	}
}

func TestAProductUpdateUserErrorNeverDeadLettersAnAlreadyWrittenPrice(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.RefuseProductType(true)
	ch := fmt.Sprintf(`{"name":{"was":"Invented item 3","at":%q},"price":{"was":810,"at":%q}}`, earlier, earlier)
	a := e.outbox(namedOp("x1", v, "Renamed in UT", "active", ch, 999))["x1"]
	if a.Error != nil || e.shop.Price(v) != "9.99" || e.shop.Title(p) != "Invented item 3" {
		t.Fatalf("%+v price %s title %q", a, e.shop.Price(v), e.shop.Title(p))
	}
	if n, _ := countLogs(e, "listing.product_update_rejected"); n != 1 {
		t.Errorf("rejection logged %d times", n)
	}
	// Nothing else was written: the refusal surfaces instead of a silent ack.
	a = e.outbox(namedOp("x2", v, "Renamed in UT", "active", nameChanged("Invented item 3", earlier), 810))["x2"]
	if a.Error == nil || a.Error.Code != "rejected_by_channel" || a.Error.Retryable {
		t.Fatalf("name-only refusal: %+v", a)
	}
}

// Unpublish and reactivation.

func unpublishOp(id, variant string) channel.OutboxOp {
	return channel.OutboxOp{ID: id, Kind: channel.OpListingUnpublish, ExternalID: variant,
		Payload: json.RawMessage(fmt.Sprintf(`{"cloud_id":"c","external_id":%q}`, variant))}
}

func TestUnpublishSetsDraftOnceAndNeverTouchesArchivedOrMultiVariantProducts(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	if a := e.outbox(unpublishOp("d1", v))["d1"]; a.Error != nil || a.ExternalID != v || e.shop.Status(p) != "DRAFT" {
		t.Fatalf("%+v status %s", a, e.shop.Status(p))
	}
	if a := e.outbox(unpublishOp("d2", v))["d2"]; a.Error != nil || e.shop.Performed("status", p) != 1 {
		t.Fatalf("second unpublish %+v wrote %d times", a, e.shop.Performed("status", p))
	}
	e.shop.SetStatus(p, "ARCHIVED")
	if a := e.outbox(unpublishOp("d3", v))["d3"]; a.Error != nil || e.shop.Status(p) != "ARCHIVED" || e.shop.Performed("status", p) != 1 {
		t.Fatalf("archived %+v status %s", a, e.shop.Status(p))
	}
	// A product with two variants stays up: one variant's item going inactive is not the product's.
	p1, v1 := e.shop.ProductIDs()[0], e.shop.VariantIDs(1)[0]
	if a := e.outbox(unpublishOp("d4", v1))["d4"]; a.Error != nil || e.shop.Status(p1) != "ACTIVE" {
		t.Fatalf("multi-variant %+v status %s", a, e.shop.Status(p1))
	}
}

func TestUnpublishInvalidIdIsRefused(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	a := e.outbox(unpublishOp("d1", "gid://shopify/Product/1003"))["d1"]
	if a.Error == nil || a.Error.Code != "invalid_payload" || len(e.shop.Requests()) != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestReactivationNeedsChangedActiveAndNeverTouchesArchived(t *testing.T) {
	activeChanged := fmt.Sprintf(`{"active":{"was":false,"at":%q}}`, earlier)
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetStatus(p, "DRAFT") // the merchant drafted it

	// A price edit (or a rename) never re-publishes a product the merchant drafted.
	ch := fmt.Sprintf(`{"price":{"was":810,"at":%q}}`, earlier)
	if a := e.outbox(namedOp("a1", v, "Invented item 3", "active", ch, 999))["a1"]; a.Error != nil || e.shop.Status(p) != "DRAFT" || e.shop.Price(v) != "9.99" {
		t.Fatalf("%+v status %s", a, e.shop.Status(p))
	}
	// The cloud says active changed: re-published.
	if a := e.outbox(namedOp("a2", v, "Invented item 3", "active", activeChanged, 999))["a2"]; a.Error != nil || e.shop.Status(p) != "ACTIVE" {
		t.Fatalf("%+v status %s", a, e.shop.Status(p))
	}
	// Archived stays archived.
	e.shop.SetStatus(p, "ARCHIVED")
	if a := e.outbox(namedOp("a3", v, "Invented item 3", "active", activeChanged, 999))["a3"]; a.Error != nil || e.shop.Status(p) != "ARCHIVED" {
		t.Fatalf("%+v status %s", a, e.shop.Status(p))
	}
}

// A multi-variant item's derived name ("Title (Blue, M)") can be long; it is
// never written, so it must not block the op's price.
func TestAnOverLongNameOfAMultiVariantItemDoesNotBlockItsPrice(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	ch := fmt.Sprintf(`{"name":{"was":"x","at":%q},"price":{"was":610,"at":%q}}`, earlier, earlier)
	if a := e.outbox(namedOp("m1", v, strings.Repeat("n", 300), "active", ch, 999))["m1"]; a.Error != nil || e.shop.Price(v) != "9.99" {
		t.Fatalf("%+v price %s", a, e.shop.Price(v))
	}
}

func TestAnEmptyChangedIgnoresTheCategoryToo(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	op := cloudUpsert("c1", v, 810, "GBP") // carries category_path ["A","B"]
	op.Payload = json.RawMessage(strings.Replace(string(op.Payload), `"category_path"`, `"changed":{},"category_path"`, 1))
	if a := e.outbox(op)["c1"]; a.Error != nil || len(e.shop.Requests()) != 0 {
		t.Fatalf("%+v, %d requests", a, len(e.shop.Requests()))
	}
}

func TestReactivationIsSkippedWhenTheMerchantDraftedItLater(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetStatus(p, "DRAFT") // the merchant drafted it after the cloud's change at "earlier"
	// The cloud believed the channel active (was=true) and wants ACTIVE again;
	// Shopify differs from was and was edited later: Shopify wins.
	ch := fmt.Sprintf(`{"active":{"was":true,"at":%q}}`, earlier)
	if a := e.outbox(namedOp("a1", v, "Invented item 3", "active", ch, 810))["a1"]; a.Error != nil || e.shop.Status(p) != "DRAFT" || e.shop.Performed("status", p) != 0 {
		t.Fatalf("%+v status %s", a, e.shop.Status(p))
	}
	// Our change newer than the Shopify edit: written.
	ch = fmt.Sprintf(`{"active":{"was":true,"at":%q}}`, later)
	if a := e.outbox(namedOp("a2", v, "Invented item 3", "active", ch, 810))["a2"]; a.Error != nil || e.shop.Status(p) != "ACTIVE" {
		t.Fatalf("%+v status %s", a, e.shop.Status(p))
	}
}

func TestNameAndStatusShareOneProductUpdate(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetStatus(p, "DRAFT")
	ch := fmt.Sprintf(`{"name":{"was":"Invented item 3","at":%q},"active":{"was":false,"at":%q}}`, earlier, earlier)
	if a := e.outbox(namedOp("s1", v, "Renamed in UT", "active", ch, 810))["s1"]; a.Error != nil {
		t.Fatalf("%+v", a)
	}
	if e.shop.Title(p) != "Renamed in UT" || e.shop.Status(p) != "ACTIVE" || countOp(e, "ProductType") != 1 {
		t.Fatalf("title %q status %s, %d product updates", e.shop.Title(p), e.shop.Status(p), countOp(e, "ProductType"))
	}
}
