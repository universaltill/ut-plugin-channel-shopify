//go:build !wasip1

package app_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// The stock sweep runs in the tick, never in an import (ut-docs#6821): the
// host accepts stock_observed in a tick (and a fetch or outbox drain) and
// refuses it in a pull, where it was silently dropped.

// stockRecords decodes every stock_observed record submitted so far.
func (e *env) stockRecords() (out []channel.StockObserved) {
	for _, raw := range e.submitted(channel.KindStockObserved) {
		var o channel.StockObserved
		if err := json.Unmarshal(raw, &o); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

// stockBytes is every stock_observed record, in order, as one string.
func (e *env) stockBytes() string {
	var sb strings.Builder
	for _, raw := range e.submitted(channel.KindStockObserved) {
		sb.Write(raw)
	}
	return sb.String()
}

// stockPass ticks until a full stock pass has finished (the place is cleared
// after something was observed) and returns how many ticks it took.
func (e *env) stockPass() (ticks int) {
	e.t.Helper()
	for ticks = 1; ticks <= 60; ticks++ {
		e.tick()
		if len(e.submitted(channel.KindStockObserved)) > 0 && len(e.em.Host.Cursors["tick.stock.loc"]) == 0 {
			return ticks
		}
	}
	e.t.Fatal("the stock sweep never finished")
	return 0
}

func TestTickObservesStockOfTrackedVariantsAndSkipsBadRecords(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 2})
	ids := e.shop.VariantIDs(1)
	loc := e.shop.Locations()[0]
	e.shop.SetTracked(ids[0], false)
	e.shop.SetStock(ids[1], loc, -3) // a negative level is not an observation
	e.stockPass()
	obs := e.stockRecords()
	// 2 products: product 1 has 2 variants, product 2 has 3 (5 in all);
	// one untracked, one negative.
	if len(obs) != 3 {
		t.Fatalf("%d observations, want 3 (5 variants, 1 untracked, 1 negative): %+v", len(obs), obs)
	}
	for _, o := range obs {
		if o.Variant == ids[0] || o.Variant == ids[1] {
			t.Errorf("observed %s: untracked and negative levels have nothing to observe", o.Variant)
		}
		if o.Location != loc || o.ExternalID != o.Variant+"@"+loc {
			t.Errorf("observation %+v", o)
		}
	}
	if e.shop.Writes() != 0 {
		t.Fatal("the stock sweep wrote to the shop")
	}
	if len(e.submitted(channel.KindOrder)) == 0 {
		t.Error("the tick still submits orders")
	}
}

func TestTickObservesTheFirstLevelFromTheFakeShop(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.stockPass()
	var first channel.StockObserved
	for _, o := range e.stockRecords() {
		if o.Variant == e.shop.VariantIDs(1)[0] {
			first = o
		}
	}
	if first.Location != e.shop.Locations()[0] || first.Available != 11 || first.ExternalVersion == "" {
		t.Fatalf("observation %+v", first)
	}
}

func TestTickStockSweepResumesFromItsCursorAcrossLocations(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 200, Locations: 2})
	variants := 0
	for i := 1; i <= 200; i++ {
		variants += len(e.shop.VariantIDs(i))
	}
	before, resumed := 0, false
	var ticks int
	for ticks = 1; ticks <= 80; ticks++ {
		e.tick()
		if n := e.levelRequests() - before; n > app.MaxTickStockPages {
			t.Fatalf("tick %d read %d stock pages, cap %d", ticks, n, app.MaxTickStockPages)
		}
		before = e.levelRequests()
		if len(e.em.Host.Cursors["tick.stock.loc"]) > 0 {
			resumed = true
		}
		if len(e.stockRecords()) >= 2*variants && len(e.em.Host.Cursors["tick.stock.loc"]) == 0 {
			break
		}
	}
	if !resumed || ticks < 2 {
		t.Fatalf("the sweep was not resumed from a cursor (ticks=%d)", ticks)
	}
	seen := map[string]bool{}
	for _, o := range e.stockRecords() {
		seen[o.ExternalID] = true
	}
	if len(seen) != 2*variants {
		t.Fatalf("%d distinct observations after resuming, want %d (every variant at both locations)", len(seen), 2*variants)
	}
}

func TestTickOrdersFirstAndStockOnlyWithBudgetLeft(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.AddOrders(5)
	// Orders with two extra pages of lines spends the ask's cost budget on
	// orders: no stock page may start.
	for _, o := range e.shop.Orders() {
		for i := 0; i < 44 && o.ID != ""; i++ {
			e.shop.AddOrderLine(o.ID, 1, 100)
		}
	}
	from := len(e.em.Seen())
	e.tick()
	if n := e.levelRequests(); n != 0 {
		t.Fatalf("an orders sweep that used the budget read %d stock pages", n)
	}
	if c := askCost(e, from); c > app.AskCostCap+app.AskCostBudget/10 {
		t.Fatalf("%d points requested in one tick", c)
	}
	if len(e.submitted(channel.KindOrder)) == 0 || len(e.submitted(channel.KindStockObserved)) != 0 {
		t.Fatal("orders go first, and stock waits for budget")
	}
	if len(e.em.Host.Cursors["tick.stock.loc"]) != 0 || len(e.em.Host.Cursors["tick.stock.after"]) != 0 {
		t.Fatal("the stock cursor moved though no stock was read")
	}
}

