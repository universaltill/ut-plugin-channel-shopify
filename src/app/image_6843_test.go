//go:build !wasip1

package app_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
)

// ADR-0212, contract 8.2 `image.upsert` (Shopify-finish slice S5): the plugin
// hands the signed fetch_url to Shopify (productCreateMedia originalSource) and
// never sees bytes. Idempotent by the sha256 it stores on the product.

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func imageOp(id, variant, sha, ctype, url, alt string) channel.OutboxOp {
	m := map[string]any{"cloud_id": "c", "external_id": variant, "sha256": sha, "content_type": ctype, "fetch_url": url}
	if alt != "" {
		m["alt"] = alt
	}
	b, _ := json.Marshal(m)
	return channel.OutboxOp{ID: id, Kind: channel.OpImageUpsert, ExternalID: variant, CloudID: "c", Payload: b}
}

func urlFor(sha string, n int) string {
	return fmt.Sprintf("https://cloud.example.test/channel/img/ref1?sha=%s&exp=%d&sig=SECRETSIG%d", sha, n, n)
}

func TestImageUpsertAddsTheMediaFromTheFetchURLAsPrimary(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	u := urlFor(shaA, 1)
	a := e.outbox(imageOp("i1", v, shaA, "image/png", u, "Invented item 3"))["i1"]
	if a.Error != nil || a.ExternalID == "" {
		t.Fatalf("ack %+v", a)
	}
	m := e.shop.Media(p)
	if len(m) != 1 || m[0].Source != u || m[0].Alt != "Invented item 3" || m[0].ID != a.ExternalID {
		t.Fatalf("media %+v ack %+v", m, a)
	}
	if got := e.shop.ImageSHA(p); got != shaA {
		t.Errorf("stored sha %q", got)
	}
	// the signed URL goes to Shopify only: not back in the ack
	if b, _ := json.Marshal(a); strings.Contains(string(b), "SECRETSIG") {
		t.Errorf("ack carries the URL: %s", b)
	}
}

func TestImageUpsertSameShaIsSkippedEvenWithAFreshURL(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	first := e.outbox(imageOp("i1", v, shaA, "image/png", urlFor(shaA, 1), ""))["i1"]
	w := e.shop.Writes()
	a := e.outbox(imageOp("i2", v, shaA, "image/png", urlFor(shaA, 2), ""))["i2"]
	if a.Error != nil || a.ExternalID != first.ExternalID {
		t.Fatalf("ack %+v first %+v", a, first)
	}
	if e.shop.Writes() != w || len(e.shop.Media(p)) != 1 {
		t.Errorf("writes %d -> %d, media %d", w, e.shop.Writes(), len(e.shop.Media(p)))
	}
}

func TestImageUpsertNewShaReplacesThePrimaryAndDeletesNothing(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.outbox(imageOp("i1", v, shaA, "image/png", urlFor(shaA, 1), ""))
	a := e.outbox(imageOp("i2", v, shaB, "image/jpeg", urlFor(shaB, 2), ""))["i2"]
	if a.Error != nil {
		t.Fatalf("%+v", a)
	}
	m := e.shop.Media(p)
	if len(m) != 2 || m[0].ID != a.ExternalID || m[0].Source != urlFor(shaB, 2) {
		t.Fatalf("new image must be first and the old one kept: %+v", m)
	}
	if e.shop.ImageSHA(p) != shaB {
		t.Errorf("sha %q", e.shop.ImageSHA(p))
	}
}

func TestImageUpsertOverAMerchantsOwnImageAddsOursFirst(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.shop.SetImage(p, "https://cdn.shopify.com/s/files/1/own.png", "mine")
	a := e.outbox(imageOp("i1", v, shaA, "image/png", urlFor(shaA, 1), ""))["i1"]
	m := e.shop.Media(p)
	if a.Error != nil || len(m) != 2 || m[0].ID != a.ExternalID || m[1].Source != "https://cdn.shopify.com/s/files/1/own.png" {
		t.Fatalf("%+v %+v", a, m)
	}
}

