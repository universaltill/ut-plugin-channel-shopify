//go:build !wasip1

package app_test

import (
	"testing"

	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

func TestStampComparesInstantsAcrossFractionsAndOffsets(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		after bool
		ok    bool
	}{
		{"2026-10-24T09:00:01Z", "2026-10-24T09:00:00Z", true, true},
		{"2026-10-24T09:00:00Z", "2026-10-24T09:00:00Z", false, true},
		{"2026-10-24T09:00:00.5Z", "2026-10-24T09:00:00Z", true, true},
		{"2026-10-24T09:00:00Z", "2026-10-24T09:00:00.5Z", false, true},
		{"2026-10-24T10:00:00+01:00", "2026-10-24T09:00:00Z", false, true},
		{"2026-10-24T10:00:01+01:00", "2026-10-24T09:00:00Z", true, true},
		{"2026-10-24T08:00:00-02:00", "2026-10-24T09:00:00Z", true, true},
		{"2026-12-31T23:59:59Z", "2027-01-01T00:00:00Z", false, true},
		{"2028-03-01T00:00:00Z", "2028-02-29T00:00:00Z", true, true},
		{"2026-10-24 09:00:00Z", "2026-10-24T09:00:00Z", false, false},
		{"", "2026-10-24T09:00:00Z", false, false},
		{"2026-13-24T09:00:00Z", "2026-10-24T09:00:00Z", false, false},
		{"2026-10-24T09:00:00", "2026-10-24T09:00:00Z", false, false},
		{"2026-10-24T09:00:0xZ", "2026-10-24T09:00:00Z", false, false},
	} {
		after, ok := app.StampAfter(c.a, c.b)
		if ok != c.ok || after != c.after {
			t.Errorf("%q after %q = %v,%v want %v,%v", c.a, c.b, after, ok, c.after, c.ok)
		}
	}
}
