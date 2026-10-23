//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

func TestClipCutsOnARuneBoundaryAndNeverGrowsOnEncoding(t *testing.T) {
	for name, in := range map[string]string{
		"accented":          strings.Repeat("é", 200),
		"emoji at boundary": strings.Repeat("a", 248) + "😀😀",
		"cjk":               strings.Repeat("漢", 100),
		"invalid bytes":     strings.Repeat("\xff", 300),
		"mixed invalid":     strings.Repeat("a", 200) + strings.Repeat("\xc3", 100),
		"ascii":             strings.Repeat("x", 600),
	} {
		got := app.Clip(in, app.MaxTitleBytes)
		if len(got) > app.MaxTitleBytes || !utf8.ValidString(got) {
			t.Errorf("%s: %d bytes, valid=%v", name, len(got), utf8.ValidString(got))
		}
		// What the host decodes after json.Marshal must be the same string.
		b, _ := json.Marshal(got)
		var back string
		_ = json.Unmarshal(b, &back)
		if back != got {
			t.Errorf("%s: encoding changed the string", name)
		}
	}
	if got := app.Clip(strings.Repeat("a", 248)+"😀", 250); got != strings.Repeat("a", 248) {
		t.Errorf("a rune that does not fit is dropped whole, got %d bytes", len(got))
	}
	if got := app.Clip("héllo", 100); got != "héllo" {
		t.Errorf("a short string is untouched: %q", got)
	}
}

func TestLongNonASCIITitlesPassTheHostsScan(t *testing.T) {
	cases := map[string]string{
		"accented":          strings.Repeat("é", 200),
		"emoji at boundary": strings.Repeat("a", 248) + "😀😀",
		"cjk":               strings.Repeat("漢", 100),
		"replacement chars": strings.Repeat("�", 100), // what an invalid-UTF-8 title is after Shopify's own JSON
		"ascii":             strings.Repeat("x", 600),
	}
	for name, title := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fakeshop.Options{})
			e.shop.MutateOrder("gid://shopify/Order/5001", func(o *fakeshop.Order) {
				o.Name = title
				o.Lines[0].Name, o.Lines[0].SKU = title, title
			})
			e.tick()
			found := false
			for _, raw := range e.submitted(channel.KindOrder) {
				var v map[string]any
				if err := json.Unmarshal(raw, &v); err != nil {
					t.Fatal(err)
				}
				if hostRefuses(v, 0, false) {
					t.Errorf("the host's scan would refuse %.60s...", raw)
				}
				if v["external_id"] != "gid://shopify/Order/5001" {
					continue
				}
				found = true
				l := v["lines"].([]any)[0].(map[string]any)
				for field, limit := range map[string]int{"name": 256, "sku": 100} {
					s := l[field].(string)
					if len(s) > limit || len(s) == 0 || !utf8.ValidString(s) || !strings.HasPrefix(title, s) {
						t.Errorf("line %s: %d bytes (limit %d), valid=%v", field, len(s), limit, utf8.ValidString(s))
					}
				}
				if n := v["order_number"].(string); len(n) > 40 || !utf8.ValidString(n) {
					t.Errorf("order_number %d bytes", len(n))
				}
			}
			if !found {
				t.Fatal("the order was not submitted")
			}
		})
	}
}

func TestRestockedIsNeverMoreThanRefundedEvenForInconsistentShopData(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	id := "gid://shopify/Order/5001" // 1 unit ordered
	e.shop.RefundLine(id, 0, 3, "RETURN")
	e.tick()
	o := lineOf(t, e, id)
	if o.RefundedQuantity != 1 || o.RestockedQuantity != 1 {
		t.Fatalf("%+v: refunded is capped at qty 1 and restocked at refunded (the cloud rejects restocked > refunded)", o)
	}
}

func logged(e *env, code string) (map[string]any, bool) {
	for _, l := range e.em.Host.Logs {
		if l.Code == code {
			return l.Fields, true
		}
	}
	return nil, false
}

func orderByID(os []order, id string) *order {
	for i := range os {
		if os[i].ExternalID == id {
			return &os[i]
		}
	}
	return nil
}

func TestEveryDiscountAllocationIsOnTheLineSoTheFormulaHolds(t *testing.T) {
	for _, excl := range []bool{false, true} {
		e := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
		id := "gid://shopify/Order/5002" // line discount 1.00
		e.shop.SetOrderCodeDiscount(id, 0, 150)
		e.tick()
		o := orderByID(e.orders(), id)
		if o == nil || o.Lines[0].DiscountLineMinor != 250 || o.TotalMinor != o.formula() {
			t.Fatalf("excl=%v: %+v", excl, o)
		}
	}
}