func TestImageUpsertRefusesBadPayloadsBeforeAnyRequest(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	good := urlFor(shaA, 1)
	cases := map[string]channel.OutboxOp{
		"short sha":     imageOp("b1", v, "abc", "image/png", good, ""),
		"upper sha":     imageOp("b2", v, strings.ToUpper(shaA), "image/png", good, ""),
		"bad type":      imageOp("b3", v, shaA, "image/svg+xml", good, ""),
		"http":          imageOp("b4", v, shaA, "image/png", "http://cloud.example.test/x", ""),
		"userinfo":      imageOp("b5", v, shaA, "image/png", "https://u:p@cloud.example.test/x", ""),
		"no host":       imageOp("b6", v, shaA, "image/png", "https:///x", ""),
		"empty url":     imageOp("b7", v, shaA, "image/png", "", ""),
		"file url":      imageOp("b8", v, shaA, "image/png", "file:///etc/passwd", ""),
		"huge url":      imageOp("b9", v, shaA, "image/png", "https://cloud.example.test/"+strings.Repeat("a", 2100), ""),
		"alt too long":  imageOp("b10", v, shaA, "image/png", good, strings.Repeat("x", 513)),
		"not a variant": imageOp("b11", "gid://shopify/Order/1", shaA, "image/png", good, ""),
		"no id":         imageOp("b12", "", shaA, "image/png", good, ""),
		"not json":      {ID: "b13", Kind: channel.OpImageUpsert, ExternalID: v, Payload: json.RawMessage(`{"sha256":5,"fetch_url":"x","content_type":"image/png"}`)},
		"half payload":  {ID: "b14", Kind: channel.OpImageUpsert, ExternalID: v, Payload: json.RawMessage(`{"sha256":"` + shaA + `"}`)},
	}
	for name, op := range cases {
		a := e.outbox(op)[op.ID]
		if a.Error == nil || a.Error.Code != "invalid_payload" || a.Error.Retryable {
			t.Errorf("%s: %+v", name, a)
		}
	}
	if n := len(e.shop.Requests()); n != 0 {
		t.Errorf("a refused payload made %d requests", n)
	}
}

func TestImageUpsertOldShapedPayloadIsUnsupported(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	for i, body := range []string{``, `{}`, `{"cloud_id":"c","external_id":"` + v + `"}`, `{"images":[{"url":"https://x.example.test/a.png"}]}`} {
		op := channel.OutboxOp{ID: fmt.Sprintf("o%d", i), Kind: channel.OpImageUpsert, ExternalID: v, Payload: json.RawMessage(body)}
		if body == "" {
			op.Payload = nil
		}
		if a := e.outbox(op)[op.ID]; a.Error == nil || a.Error.Code != "unsupported_op" || a.Error.Retryable {
			t.Errorf("%q: %+v", body, a)
		}
	}
	if len(e.shop.Requests()) != 0 {
		t.Error("made a request")
	}
}

func TestImageUpsertUnknownVariantAndShopifyRefusals(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	if a := e.outbox(imageOp("n1", "gid://shopify/ProductVariant/999999", shaA, "image/png", urlFor(shaA, 1), ""))["n1"]; a.Error == nil || a.Error.Code != "not_found" {
		t.Errorf("unknown variant %+v", a)
	}
	e.shop.RefuseMedia()
	a := e.outbox(imageOp("n2", v, shaA, "image/png", urlFor(shaA, 1), ""))["n2"]
	if a.Error == nil || a.Error.Code != "rejected_by_channel" || a.Error.Retryable {
		t.Errorf("refused media %+v", a)
	}
}

