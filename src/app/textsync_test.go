//go:build !wasip1

package app_test

import (
	"testing"

	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

func TestNormalizeText(t *testing.T) {
	for in, want := range map[string]string{
		"<p>a</p><p>b</p>":    "a b",
		"a\n\n b ":            "a b",
		"x &amp; y &lt;z&gt;": "x & y <z>",
		"<p>x &amp; y</p>":    "x & y",
		"1 < 2 and 3 > 2":     "1 < 2 and 3 > 2",
		"":                    "",
		"<b>bold</b>text":     "bold text",
		"a\xffb":              "ab",
	} {
		if got := app.NormalizeText(in); got != want {
			t.Errorf("NormalizeText(%q) = %q, want %q", in, got, want)
		}
	}
}
