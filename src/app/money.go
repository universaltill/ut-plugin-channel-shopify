package app

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Shopify sends money as a decimal string ("35.00") next to a currency code.
// The adapter turns it into integer minor units by string arithmetic only, so
// no float ever touches an amount.

var amountRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,14})(\.[0-9]{1,6})?$`)

// exponent is the number of minor-unit digits of an ISO 4217 currency.
func exponent(cur string) int {
	switch cur {
	case "JPY", "KRW", "VND", "CLP", "ISK", "UGX", "PYG", "RWF", "XAF", "XOF", "XPF", "KMF", "GNF", "DJF", "VUV", "BIF":
		return 0
	case "BHD", "KWD", "OMR", "JOD", "TND", "IQD", "LYD":
		return 3
	}
	return 2
}

// parseMoney turns "35.00" in cur into 3500. It refuses a negative amount,
// a malformed one, and one with more non-zero decimals than the currency has
// (it never rounds silently).
func parseMoney(amount, cur string) (int64, bool) {
	if !curRe.MatchString(cur) || !amountRe.MatchString(amount) {
		return 0, false
	}
	whole, frac, _ := strings.Cut(amount, ".")
	frac = strings.TrimPrefix(frac, ".")
	e := exponent(cur)
	if len(frac) > e {
		if strings.Trim(frac[e:], "0") != "" {
			return 0, false
		}
		frac = frac[:e]
	}
	frac += strings.Repeat("0", e-len(frac))
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// formatMoney is the inverse: 3500 GBP is "35.00".
func formatMoney(minor int64, cur string) string {
	e := exponent(cur)
	s := strconv.FormatInt(minor, 10)
	if e == 0 {
		return s
	}
	if len(s) <= e {
		s = strings.Repeat("0", e-len(s)+1) + s
	}
	return s[:len(s)-e] + "." + s[len(s)-e:]
}

// rateBP turns a tax rate (0.2) into basis points (2000). A rate is a ratio,
// not money, so a float is fine; the result is rounded half-up and bounded.
func rateBP(rate float64) (int64, bool) {
	if math.IsNaN(rate) || rate < 0 || rate > 1 {
		return 0, false
	}
	return int64(math.Round(rate * 10000)), true
}
