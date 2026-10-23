//go:build !wasip1

package app_test

import (
	"testing"

	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

func TestMoneyIsParsedByStringArithmeticAndNeverRounded(t *testing.T) {
	for _, c := range []struct {
		amount, cur string
		minor       int64
		ok          bool
	}{
		{"35.00", "GBP", 3500, true}, {"35", "GBP", 3500, true}, {"35.5", "GBP", 3550, true}, {"0.05", "GBP", 5, true}, {"0", "GBP", 0, true},
		{"35.000", "GBP", 3500, true}, {"35.001", "GBP", 0, false}, {"35.005", "GBP", 0, false},
		{"1200", "JPY", 1200, true}, {"1200.0", "JPY", 1200, true}, {"1200.5", "JPY", 0, false},
		{"12.345", "KWD", 12345, true}, {"12.3456", "KWD", 0, false},
		{"-1.00", "GBP", 0, false}, {"1e3", "GBP", 0, false}, {"", "GBP", 0, false}, {"1,00", "GBP", 0, false}, {" 1.00", "GBP", 0, false},
		{"01.00", "GBP", 0, false}, {"1.00", "gbp", 0, false}, {"1.00", "", 0, false}, {"99999999999999.99", "GBP", 9999999999999999, true},
		{"9999999999999999.99", "GBP", 0, false},
	} {
		got, ok := app.ParseMoney(c.amount, c.cur)
		if ok != c.ok || (ok && got != c.minor) {
			t.Errorf("ParseMoney(%q, %q) = %d, %v; want %d, %v", c.amount, c.cur, got, ok, c.minor, c.ok)
		}
	}
}

func TestMoneyIsFormattedForShopify(t *testing.T) {
	for _, c := range []struct {
		minor int64
		cur   string
		want  string
	}{{3500, "GBP", "35.00"}, {5, "GBP", "0.05"}, {0, "GBP", "0.00"}, {1200, "JPY", "1200"}, {12345, "KWD", "12.345"}, {7, "KWD", "0.007"}, {123456, "GBP", "1234.56"}} {
		if got := app.FormatMoney(c.minor, c.cur); got != c.want {
			t.Errorf("FormatMoney(%d, %s) = %q, want %q", c.minor, c.cur, got, c.want)
		}
		if back, ok := app.ParseMoney(c.want, c.cur); !ok || back != c.minor {
			t.Errorf("round trip of %q gave %d", c.want, back)
		}
	}
}

func TestTaxRateBecomesBasisPoints(t *testing.T) {
	for _, c := range []struct {
		rate float64
		bp   int64
		ok   bool
	}{{0.2, 2000, true}, {0.055, 550, true}, {0.0825, 825, true}, {0, 0, true}, {1, 10000, true}, {1.01, 0, false}, {-0.1, 0, false}} {
		got, ok := app.RateBP(c.rate)
		if ok != c.ok || (ok && got != c.bp) {
			t.Errorf("RateBP(%v) = %d, %v", c.rate, got, ok)
		}
	}
}
