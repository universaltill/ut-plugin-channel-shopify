//go:build !wasip1

package app_test

import (
	"strings"
	"testing"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
)

// ut-docs#5014 tester phase: the op the REAL ut-cloud builds for a rename in
// UT (captured from SweepOutbound on Postgres, changed = name only) is fed to
// the REAL adapter against the fake shop. The title lands exactly once, the
// price is never touched, and the three cycles after it (the cloud re-offers
// nothing once the re-read finds the shop in line; a duplicate delivery is the
// worst case) write nothing further.
func TestCloudBuiltRenameOpDrivesTheAdapterOnceAndSettles(t *testing.T) {
	op := loadOp(t, "cloud_rename_op.json")
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetTitle(p, "Demo item 1") // the title the cloud's baseline holds (changed.name.was)
	e.shop.SetPrice(v, "5.10")        // the op's price_minor 510: in line, and not in changed

	// Re-bind the captured demo ids to the fake shop's variant.
	raw := strings.ReplaceAll(string(op.Payload), `"v-0001-0"`, `"`+v+`"`)
	op.Payload = []byte(raw)
	op.ExternalID = v

	prevWrites := e.shop.Writes()
	for cycle := 1; cycle <= 3; cycle++ {
		a := e.outbox(op)[op.ID]
		if a.Error != nil || a.ExternalID != v {
			t.Fatalf("cycle %d: ack %+v", cycle, a)
		}
		if e.shop.Title(p) != "Renamed in my" || e.shop.Price(v) != "5.10" || e.shop.Status(p) != "ACTIVE" {
			t.Fatalf("cycle %d: title %q price %s status %s", cycle, e.shop.Title(p), e.shop.Price(v), e.shop.Status(p))
		}
		if n := e.shop.Performed("title", p); n != 1 {
			t.Fatalf("cycle %d: title written %d times, want exactly 1 across all cycles", cycle, n)
		}
		if cycle == 1 {
			prevWrites = e.shop.Writes()
		} else if w := e.shop.Writes(); w != prevWrites {
			t.Fatalf("cycle %d: %d further writes (ping-pong)", cycle, w-prevWrites)
		}
	}
	if countOp(e, "VariantPrices") != 0 {
		t.Errorf("a price write happened for an op whose changed has no price")
	}
}