func TestEveryShippingLineIsSummed(t *testing.T) {
	for _, excl := range []bool{false, true} {
		e := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
		id := "gid://shopify/Order/5001" // first shipping line 3.99
		e.shop.MutateOrder(id, func(o *fakeshop.Order) {
			o.ExtraShip = []fakeshop.ShipLine{{Amount: 250, Rate: 0.2}}
			o.Total += 250
			if excl {
				o.Total += 50
			}
		})
		e.tick()
		o := orderByID(e.orders(), id)
		if o == nil || o.ShippingMinor != 649 || o.ShippingTaxBP != 2000 || o.TotalMinor != o.formula() {
			t.Fatalf("excl=%v: %+v", excl, o)
		}
	}
}

func TestShippingLinesWithDifferentTaxRatesOrTooManyAreRefusedAndLogged(t *testing.T) {
	id := "gid://shopify/Order/5001"
	e := newEnv(t, fakeshop.Options{})
	e.shop.MutateOrder(id, func(o *fakeshop.Order) { o.ExtraShip = []fakeshop.ShipLine{{Amount: 250, Rate: 0.05}} })
	e.tick()
	if _, ok := logged(e, "order.refused.mixed_shipping_tax"); !ok || orderByID(e.orders(), id) != nil {
		t.Fatalf("mixed shipping tax: logged=%v", ok)
	}
	e = newEnv(t, fakeshop.Options{})
	e.shop.MutateOrder(id, func(o *fakeshop.Order) {
		for i := 0; i < app.MaxShipLines; i++ { // 1 + 10 = 11
			o.ExtraShip = append(o.ExtraShip, fakeshop.ShipLine{Amount: 1, Rate: 0.2})
		}
	})
	e.tick()
	if _, ok := logged(e, "order.refused.too_many_shipping_lines"); !ok || orderByID(e.orders(), id) != nil {
		t.Fatalf("11 shipping lines: logged=%v", ok)
	}
}

func TestTaxExcludedOrdersUseTheOrdersOwnTax(t *testing.T) {
	e := newEnv(t, fakeshop.Options{TaxesExcluded: true})
	id := "gid://shopify/Order/5003"
	var perLine int64
	e.shop.MutateOrder(id, func(o *fakeshop.Order) {
		// Shopify rounds the order's tax once: a cent more than the sum of the
		// per-line roundings.
		perLine = o.Total - o.Lines[0].Qty*o.Lines[0].Unit - o.Ship
		tax := perLine + 1
		o.TaxTotal = &tax
		o.Total += 1
	})
	e.tick()
	o := orderByID(e.orders(), id)
	if o == nil {
		_, refused := logged(e, "order.refused.total_mismatch")
		t.Fatalf("a one-cent rounding difference in the per-line taxes refused the order (logged=%v)", refused)
	}
	if o.TaxAddedMinor != perLine+1 || o.TotalMinor != o.formula() {
		t.Fatalf("tax_added %d (order tax %d), total %d formula %d", o.TaxAddedMinor, perLine+1, o.TotalMinor, o.formula())
	}
}

func TestAnOrderTheFormulaCannotExplainIsRefusedWithCountsOnly(t *testing.T) {
	for _, excl := range []bool{false, true} {
		e := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
		id := "gid://shopify/Order/5001"
		e.shop.MutateOrder(id, func(o *fakeshop.Order) { o.Unaccounted = 500; o.Total += 500 }) // a tip or a duty
		e.tick()
		if orderByID(e.orders(), id) != nil {
			t.Fatalf("excl=%v: an order whose total the formula does not give was submitted", excl)
		}
		f, ok := logged(e, "order.refused.total_mismatch")
		if !ok {
			t.Fatalf("excl=%v: not logged", excl)
		}
		for k, v := range f {
			if _, isNum := v.(float64); !isNum {
				t.Errorf("log field %s=%v is not a count", k, v)
			}
		}
		if orderByID(e.orders(), "gid://shopify/Order/5002") == nil {
			t.Error("one refused order stopped the others")
		}
		// And the fetch path does the same.
		e2 := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
		e2.shop.MutateOrder(id, func(o *fakeshop.Order) { o.Unaccounted = 500; o.Total += 500 })
		e2.ask(channel.AskFetch, channel.FetchInput{Event: channel.EventOrderChanged, ResourceIDs: []string{id}})
		if _, ok := logged(e2, "order.refused.total_mismatch"); !ok || len(e2.orders()) != 0 {
			t.Errorf("fetch: logged=%v, %d records", ok, len(e2.orders()))
		}
	}
}

