package app

import "testing"

var normalizeVectors = []struct{ in, want string }{
	{"<p>a</p><p>b</p>", "a b"},
	{"a<b>b</b>c", "a b c"},
	{"<strong>Fresh</strong>bread", "Fresh bread"},
	{"1 < 2 and 3 > 1", "1 < 2 and 3 > 1"},
	{"x &amp; y &lt;z&gt;", "x & y <z>"},
	{"line1\nline2", "line1 line2"},
	{"<!-- c -->text", "text"},
	{"a<br/>b", "a b"},
	{"<span>a</span><span>b</span>", "a b"},
}

func TestNormalizeTextMatchesCloudVectors(t *testing.T) {
	for _, v := range normalizeVectors {
		if got := NormalizeText(v.in); got != v.want {
			t.Errorf("NormalizeText(%q) = %q, want %q", v.in, got, v.want)
		}
	}
}
