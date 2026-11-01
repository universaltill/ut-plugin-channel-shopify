//go:build !wasip1

package app_test

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
)

// The order records a tick submits, byte for byte, with the shop's prices
// including and excluding tax. The golden file was written by the adapter's former
// local orderRecord type, before it moved to the SDK's channel.Order (ut-docs#4994),
// so a drift between the SDK type and the contract's record shows up here as
// a diff. Regenerate with UPDATE_GOLDEN=1 only for a change the contract
// itself makes.
func TestSubmittedOrderBytesAreStable(t *testing.T) {
	var got bytes.Buffer
	for _, excl := range []bool{false, true} {
		e := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
		if n := e.tick(); n != 3 {
			t.Fatalf("taxes_excluded=%v: %d orders", excl, n)
		}
		fmt.Fprintf(&got, "# taxes_excluded=%v\n", excl)
		for _, r := range e.submitted(channel.KindOrder) {
			got.Write(r)
			got.WriteByte('\n')
		}
	}
	const golden = "testdata/orders.golden.jsonl"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("submitted orders changed on the wire:\n got %s\nwant %s", got.Bytes(), want)
	}
}