func TestTickSubmitsOrdersBeforeStock(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.tick()
	kinds := []string{}
	for _, b := range e.em.Host.Submitted {
		kinds = append(kinds, b.Kind)
	}
	if len(kinds) < 2 || kinds[0] != channel.KindOrder || kinds[len(kinds)-1] != channel.KindStockObserved {
		t.Fatalf("batches %v: want the orders first, then stock", kinds)
	}
	if n := e.levelRequests(); n == 0 || n > app.MaxTickStockPages {
		t.Fatalf("%d stock pages in one tick, want 1..%d", n, app.MaxTickStockPages)
	}
}

func TestTickStockSweepTwiceIsIdentical(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 80, Locations: 2})
	e.stockPass()
	first := e.stockBytes()
	if first == "" {
		t.Fatal("no stock observed")
	}
	e.em.Host.Submitted = nil
	e.stockPass()
	if second := e.stockBytes(); second != first {
		t.Fatal("a second sweep is not byte-identical to the first: the host would see changes")
	}
}

func TestAShopWithMoreLocationsThanTheSweepReadsIsLoggedNotSilentlyCut(t *testing.T) {
	logged := func(e *env) bool {
		for _, l := range e.em.Host.Logs {
			if l.Code == "stock.locations_truncated" {
				return true
			}
			if l.Code == "import.locations_truncated" {
				t.Error("the warning was renamed stock.locations_truncated")
			}
		}
		return false
	}
	e := newEnv(t, fakeshop.Options{Locations: app.MaxLocations})
	e.stockPass()
	if logged(e) {
		t.Error("exactly the cap is not a truncation")
	}
	e = newEnv(t, fakeshop.Options{Locations: app.MaxLocations + 1})
	e.stockPass()
	if !logged(e) {
		t.Error("a 21st location was dropped without a reason code in the log")
	}
}

func TestLocationsTruncatedIsLoggedOncePerPass(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 200, Locations: app.MaxLocations + 1})
	for i := 0; i < 6; i++ {
		e.tick()
	}
	n := 0
	for _, l := range e.em.Host.Logs {
		if l.Code == "stock.locations_truncated" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logged %d times over 6 ticks of one pass, want once", n)
	}
}

// An install upgraded from 0.4.4 may hold the old import stock stage: the
// import must not stay in it, and must clear it.
func TestImportIgnoresAndClearsALegacyStockStageCursor(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.em.Host.Cursors["import.stage"] = []byte("stock")
	e.em.Host.Cursors["import.stock.loc"] = []byte(e.shop.Locations()[0])
	e.em.Host.Cursors["import.stock.after"] = []byte("abc")
	e.importAll()
	if n := len(e.submitted(channel.KindListing)); n != 50 {
		t.Fatalf("%d listings: the import stayed in the old stock stage", n)
	}
	e.assertNoStockInImport()
	for _, k := range []string{"import.stage", "import.stock.loc", "import.stock.after"} {
		if len(e.em.Host.Cursors[k]) != 0 {
			t.Errorf("legacy cursor %s still set", k)
		}
	}
}

// A stock page that fails ends the sweep, not the tick: the orders were
// submitted and their cursor moved, the failure is logged by code, and the
// next tick resumes after the last good page instead of re-reading it.
func TestTickSurvivesAStockPageErrorAndResumesPastTheLastGoodPage(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 200})
	e.tickAll() // orders done: later ticks read one orders page, so stock gets 3 pages
	startAfter := string(e.em.Host.Cursors["tick.stock.after"])
	e.shop.FulfilExternally("gid://shopify/Order/5001") // an order for the failing tick to submit
	e.em.Host.Submitted = nil
	e.shop.FailAfter("Levels", 1) // page 1 reads, page 2 fails
	e.tick()                      // must not fail the ask (e.ask would t.Fatal)
	if len(e.submitted(channel.KindOrder)) == 0 {
		t.Fatal("the orders of the tick were lost")
	}
	loc, after := string(e.em.Host.Cursors["tick.stock.loc"]), string(e.em.Host.Cursors["tick.stock.after"])
	if loc != e.shop.Locations()[0] || after == "" || after == startAfter {
		t.Fatalf("stock cursor %q/%q (was %q): want the place after the last good page", loc, after, startAfter)
	}
	if _, ok := logged(e, "stock.sweep_failed"); !ok {
		t.Fatal("the failed sweep was not logged")
	}
	good := len(e.stockRecords())
	if good == 0 {
		t.Fatal("the good page was not submitted")
	}
	e.shop.FailAfter("Levels", -1)
	before := e.levelRequests()
	e.tick()
	if e.levelRequests() == before {
		t.Fatal("the next tick read no stock")
	}
	if got := e.stockRecords(); got[good].ExternalID == got[good-1].ExternalID || string(e.em.Host.Cursors["tick.stock.after"]) == after {
		t.Fatal("the next tick did not resume past the last good page")
	}
}

// The page cap is reachable and exact when the orders leave the whole budget.
func TestTickStockPageCapIsReachedWhenNoOrdersAreRead(t *testing.T) {
	e := newEnv(t, fakeshop.Options{Products: 200})
	e.tickAll() // the window now re-reads only the newest order: one orders page
	before := e.levelRequests()
	e.tick()
	n := e.levelRequests() - before
	if n != app.MaxTickStockPages {
		t.Fatalf("%d stock pages in a tick with 1 orders page, want exactly the cap %d", n, app.MaxTickStockPages)
	}
	// And a one-page-over cap would not fit the budget: 560+30+cap*300 <= 1500.
	if 560+30+app.MaxTickStockPages*300 > app.AskCostBudget {
		t.Fatal("the cap does not fit the cost budget: it can never be reached")
	}
}
