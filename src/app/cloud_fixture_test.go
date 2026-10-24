//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
)

// loadOp reads a captured op: a leading "//" comment line, then the op JSON
// exactly as the cloud's outbox_next hands it to the plugin.
func loadOp(t *testing.T, name string) channel.OutboxOp {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "//") {
		t.Fatalf("%s has no header comment", name)
	}
	_, s, _ = strings.Cut(s, "\n")
	var op channel.OutboxOp
	if err := json.Unmarshal([]byte(s), &op); err != nil {
		t.Fatal(err)
	}
	return op
}

func countLogs(e *env, code string) (n int, fields map[string]any) {
	for _, l := range e.em.Host.Logs {
		if l.Code == code {
			n++
			fields = l.Fields
		}
	}
	return
}

func TestCapturedCloudUpsertPushesTheVariantPriceAndKeepsTheVariantId(t *testing.T) {
	op := loadOp(t, "cloud_upsert_op.json")
	e := newEnv(t, fakeshop.Options{})
	v := op.ExternalID
	if v != e.shop.VariantIDs(1)[0] {
		t.Fatalf("fixture variant %s is not the fake shop's first variant", v)
	}
	e.shop.SetPrice(v, "1.00") // the shop disagrees with the cloud's 6.10
	w := e.shop.Writes()
	a := e.outbox(op)[op.ID]
	if a.Error != nil || a.ExternalID != v {
		t.Fatalf("ack %+v", a)
	}
	if e.shop.Price(v) != "6.10" || e.shop.Writes() != w+1 {
		t.Fatalf("price %q, writes %d -> %d", e.shop.Price(v), w, e.shop.Writes())
	}
	// Only content fields count: name, description, status, sku, barcodes.
	n, f := countLogs(e, "listing.fields_ignored")
	if n != 1 || f["count"] != float64(5) {
		t.Errorf("fields_ignored logged %d times with %v, want once with count 5", n, f)
	}
	// Re-sending the same bytes is a no-op ack (price already there).
	w = e.shop.Writes()
	if a := e.outbox(op)[op.ID]; a.Error != nil || a.ExternalID != v || e.shop.Writes() != w {
		t.Fatalf("resend %+v writes %d -> %d", a, w, e.shop.Writes())
	}
}

func TestCapturedCloudCreateIsRefusedAndSendsNothing(t *testing.T) {
	op := loadOp(t, "cloud_create_op.json")
	if op.ExternalID != "" {
		t.Fatal("fixture is not a create")
	}
	e := newEnv(t, fakeshop.Options{})
	a := e.outbox(op)[op.ID]
	if a.Error == nil || a.Error.Code != "create_not_supported" || a.Error.Retryable {
		t.Fatalf("%+v", a)
	}
	if len(e.shop.Requests()) != 0 {
		t.Errorf("%d requests", len(e.shop.Requests()))
	}
}

// Content fields are counted; structural keys (cloud_id, external_id,
// variants, currency...) are not. A price-only record logs nothing, and the
// cloud's real record always carries content, so a real push logs once.
func TestFieldsIgnoredCountsOnlyContentFields(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	body := fmt.Sprintf(`{"cloud_id":"c","external_id":%q,"variants":[{"external_id":%q,"cloud_id":"c","price_minor":777,"currency":"GBP","tax_included":true}]}`, v, v)
	e.outbox(channel.OutboxOp{ID: "s", Kind: channel.OpListingUpsert, ExternalID: v, Payload: json.RawMessage(body)})
	if n, _ := countLogs(e, "listing.fields_ignored"); n != 0 {
		t.Errorf("structural keys alone logged fields_ignored")
	}
	if e.shop.Price(v) != "7.77" {
		t.Fatal("not pushed")
	}
}

