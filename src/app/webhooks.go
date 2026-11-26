package app

import (
	"errors"
	"regexp"
	"strings"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// channel.webhook.register.ask (ut-docs#7084, contract v1 additive). The host
// gives a callback address and the topics the manifest declares; the adapter
// ensures the subscriptions it can create through the Admin API (the three
// product topics) and says "unsupported" for the rest (the GDPR topics are
// app configuration; app/uninstalled and orders/* are not registered yet).
//
// Rules, each pinned by webhook_register_7084_test.go:
//   - idempotent: a list first; the same address present = "already"; a
//     subscription of OURS (callback path under /hooks/channel/) at another
//     address is repointed in place = "updated"; absent = created.
//   - a subscription that is not ours (another path, another endpoint kind) is
//     never updated; and nothing is ever deleted (CLAUDE.md), so mode "remove"
//     is answered "unsupported": Shopify drops an app's subscriptions when the
//     app is uninstalled.
//   - the callback address (it holds the connection's opaque ref) is never
//     logged or answered; failures are stable codes (scope_missing,
//     topic_not_allowed, throttled, shopify_error), never Shopify's text.

// registrable maps the manifest's topic to Shopify's enum value.
var registrable = map[string]string{
	"products/create": "PRODUCTS_CREATE",
	"products/update": "PRODUCTS_UPDATE",
	"products/delete": "PRODUCTS_DELETE",
}

const (
	hookPathPrefix = "/hooks/channel/"
	maxRegTopics   = 24
	// webhookInclude is the least a doorbell needs: the id and the update
	// time. The product is re-read anyway.
	webhookListFirst = 100
)

var webhookInclude = []string{"id", "admin_graphql_api_id", "updated_at"}

// callbackRe is the shape of the host's hook address: https, a plain host, no
// userinfo, query or fragment, and a path under /hooks/channel/ of non-empty
// segments of plain characters.
var callbackRe = regexp.MustCompile(`^https:[/][/][A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?(/hooks/channel/[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*)$`)

func okCallback(u string) (path string, ok bool) {
	if len(u) > 512 {
		return "", false
	}
	m := callbackRe.FindStringSubmatch(u)
	if m == nil {
		return "", false
	}
	for _, seg := range strings.Split(m[3], "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	return m[3], true
}

// sameHookOwner says a subscription's address is ours to repoint: the same
// host and the same plugin segment under /hooks/channel/ as the new address
// (a rotation changes only the connection ref). Another cloud's hook address
// (staging beside prod) is never taken over.
func sameHookOwner(old, url string) bool {
	op, ok := okCallback(old)
	if !ok {
		return false
	}
	np, ok := okCallback(url)
	if !ok {
		return false
	}
	host := func(u, path string) string { return strings.ToLower(strings.TrimSuffix(u, path)) }
	seg := func(p string) string { return strings.SplitN(strings.TrimPrefix(p, hookPathPrefix), "/", 2)[0] }
	return host(old, op) == host(url, np) && seg(op) == seg(np)
}

type whSub struct {
	ID       string `json:"id"`
	Topic    string `json:"topic"`
	Endpoint struct {
		Callback string `json:"callbackUrl"`
	} `json:"endpoint"`
}

type whUserError struct {
	Field   []string `json:"field"`
	Message string   `json:"message"`
}

type whPayload struct {
	Sub *struct {
		ID string `json:"id"`
	} `json:"webhookSubscription"`
	UserErrors []whUserError `json:"userErrors"`
}

func webhookRegister(ev channel.Event) (any, error) {
	var in channel.WebhookRegisterInput
	if err := ev.Decode(&in); err != nil {
		return nil, errors.New("register input is not valid JSON")
	}
	if in.Mode != channel.WebhookModeEnsure && in.Mode != channel.WebhookModeRemove {
		return nil, errors.New("register mode is not ensure or remove")
	}
	if _, ok := okCallback(in.CallbackURL); !ok {
		return nil, errors.New("the callback address is not a hook address of this host")
	}
	if len(in.Topics) > maxRegTopics {
		return nil, errors.New("too many topics")
	}
	res := channel.WebhookRegisterResult{Topics: map[string]channel.TopicState{}}
	var want []string // registrable topics asked, in order
	for _, t := range in.Topics {
		if _, dup := res.Topics[t]; dup {
			continue
		}
		if _, ok := registrable[t]; !ok {
			res.Topics[t] = channel.TopicState{State: channel.TopicUnsupported}
			continue
		}
		if in.Mode == channel.WebhookModeRemove {
			res.Topics[t] = channel.TopicState{State: channel.TopicUnsupported, Code: "remove_not_supported"}
			continue
		}
		res.Topics[t] = channel.TopicState{State: channel.TopicFailed} // until proven
		want = append(want, t)
	}
	if len(want) == 0 {
		return res, nil
	}
	enums := make([]string, 0, len(want))
	for _, t := range want {
		enums = append(enums, registrable[t])
	}
	var d struct {
		Subs struct {
			Nodes []whSub `json:"nodes"`
		} `json:"webhookSubscriptions"`
	}
	if err := run(qWebhookList, map[string]any{"first": webhookListFirst, "topics": enums}, "", &d); err != nil {
		code := regCode(err)
		logErr("webhook.list_failed", err)
		for _, t := range want {
			res.Topics[t] = channel.TopicState{State: channel.TopicFailed, Code: code}
		}
		return res, nil
	}
	for _, t := range want {
		res.Topics[t] = ensureTopic(t, in.CallbackURL, d.Subs.Nodes)
	}
	hostLog(channel.LevelInfo, "webhook.ensure", map[string]any{"topics": len(want)})
	return res, nil
}

// ensureTopic makes one topic's subscription point at url.
func ensureTopic(topic, url string, subs []whSub) channel.TopicState {
	enum := registrable[topic]
	var ours *whSub
	for i := range subs {
		s := &subs[i]
		if s.Topic != enum || s.Endpoint.Callback == "" {
			continue
		}
		if s.Endpoint.Callback == url {
			return channel.TopicState{State: channel.TopicAlready}
		}
		if ours == nil && sameHookOwner(s.Endpoint.Callback, url) && okGID(tWebhook, s.ID) {
			ours = s
		}
	}
	input := func(withFields bool) map[string]any {
		m := map[string]any{"callbackUrl": url, "format": "JSON"}
		if withFields {
			m["includeFields"] = webhookInclude
		}
		return m
	}
	for _, withFields := range []bool{true, false} {
		var out struct {
			Create *whPayload `json:"webhookSubscriptionCreate"`
			Update *whPayload `json:"webhookSubscriptionUpdate"`
		}
		var err error
		if ours != nil {
			err = run(qWebhookUpdate, map[string]any{"id": ours.ID, "sub": input(withFields)}, "", &out)
		} else {
			err = run(qWebhookCreate, map[string]any{"topic": enum, "sub": input(withFields)}, "", &out)
		}
		if err != nil {
			logErr("webhook.write_failed", err)
			return channel.TopicState{State: channel.TopicFailed, Code: regCode(err)}
		}
		p := out.Create
		done := channel.TopicRegistered
		if ours != nil {
			p, done = out.Update, channel.TopicUpdated
		}
		if p == nil {
			return channel.TopicState{State: channel.TopicFailed, Code: "shopify_error"}
		}
		if len(p.UserErrors) == 0 {
			return channel.TopicState{State: done}
		}
		// Only a code leaves this function: the message can echo the address.
		if withFields && userErrorOn(p.UserErrors, "includeFields") {
			continue // retry without includeFields: the id-keyed dedupe makes both safe
		}
		if ours == nil && userErrorOn(p.UserErrors, "callbackUrl") && strings.Contains(strings.ToLower(p.UserErrors[0].Message), "taken") {
			return channel.TopicState{State: channel.TopicAlready}
		}
		code := "shopify_error"
		if userErrorOn(p.UserErrors, "topic") {
			code = "topic_not_allowed"
		}
		hostLog(channel.LevelWarn, "webhook.refused", map[string]any{"code": code})
		return channel.TopicState{State: channel.TopicFailed, Code: code}
	}
	return channel.TopicState{State: channel.TopicFailed, Code: "shopify_error"}
}

func userErrorOn(errs []whUserError, field string) bool {
	for _, e := range errs {
		for _, f := range e.Field {
			if f == field {
				return true
			}
		}
	}
	return false
}

// regCode turns an error into one of the stable registration codes.
func regCode(err error) string {
	var ge *gqlError
	switch {
	case errors.Is(err, errThrottled):
		return "throttled"
	case errors.As(err, &ge) && ge.Code == "ACCESS_DENIED":
		return "scope_missing"
	}
	return "shopify_error"
}
