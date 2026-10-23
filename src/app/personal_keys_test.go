//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
)

// personalKeys mirrors ut-cloud internal/channelhost/host.go personalKeys
// (copied, read-only, on 2026-10-23): the host refuses an order record that
// has any of these JSON keys at ANY depth, case-insensitively. Keep it in step
// with the cloud's list; ut-docs#4890 tracks the cloud side.
var personalKeys = map[string]bool{
	"name": true, "first_name": true, "last_name": true, "full_name": true,
	"email": true, "phone": true, "mobile": true, "telephone": true,
	"address": true, "address1": true, "address2": true, "street": true,
	"city": true, "postcode": true, "postal_code": true, "zip": true,
	"shipping_address": true, "billing_address": true,
}

// personalPaths walks a decoded record the way the host's scan does and
// returns the path of every key the scan would refuse (array indexes are
// written as []).
func personalPaths(v any, path string, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if personalKeys[strings.ToLower(k)] {
				out[p] = true
			}
			personalPaths(vv, p, out)
		}
	case []any:
		for _, vv := range x {
			personalPaths(vv, path+"[]", out)
		}
	}
}

// maxLineTitleBytes mirrors ut-cloud channelhost maxLineTitleBytes: the one
// "name" the scan lets through is a string of at most this many BYTES after
// JSON decoding.
const maxLineTitleBytes = 256

// hostRefuses mirrors the cloud's scanPersonal (ut-docs#4890, ut-cloud PR 601):
// the key "name" is allowed only directly on an element of the record's
// ROOT-level "lines" array, as a string of at most maxLineTitleBytes bytes;
// every other personal key, anywhere, is refused. inLines is true only while
// walking those elements, and is not passed any deeper.
func hostRefuses(v any, depth int, inLines bool) bool {
	if depth > 24 {
		return true
	}
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			lk := strings.ToLower(k)
			if personalKeys[lk] {
				title, isString := vv.(string)
				if inLines && lk == "name" && isString && len(title) <= maxLineTitleBytes {
					continue
				}
				return true
			}
			if depth == 0 && lk == "lines" {
				if arr, ok := vv.([]any); ok {
					for _, el := range arr {
						if hostRefuses(el, depth+2, true) {
							return true
						}
					}
					continue
				}
			}
			if hostRefuses(vv, depth+1, false) {
				return true
			}
		}
	case []any:
		for _, vv := range x {
			if hostRefuses(vv, depth+1, false) {
				return true
			}
		}
	}
	return false
}

func TestTheHostScanMirrorHasTheTitleLengthAndRootOnlyRules(t *testing.T) {
	line := func(name any) map[string]any {
		return map[string]any{"lines": []any{map[string]any{"name": name, "sku": "s"}}}
	}
	for _, c := range []struct {
		why     string
		rec     any
		refused bool
	}{
		{"256 bytes pass", line(strings.Repeat("a", 256)), false},
		{"257 bytes are refused", line(strings.Repeat("a", 257)), true},
		{"a title is a string", line(map[string]any{"first_name": "x"}), true},
		{"a number is not a title", line(7.0), true},
		{"lines at the root only", map[string]any{"extra": map[string]any{"lines": []any{map[string]any{"name": "t"}}}}, true},
		{"name deeper inside a line", map[string]any{"lines": []any{map[string]any{"x": map[string]any{"name": "t"}}}}, true},
		{"name beside lines at the root", map[string]any{"name": "t", "lines": []any{}}, true},
		{"the key is case-insensitive", map[string]any{"lines": []any{map[string]any{"NAME": "t"}}}, false},
	} {
		if got := hostRefuses(c.rec, 0, false); got != c.refused {
			t.Errorf("%s: refused=%v, want %v", c.why, got, c.refused)
		}
	}
}

// The contract REQUIRES lines[].name, and the host's scan refuses the key
// "name" anywhere (ut-docs#4890, tracked cloud-side): real orders pass only
// once that fix is deployed. Until then this is the one known exception, and
// any other key the host would refuse must fail here, loudly, in this repo.
func TestTheOnlyKeyTheHostsPersonalDataScanWouldRefuseIsLinesName(t *testing.T) {
	const known = "lines[].name"
	found := map[string]bool{}
	for _, excl := range []bool{false, true} {
		e := newEnv(t, fakeshop.Options{TaxesExcluded: excl})
		e.shop.SetOrderCodeDiscount("gid://shopify/Order/5002", 0, 150)
		e.shop.RefundLine("gid://shopify/Order/5003", 0, 1, "RETURN")
		e.shop.EditRemoveUnits("gid://shopify/Order/5001", 0, 1)
		e.tick()
		e.ask(channel.AskFetch, channel.FetchInput{Event: channel.EventOrderChanged, ResourceIDs: []string{"gid://shopify/Order/5001"}})
		recs := e.submitted(channel.KindOrder)
		if len(recs) < 4 {
			t.Fatalf("%d order records", len(recs))
		}
		for _, raw := range recs {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			personalPaths(v, "", found)
		}
	}
	var paths []string
	for p := range found {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if fmt.Sprint(paths) != fmt.Sprint([]string{known}) {
		t.Errorf("the host's personal-data scan would refuse the key(s) %v; only %q is a known (ut-docs#4890) exception", paths, known)
	}
}

func TestThePersonalKeyScanMirrorCatchesANewKey(t *testing.T) {
	out := map[string]bool{}
	personalPaths(map[string]any{"customer": map[string]any{"Email": "x", "external_id": "y"}, "lines": []any{map[string]any{"name": "z", "sku": "s"}}}, "", out)
	if len(out) != 2 || !out["customer.Email"] || !out["lines[].name"] {
		t.Fatalf("%v", out)
	}
}
