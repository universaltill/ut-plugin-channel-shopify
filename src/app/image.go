package app

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// image.upsert (ADR-0212, contract 8.2, ut-docs#6843 slice S5).
//
// The cloud sends {cloud_id, external_id, sha256, content_type, fetch_url, alt?}.
// fetch_url is a signed, at most 15 minute URL on the cloud: the plugin never
// fetches it and never sees bytes, it hands it to Shopify as the originalSource
// of productCreateMedia. No egress host is added for it: only Shopify fetches.
// The URL is a secret for its lifetime: it is never logged and never put in an
// acknowledgement.
//
// Idempotency: the sha256 of the image last pushed is kept in the product
// metafield unitill.image_sha256. The op is skipped when the metafield equals
// the wanted sha AND the media id we pushed (metafield unitill.image_media_id)
// is still on the product (among its first 10 media) and has not FAILED. Only
// that media is ever acknowledged: an image the merchant put first is never
// claimed as ours (ADR-0212). A merchant who deleted our image, or a fetch
// Shopify could not make, is repaired by the next push. Otherwise the media is created, then the
// metafield set, then the new media moved to position 0 (primary). Old media
// are never deleted (ADR-0169 section 7).
//
// The metafield and reorder steps are best effort and logged: the image is on
// the product once the create is accepted, and failing the op after that would
// retry into a duplicate. The cost is that a failed metafield write lets a
// later identical push add the same image again.

const (
	imgNamespace = "unitill"
	imgKey       = "image_sha256"
	imgMediaKey  = "image_media_id"
	maxAltRunes  = 512
	maxFetchURL  = 2048
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true}

// okImageURL: an absolute https URL with a host, no userinfo, of sane length.
func okImageURL(raw string) bool {
	if raw == "" || len(raw) > maxFetchURL {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Hostname() != ""
}

func imageUpsert(op channel.OutboxOp) channel.Ack {
	var p struct {
		ExternalID  string          `json:"external_id"`
		SHA256      json.RawMessage `json:"sha256"`
		ContentType json.RawMessage `json:"content_type"`
		FetchURL    json.RawMessage `json:"fetch_url"`
		Alt         string          `json:"alt"`
	}
	// A payload with none of the image keys is the pre-ADR-0212 shape (or an
	// operation of another kind): not something this adapter does.
	if len(op.Payload) == 0 || json.Unmarshal(op.Payload, &p) != nil && !json.Valid(op.Payload) ||
		(p.SHA256 == nil && p.ContentType == nil && p.FetchURL == nil) {
		hostLog(channel.LevelWarn, "outbox.unsupported_op", map[string]any{"kind": clip(op.Kind, 40)})
		return fail("unsupported_op", false)
	}
	var sha, ctype, src string
	if json.Unmarshal(op.Payload, &p) != nil ||
		json.Unmarshal(p.SHA256, &sha) != nil || json.Unmarshal(p.ContentType, &ctype) != nil || json.Unmarshal(p.FetchURL, &src) != nil ||
		!shaRe.MatchString(sha) || !imageTypes[ctype] || !okImageURL(src) ||
		!okGID(tVariant, op.ExternalID) || (p.ExternalID != "" && p.ExternalID != op.ExternalID) ||
		len([]rune(p.Alt)) > maxAltRunes {
		hostLog(channel.LevelWarn, "image.invalid_payload", nil)
		return fail("invalid_payload", false)
	}
	cur, bad := readVariant(op.ExternalID)
	if bad != nil {
		return *bad
	}
	pid := cur.Variant.Product.ID

	var held struct {
		Product *struct {
			Metafield *struct {
				Value string `json:"value"`
			} `json:"metafield"`
			MediaRef *struct {
				Value string `json:"value"`
			} `json:"mediaRef"`
			Media struct {
				Nodes []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"nodes"`
			} `json:"media"`
		} `json:"product"`
	}
	if err := run(qProductImage, map[string]any{"id": pid, "ns": imgNamespace, "key": imgKey, "mkey": imgMediaKey}, "", &held); err != nil {
		return failFor(err)
	}
	if held.Product == nil {
		return fail("not_found", false)
	}
	ours := ""
	if mr := held.Product.MediaRef; mr != nil && mr.Value != "" {
		for _, n := range held.Product.Media.Nodes {
			if n.ID == mr.Value && strings.ToUpper(n.Status) != "FAILED" {
				ours = n.ID
			}
		}
	}
	if mf := held.Product.Metafield; mf != nil && mf.Value == sha && ours != "" {
		hostLog(channel.LevelInfo, "image.skipped_same_sha", nil)
		return channel.Ack{ExternalID: ours}
	}

	var created struct {
		Create struct {
			Media []struct {
				ID string `json:"id"`
			} `json:"media"`
			Errors []struct{} `json:"mediaUserErrors"`
		} `json:"productCreateMedia"`
	}
	in := map[string]any{"originalSource": src, "mediaContentType": "IMAGE"}
	if p.Alt != "" {
		in["alt"] = p.Alt
	}
	// Not safe to repeat: no idempotency key, so a lost answer is not re-sent by the host.
	if err := run(qProductCreateMedia, map[string]any{"productId": pid, "media": []any{in}}, "", &created); err != nil {
		return failFor(err)
	}
	if len(created.Create.Errors) > 0 || len(created.Create.Media) != 1 || created.Create.Media[0].ID == "" {
		hostLog(channel.LevelWarn, "image.media_rejected", nil)
		return fail("rejected_by_channel", false)
	}
	mid := created.Create.Media[0].ID

	var set struct {
		Set struct {
			Errors []struct{} `json:"userErrors"`
		} `json:"metafieldsSet"`
	}
	mf := []any{
		map[string]any{"ownerId": pid, "namespace": imgNamespace, "key": imgKey, "type": "single_line_text_field", "value": sha},
		map[string]any{"ownerId": pid, "namespace": imgNamespace, "key": imgMediaKey, "type": "single_line_text_field", "value": mid},
	}
	if err := run(qMetafieldsSet, map[string]any{"metafields": mf}, op.ID+":sha", &set); err != nil || len(set.Set.Errors) > 0 {
		hostLog(channel.LevelWarn, "image.sha_not_stored", nil)
	}
	if len(held.Product.Media.Nodes) > 0 { // something is there: ours must go first
		var re struct {
			Reorder struct {
				Errors []struct{} `json:"mediaUserErrors"`
			} `json:"productReorderMedia"`
		}
		moves := []any{map[string]any{"id": mid, "newPosition": "0"}}
		if err := run(qProductReorderMedia, map[string]any{"id": pid, "moves": moves}, op.ID+":move", &re); err != nil || len(re.Reorder.Errors) > 0 {
			hostLog(channel.LevelWarn, "image.not_made_primary", nil)
		}
	}
	return channel.Ack{ExternalID: mid}
}