func withLines(e *env, id string, n int) {
	e.shop.MutateOrder(id, func(o *fakeshop.Order) {
		first := o.Lines[0]
		for i := 1; i < n; i++ {
			l := first
			l.ID = fmt.Sprintf("gid://shopify/LineItem/%d", 7000+i)
			o.Lines = append(o.Lines, l)
			o.Total += l.Qty*l.Unit - l.Discount // tax-included shop: nothing is added on top
		}
	})
}

func TestLongLineListsAreCompletedWithinTheCostCapAndPastItRefused(t *testing.T) {
	id := "gid://shopify/Order/5001"
	e := newEnv(t, fakeshop.Options{})
	withLines(e, id, 60) // the first page of 20 plus two more
	e.tick()
	o := orderByID(e.orders(), id)
	if o == nil || len(o.Lines) != 60 || o.TotalMinor != o.formula() {
		t.Fatalf("60 lines: %+v", o)
	}
	if c := askCost(e, 0); c > app.AskCostCap+300 {
		t.Fatalf("an ask requested %d points", c)
	}
	e = newEnv(t, fakeshop.Options{})
	withLines(e, id, 61)
	e.tick()
	if _, ok := logged(e, "order.refused.too_many_lines"); !ok || orderByID(e.orders(), id) != nil {
		t.Fatalf("61 lines must be refused and logged, never truncated (logged=%v)", ok)
	}
}

func TestAnOrderPageThatCannotBeCompletedInTheBudgetIsReadAgainNotLost(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.AddOrders(1)
	var extra string
	for _, o := range e.shop.Orders() {
		extra = o.ID // the last one
	}
	withLines(e, "gid://shopify/Order/5001", 40)
	withLines(e, extra, 60)
	e.tick()
	if _, ok := logged(e, "orders.budget_reached"); !ok {
		t.Fatal("the ask ran past its cost cap without stopping")
	}
	if c := askCost(e, 0); c > app.AskCostCap {
		t.Fatalf("%d points requested, cap %d", c, app.AskCostCap)
	}
	if string(e.em.Host.Cursors["orders.after"]) == "" || string(e.em.Host.Cursors["orders.since"]) != "" {
		t.Fatalf("the stopped sweep must keep its place: %v", e.em.Host.Cursors)
	}
	e.tickAll()
	seen := map[string]int{}
	for _, o := range e.orders() {
		seen[o.ExternalID] = len(o.Lines)
	}
	if len(seen) != 4 || seen[extra] != 60 || seen["gid://shopify/Order/5001"] != 40 {
		t.Fatalf("after resuming: %v", seen)
	}
}

func TestFetchStopsAtItsBudgetAndTheTickSweepReadsTheRest(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.AddOrders(7)
	var ids []string
	for _, o := range e.shop.Orders() {
		ids = append(ids, o.ID)
	}
	e.ask(channel.AskFetch, channel.FetchInput{Event: channel.EventOrderChanged, ResourceIDs: ids})
	if c := askCost(e, 0); c > app.AskCostBudget+300 {
		t.Fatalf("%d points requested in one ask: 10 orders cannot fit the 30 s deadline", c)
	}
	first := len(e.orders())
	if _, ok := logged(e, "fetch.deferred_to_tick"); !ok || first == 0 || first >= len(ids) {
		t.Fatalf("%d of %d orders submitted, deferral logged=%v", first, len(ids), ok)
	}
	// The same ask again would read the same first orders, so the rest is the
	// tick's: it sweeps every order updated since its cursor.
	e.tickAll()
	seen := map[string]bool{}
	for _, o := range e.orders() {
		seen[o.ExternalID] = true
	}
	if len(seen) != len(ids) {
		t.Fatalf("%d of %d distinct orders after the tick", len(seen), len(ids))
	}
}

func TestImportStopsAtItsCostBudgetAndResumes(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 300})
	from := len(e.em.Seen())
	var ans struct{ Done bool }
	if err := json.Unmarshal(e.ask(channel.AskImport, struct{}{}), &ans); err != nil {
		t.Fatal(err)
	}
	if ans.Done {
		t.Fatal("300 products fit one import ask: the 5 minute deadline is not protected by the budget")
	}
	if c := askCost(e, from); c > app.ImportCostBudget+1500 {
		t.Fatalf("an import ask requested %d points", c)
	}
	e.importAll()
	if n := len(e.submitted(channel.KindListing)); n < 300 {
		t.Fatalf("%d listings after resuming", n)
	}
}