func TestImageUpsertSchemaErrorIsClassifiedNotRetriedForever(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	_, v := singleProduct(t, e)
	e.shop.SchemaError("ProductCreateMedia")
	if a := e.outbox(imageOp("s1", v, shaA, "image/png", urlFor(shaA, 1), ""))["s1"]; a.Error == nil || a.Error.Code != "rejected_by_channel" {
		t.Errorf("%+v", a)
	}
}

func TestImportReadsOnlyTheFirstImageURL(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	ids := e.shop.ProductIDs()
	e.shop.SetImage(ids[2], "https://cdn.shopify.com/s/files/1/first.png", "")
	e.shop.AddImage(ids[2], "https://cdn.shopify.com/s/files/1/second.png", "")
	ls := importedListings(e)
	l := ls[ids[2]]
	if len(l.Images) != 1 || l.Images[0].URL != "https://cdn.shopify.com/s/files/1/first.png" || l.Images[0].Position != 1 || l.Images[0].ExternalID == "" {
		t.Fatalf("images %+v", l.Images)
	}
	if len(ls[ids[0]].Images) != 0 {
		t.Errorf("a product without an image got %+v", ls[ids[0]].Images)
	}
}

func TestImportDropsAnImageURLThatIsNotHTTPS(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	ids := e.shop.ProductIDs()
	e.shop.SetImage(ids[2], "http://cdn.shopify.com/x.png", "")
	if l := importedListings(e)[ids[2]]; len(l.Images) != 0 {
		t.Errorf("%+v", l.Images)
	}
}

func TestImageUpsertRepairsAFailedOrDeletedImageWithTheSameSha(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	e.outbox(imageOp("i1", v, shaA, "image/png", urlFor(shaA, 1), ""))
	e.shop.SetMediaStatus(p, "FAILED") // Shopify could not fetch it
	a := e.outbox(imageOp("i2", v, shaA, "image/png", urlFor(shaA, 2), ""))["i2"]
	if a.Error != nil || len(e.shop.Media(p)) != 2 || e.shop.Media(p)[0].Source != urlFor(shaA, 2) {
		t.Fatalf("%+v %+v", a, e.shop.Media(p))
	}
}

// Review fix: the same-sha skip claims a media only when it is the one we pushed.
func TestImageUpsertSameShaWithAMerchantImageInFrontAcksOursNotTheirs(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	ours := e.outbox(imageOp("i1", v, shaA, "image/png", urlFor(shaA, 1), ""))["i1"]
	e.shop.PrependImage(p, "https://cdn.shopify.com/s/files/1/own.png", "mine")
	w := e.shop.Writes()
	a := e.outbox(imageOp("i2", v, shaA, "image/png", urlFor(shaA, 2), ""))["i2"]
	if a.Error != nil || a.ExternalID != ours.ExternalID {
		t.Fatalf("must ack the media we pushed (%s), got %+v", ours.ExternalID, a)
	}
	if e.shop.Writes() != w || len(e.shop.Media(p)) != 2 {
		t.Errorf("a media of ours is still there: nothing to push; writes %d -> %d, media %d", w, e.shop.Writes(), len(e.shop.Media(p)))
	}
}

func TestImageUpsertSameShaButOursGoneRepushesAndNeverClaimsTheMerchants(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	p, v := singleProduct(t, e)
	first := e.outbox(imageOp("i1", v, shaA, "image/png", urlFor(shaA, 1), ""))["i1"]
	e.shop.SetImage(p, "https://cdn.shopify.com/s/files/1/own.png", "mine") // our media deleted, theirs is first
	own := e.shop.Media(p)[0].ID
	a := e.outbox(imageOp("i2", v, shaA, "image/png", urlFor(shaA, 2), ""))["i2"]
	if a.Error != nil || a.ExternalID == own || a.ExternalID == first.ExternalID || a.ExternalID == "" {
		t.Fatalf("ack %+v (merchant's %s, old %s)", a, own, first.ExternalID)
	}
	if m := e.shop.Media(p); len(m) != 2 || m[0].ID != a.ExternalID {
		t.Fatalf("media %+v", m)
	}
}
