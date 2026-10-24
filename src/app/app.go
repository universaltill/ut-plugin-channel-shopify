package app

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Handlers is the adapter's whole surface: one handler per ask the manifest's
// capabilities need (contract section 4).
func Handlers() channel.Handlers {
	return budgetedHandlers(channel.Handlers{
		channel.AskConnectTest:   connectTest,
		channel.AskOAuthCallback: oauthCallback,
		channel.AskWebhook:       webhook,
		channel.AskContact:       contact,
		channel.AskFetch:         fetch,
		channel.AskImport:        importCatalogue,
		channel.AskOutbox:        drainOutbox,
		channel.AskTick:          tick,
	})
}

// connectTest is read-only: it asks the shop who it is and counts, and
// writes nothing anywhere (conformance check 1).
func connectTest(channel.Event) (any, error) {
	var d struct {
		Shop struct {
			Name string `json:"name"`
		} `json:"shop"`
		Install struct {
			Scopes []struct {
				Handle string `json:"handle"`
			} `json:"accessScopes"`
		} `json:"currentAppInstallation"`
		Locations struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"locations"`
		Products struct {
			Count int64 `json:"count"`
		} `json:"productsCount"`
		Orders struct {
			Count int64 `json:"count"`
		} `json:"ordersCount"`
	}
	if err := run(qConnectTest, nil, "", &d); err != nil {
		return nil, err
	}
	out := channel.ConnectTestResult{AccountName: clip(d.Shop.Name, 100), Products: d.Products.Count, Orders: d.Orders.Count}
	for _, s := range d.Install.Scopes {
		out.Scopes = append(out.Scopes, clip(s.Handle, 100))
	}
	for _, l := range d.Locations.Nodes {
		out.Locations = append(out.Locations, clip(l.Name, 100))
	}
	return out, nil
}

// oauthCallback parses the parameters of an install request the HOST has
// already verified (signature, state, freshness) and whose code the host has
// already exchanged. It checks the shop domain once more and returns it.
func oauthCallback(ev channel.Event) (any, error) {
	var in channel.OAuthCallbackInput
	if err := ev.Decode(&in); err != nil {
		return nil, errors.New("callback parameters are not valid JSON")
	}
	shop := in.Params["shop"]
	if !accountRe.MatchString(shop) {
		return nil, errors.New("the shop is not an account of this channel")
	}
	out := channel.OAuthCallbackResult{Account: shop}
	for _, s := range strings.FieldsFunc(in.Params["scope"], func(r rune) bool { return r == ',' || r == ' ' }) {
		if len(s) > 100 {
			return nil, errors.New("a scope is too long")
		}
		out.Scopes = append(out.Scopes, s)
	}
	return out, nil
}

var webhookIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// header reads a header case-insensitively.
func header(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// webhook parses a webhook the host has verified. It has no tenant context:
// it makes no host call but Log, and says only what happened and to what. The
// body of an order webhook carries the buyer's details; the one field read
// from it is the order's global id. The body of a GDPR topic is not read at
// all.
func webhook(ev channel.Event) (any, error) {
	var in channel.WebhookInput
	if err := ev.Decode(&in); err != nil {
		return nil, errors.New("webhook input is not valid JSON")
	}
	shop, topic, id := header(in.Headers, "X-Shopify-Shop-Domain"), header(in.Headers, "X-Shopify-Topic"), header(in.Headers, "X-Shopify-Webhook-Id")
	if !accountRe.MatchString(shop) || topic == "" || len(topic) > 64 || !webhookIDRe.MatchString(id) {
		return nil, errors.New("not a webhook of this channel")
	}
	out := channel.WebhookResult{Account: shop, DedupeID: id, Event: webhookEvent(topic)}
	if out.Event == channel.EventOrderChanged {
		var b struct {
			ID string `json:"admin_graphql_api_id"`
		}
		if err := json.Unmarshal(in.Body, &b); err != nil || !okGID(tOrder, b.ID) {
			return nil, errors.New("body is not an order webhook of this channel")
		}
		out.ResourceIDs = []string{b.ID}
	}
	return out, nil
}

// webhookEvent maps Shopify's topic to the neutral event; the host never acts
// on a channel's own topic names.
func webhookEvent(topic string) string {
	switch topic {
	case "orders/create", "orders/updated":
		return channel.EventOrderChanged
	case "app/uninstalled":
		return channel.EventUninstalled
	case "customers/redact":
		return channel.EventCustomerRedact
	case "shop/redact":
		return channel.EventShopRedact
	case "customers/data_request":
		return channel.EventDataRequest
	}
	return channel.EventOther
}

// contact fetches an order's customer details live, for display only. The
// answer is never stored, submitted or logged.
func contact(ev channel.Event) (any, error) {
	var in channel.ContactInput
	if err := ev.Decode(&in); err != nil || !okGID(tOrder, in.OrderExternalID) {
		return nil, errors.New("not a valid order")
	}
	var d struct {
		Order *struct {
			Email   string `json:"email"`
			Phone   string `json:"phone"`
			Address *struct {
				Name     string `json:"name"`
				Address1 string `json:"address1"`
				Address2 string `json:"address2"`
				City     string `json:"city"`
				Province string `json:"province"`
				Zip      string `json:"zip"`
				Country  string `json:"country"`
			} `json:"shippingAddress"`
		} `json:"order"`
	}
	if err := run(qContact, map[string]any{"id": in.OrderExternalID}, "", &d); err != nil {
		return nil, err
	}
	if d.Order == nil {
		return nil, errors.New("no such order")
	}
	o := d.Order
	res := channel.ContactResult{Email: clip(o.Email, 200), Phone: clip(o.Phone, 50)}
	if a := o.Address; a != nil {
		res.Name = clip(a.Name, 200)
		var parts []string
		for _, p := range []string{a.Address1, a.Address2, a.City, a.Province, a.Zip, a.Country} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		res.Address = clip(strings.Join(parts, "\n"), 500)
	}
	return res, nil
}
