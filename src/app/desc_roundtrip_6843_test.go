//go:build !wasip1

package app_test

import (
	"testing"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
	"github.com/universaltill/ut-plugin-channel-shopify/src/app"
)

// Found on the dev store 2026-11-23 (ut-docs#6843, live run): Shopify's
// Product.description (plain text) glues paragraphs together ("<p>a</p><p>b</p>"
// reads "ab"), so reading it made a description pushed as two paragraphs come
// back changed and be rewritten on every sweep. The adapter reads descriptionHtml.

func TestAMultiParagraphDescriptionRoundTripsAndIsNotRewritten(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetDescription(p, "Old text")
	changed := "{" + ch("description", "Old text", earlier) + "}"
	if a := e.outbox(fieldsOp("r1", v, "Line one\nLine two", "", nil, changed))["r1"]; a.Error != nil {
		t.Fatalf("ack %+v", a)
	}
	if got := e.shop.DescriptionHTML(p); got != "<p>Line one</p><p>Line two</p>" {
		t.Fatalf("descriptionHtml %q", got)
	}
	// inbound: the import reads what Shopify now holds and gets the same words
	l := importedListings(e)[p]
	if got, want := l.Description, app.NormalizeText("Line one\nLine two"); got != want {
		t.Errorf("import description %q, want %q (paragraphs glued)", got, want)
	}
	// outbound again: the same item text is already there, so nothing is written
	w := e.shop.Writes()
	if a := e.outbox(fieldsOp("r2", v, "Line one\nLine two", "", nil, changed))["r2"]; a.Error != nil || e.shop.Writes() != w {
		t.Errorf("identical push rewrote the description: ack %+v, writes %d -> %d", a, w, e.shop.Writes())
	}
}
