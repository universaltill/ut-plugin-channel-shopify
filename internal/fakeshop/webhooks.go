package fakeshop

import (
	"strings"
)

// Webhook subscriptions and the product read by id (ut-docs#7084). The fake
// models the part of the Admin API 2026-10 the adapter uses: only
// subscriptions created by this app are listed, a second subscription for the
// same topic and address is a userError ("has already been taken"), and the
// callback URL of a subscription is only reported for an HTTP endpoint.

type subscription struct {
	ID, Topic, URL string
	Include        []string
	NoHTTP         bool // another endpoint kind (no callbackUrl)
}

// Subscription is one webhook subscription as the tests see it.
type Subscription struct {
	ID, Topic, URL string
	IncludeFields  []string
}

// Subscriptions lists what the shop holds, in creation order.
func (s *Shop) Subscriptions() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Subscription
	for _, x := range s.subs {
		out = append(out, Subscription{ID: x.ID, Topic: x.Topic, URL: x.URL, IncludeFields: x.Include})
	}
	return out
}

// AddSubscription plants a subscription (a hand-made or foreign one) and
// returns its id. An empty url makes it a non-HTTP endpoint.
func (s *Shop) AddSubscription(topic, url string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subSeq++
	id := gid("WebhookSubscription", 9000+s.subSeq)
	s.subs = append(s.subs, subscription{ID: id, Topic: topic, URL: url, NoHTTP: url == ""})
	return id
}

// RemoveSubscriptions drops every subscription (Shopify deleting a failing one).
func (s *Shop) RemoveSubscriptions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = nil
}

// RefuseIncludeFields makes create and update answer a userError on includeFields.
func (s *Shop) RefuseIncludeFields(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseInclude = on
}

// RefuseTopic makes create answer a userError on the topic field, once.
func (s *Shop) RefuseTopic() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subUserErr = "topic"
}

func (s *Shop) opProductsByID(req gqlRequest) obj {
	var ids []string
	s.vars(req, "ids", &ids)
	shop := obj{"currencyCode": s.opt.Currency, "taxesIncluded": !s.opt.TaxesExcluded}
	nodes := make([]any, 0, len(ids))
	for _, id := range ids {
		var found any
		for _, p := range s.products {
			if p.ID == id {
				found = obj{"id": p.ID, "title": p.Title, "description": p.Desc, "descriptionHtml": htmlOf(p), "status": p.Status, "updatedAt": p.UpdatedAt,
					"productType": p.ProductType, "category": taxonomyJSON(p), "collections": collectionsJSON(p), "featuredMedia": featuredJSON(p),
					"variants": s.variantPage(p, s.num(req, "vfirst", 50), "")}
			}
		}
		nodes = append(nodes, found) // nil = gone or no access, as Shopify answers
	}
	return obj{"shop": shop, "nodes": nodes}
}

func (s *Shop) opWebhookList(req gqlRequest) obj {
	var topics []string
	s.vars(req, "topics", &topics)
	want := map[string]bool{}
	for _, t := range topics {
		want[t] = true
	}
	nodes := []obj{}
	for _, x := range s.subs {
		if len(want) > 0 && !want[x.Topic] {
			continue
		}
		ep := obj{"__typename": "WebhookEventBridgeEndpoint"}
		if !x.NoHTTP {
			ep = obj{"__typename": "WebhookHttpEndpoint", "callbackUrl": x.URL}
		}
		nodes = append(nodes, obj{"id": x.ID, "topic": x.Topic, "endpoint": ep})
	}
	return obj{"webhookSubscriptions": obj{"nodes": nodes}}
}

type subInput struct {
	CallbackURL   string   `json:"callbackUrl"`
	Format        string   `json:"format"`
	IncludeFields []string `json:"includeFields"`
}

func whUserErr(field, msg string) obj {
	return obj{"userErrors": []obj{{"field": strings.Split(field, "."), "message": msg}}, "webhookSubscription": nil}
}

func (s *Shop) opWebhookCreate(req gqlRequest) obj {
	topic := s.str(req, "topic")
	var in subInput
	s.vars(req, "sub", &in)
	key := "webhookSubscriptionCreate"
	if s.subUserErr == "topic" {
		s.subUserErr = ""
		return obj{key: whUserErr("topic", "Topic is not allowed")}
	}
	if s.refuseInclude && len(in.IncludeFields) > 0 {
		return obj{key: whUserErr("webhookSubscription.includeFields", "Include fields is invalid")}
	}
	if !strings.HasPrefix(in.CallbackURL, "https://") {
		return obj{key: whUserErr("webhookSubscription.callbackUrl", "Address is invalid")}
	}
	for _, x := range s.subs {
		if x.Topic == topic && x.URL == in.CallbackURL {
			return obj{key: whUserErr("webhookSubscription.callbackUrl", "Address for this topic has already been taken")}
		}
	}
	s.subSeq++
	id := gid("WebhookSubscription", 9000+s.subSeq)
	s.subs = append(s.subs, subscription{ID: id, Topic: topic, URL: in.CallbackURL, Include: in.IncludeFields})
	return obj{key: obj{"webhookSubscription": obj{"id": id}, "userErrors": []obj{}}}
}

func (s *Shop) opWebhookUpdate(req gqlRequest) obj {
	id := s.str(req, "id")
	var in subInput
	s.vars(req, "sub", &in)
	key := "webhookSubscriptionUpdate"
	if s.refuseInclude && len(in.IncludeFields) > 0 {
		return obj{key: whUserErr("webhookSubscription.includeFields", "Include fields is invalid")}
	}
	for i := range s.subs {
		if s.subs[i].ID == id {
			s.subs[i].URL, s.subs[i].Include, s.subs[i].NoHTTP = in.CallbackURL, in.IncludeFields, false
			return obj{key: obj{"webhookSubscription": obj{"id": id}, "userErrors": []obj{}}}
		}
	}
	return obj{key: whUserErr("id", "Webhook subscription does not exist")}
}