func upsertTax(id, variant string, price int64, taxIncluded bool) channel.OutboxOp {
	body := fmt.Sprintf(`{"variants":[{"external_id":%q,"price_minor":%d,"currency":"GBP","tax_included":%v}]}`, variant, price, taxIncluded)
	return channel.OutboxOp{ID: id, Kind: channel.OpListingUpsert, ExternalID: variant, Payload: json.RawMessage(body)}
}

// The cloud's price is in the basis recorded when the item was imported
// (variants[0].tax_included = shop.taxesIncluded && taxable). A payload that
// says "tax included" while the shop now EXCLUDES tax carries an incl-tax
// number that Shopify would add tax to again (a stale link): refused, nothing
// written. The other mismatches are legitimate or ambiguous and still pass:
// false in a tax-included shop is a non-taxable variant (catalogue.go).
func TestTaxBasisGuard(t *testing.T) {
	for _, c := range []struct {
		name          string
		shopExcl, inc bool
		refuse        bool
	}{
		{"incl payload, incl shop", false, true, false},
		{"non-taxable variant in an incl shop", false, false, false},
		{"excl payload, excl shop", true, false, false},
		{"incl payload, excl shop (stale link)", true, true, true},
	} {
		e := newEnv(t, fakeshop.Options{TaxesExcluded: c.shopExcl})
		v := e.shop.VariantIDs(1)[0]
		w := e.shop.Writes()
		a := e.outbox(upsertTax("t", v, 3300, c.inc))["t"]
		if c.refuse {
			if a.Error == nil || a.Error.Code != "tax_basis_mismatch" || a.Error.Retryable || e.shop.Writes() != w || e.shop.Price(v) != "6.10" {
				t.Errorf("%s: %+v writes %d -> %d price %q", c.name, a, w, e.shop.Writes(), e.shop.Price(v))
			}
			continue
		}
		if a.Error != nil || a.ExternalID != v || e.shop.Price(v) != "33.00" {
			t.Errorf("%s: %+v price %q", c.name, a, e.shop.Price(v))
		}
	}
}

// A shop answer that is not the variant asked for, or has no product, is
// not_found and nothing is written.
func TestVariantLookupGuards(t *testing.T) {
	for _, mode := range []string{"foreign", "noproduct"} {
		e := newEnv(t, fakeshop.Options{})
		v := e.shop.VariantIDs(1)[0]
		e.shop.SetVariantLookup(mode)
		w := e.shop.Writes()
		a := e.outbox(cloudUpsert("x", v, 3300, "GBP"))["x"]
		if a.Error == nil || a.Error.Code != "not_found" || a.Error.Retryable || e.shop.Writes() != w || e.shop.Price(v) != "6.10" {
			t.Errorf("%s: %+v writes %d -> %d", mode, a, w, e.shop.Writes())
		}
	}
}

// The upper price bound is exact: 999_999_999_999 passes the guard, one more
// is refused before any request.
func TestPriceUpperBound(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	a := e.outbox(cloudUpsert("hi", v, 1_000_000_000_000, "GBP"))["hi"]
	if a.Error == nil || a.Error.Code != "invalid_payload" || a.Error.Retryable || len(e.shop.Requests()) != 0 {
		t.Fatalf("%+v requests %d", a, len(e.shop.Requests()))
	}
	a = e.outbox(cloudUpsert("max", v, 999_999_999_999, "GBP"))["max"]
	if a.Error != nil {
		t.Fatalf("max price refused: %+v", a)
	}
}

// Every request of a price push carries a cost hint, the variant read included.
func TestPricePushRequestsAllCarryACostHint(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	v := e.shop.VariantIDs(1)[0]
	e.outbox(cloudUpsert("c", v, 3300, "GBP"))
	for _, s := range e.em.Seen() {
		if s.CostHint <= 0 || s.CostHint > 1000 {
			t.Errorf("cost hint %d for %.60s", s.CostHint, s.Body)
		}
	}
	if len(e.em.Seen()) != 3 {
		t.Errorf("%d requests, want ShopCurrency + VariantPrice + VariantPrices", len(e.em.Seen()))
	}
}
