//go:build !wasip1

package app_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"

	"github.com/universaltill/ut-plugin-channel-shopify/internal/fakeshop"
)

// ut-docs#7084: channel.webhook.register.ask. Idempotent, level-triggered by
// the host, never touches a subscription that is not ours.

const cb = "https://cloud.example.invalid/hooks/channel/com.universaltill.channel-shopify/ABCDEFGHIJKLMNOPQRSTUV"

var productTopics = []string{"products/create", "products/update", "products/delete"}

func (e *env) register(url string, topics []string, mode string) (channel.WebhookRegisterResult, error) {
	e.t.Helper()
	r := e.em.Ask(channel.AskWebhookRegister, channel.WebhookRegisterInput{CallbackURL: url, Topics: topics, Mode: mode})
	if r.Err != nil {
		return channel.WebhookRegisterResult{}, r.Err
	}
	var out channel.WebhookRegisterResult
	if err := json.Unmarshal(r.Answer, &out); err != nil {
		e.t.Fatal(err)
	}
	return out, nil
}

func TestRegisterCreatesOneSubscriptionPerProductTopicAndIsIdempotent(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	all := append([]string{"orders/create", "app/uninstalled", "customers/redact"}, productTopics...)
	for i := 0; i < 3; i++ {
		got, err := e.register(cb, all, channel.WebhookModeEnsure)
		if err != nil {
			t.Fatal(err)
		}
		want := channel.TopicRegistered
		if i > 0 {
			want = channel.TopicAlready
		}
		for _, tp := range productTopics {
			if got.Topics[tp].State != want {
				t.Fatalf("run %d %s: %+v", i, tp, got.Topics[tp])
			}
		}
		for _, tp := range []string{"orders/create", "app/uninstalled", "customers/redact"} {
			if got.Topics[tp].State != channel.TopicUnsupported {
				t.Fatalf("%s: %+v", tp, got.Topics[tp])
			}
		}
	}
	subs := e.shop.Subscriptions()
	if len(subs) != 3 {
		t.Fatalf("3 runs left %d subscriptions, want 3", len(subs))
	}
	seen := map[string]bool{}
	for _, s := range subs {
		if s.URL != cb || seen[s.Topic] {
			t.Fatalf("subscription %+v", s)
		}
		seen[s.Topic] = true
	}
	if !seen["PRODUCTS_UPDATE"] || !seen["PRODUCTS_CREATE"] || !seen["PRODUCTS_DELETE"] {
		t.Fatalf("topics %v", seen)
	}
	// Least data in transit: ids and the update time only.
	for _, s := range subs {
		if strings.Join(s.IncludeFields, ",") != "id,admin_graphql_api_id,updated_at" {
			t.Fatalf("include fields %v", s.IncludeFields)
		}
	}
	// The second and third run only listed.
	writes := 0
	for _, r := range e.shop.Requests() {
		if strings.HasPrefix(r.Op, "WebhookCreate") {
			writes++
		}
	}
	if writes != 3 {
		t.Fatalf("%d create mutations over three runs, want 3", writes)
	}
}

func TestRegisterNeverTouchesAForeignSubscription(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	hand := e.shop.AddSubscription("PRODUCTS_UPDATE", "https://other.example.invalid/hook")
	hand2 := e.shop.AddSubscription("PRODUCTS_UPDATE", "https://cloud.example.invalid/somewhere/else")
	e.shop.AddSubscription("PRODUCTS_CREATE", "") // another endpoint kind
	if _, err := e.register(cb, productTopics, channel.WebhookModeEnsure); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.shop.Subscriptions() {
		if (s.ID == hand && s.URL != "https://other.example.invalid/hook") || (s.ID == hand2 && s.URL != "https://cloud.example.invalid/somewhere/else") {
			t.Fatalf("a foreign subscription was changed: %+v", s)
		}
	}
	if len(e.shop.Subscriptions()) != 3+3 {
		t.Fatalf("%d subscriptions", len(e.shop.Subscriptions()))
	}
	for _, r := range e.shop.Requests() {
		if r.Op == "WebhookUpdate" {
			t.Fatal("an update was sent although nothing of ours existed")
		}
	}
}

func TestRegisterAfterRotationUpdatesOurSubscriptionInPlace(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	if _, err := e.register(cb, productTopics, channel.WebhookModeEnsure); err != nil {
		t.Fatal(err)
	}
	before := e.shop.Subscriptions()
	rotated := "https://cloud.example.invalid/hooks/channel/com.universaltill.channel-shopify/ZYXWVUTSRQPONMLKJIHGFE"
	got, err := e.register(rotated, productTopics, channel.WebhookModeEnsure)
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range productTopics {
		if got.Topics[tp].State != channel.TopicUpdated {
			t.Fatalf("%s: %+v", tp, got.Topics[tp])
		}
	}
	after := e.shop.Subscriptions()
	if len(after) != len(before) {
		t.Fatalf("%d subscriptions after rotation, was %d", len(after), len(before))
	}
	for i := range after {
		if after[i].ID != before[i].ID || after[i].URL != rotated {
			t.Fatalf("not updated in place: %+v -> %+v", before[i], after[i])
		}
	}
}

func TestRegisterHealsASubscriptionShopifyDropped(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	if _, err := e.register(cb, productTopics, channel.WebhookModeEnsure); err != nil {
		t.Fatal(err)
	}
	e.shop.RemoveSubscriptions()
	got, err := e.register(cb, productTopics, channel.WebhookModeEnsure)
	if err != nil || got.Topics["products/update"].State != channel.TopicRegistered || len(e.shop.Subscriptions()) != 3 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestRegisterRetriesWithoutIncludeFieldsWhenShopifyRefusesThem(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.RefuseIncludeFields(true)
	got, err := e.register(cb, []string{"products/update"}, channel.WebhookModeEnsure)
	if err != nil || got.Topics["products/update"].State != channel.TopicRegistered {
		t.Fatalf("%v %+v", err, got)
	}
	if s := e.shop.Subscriptions(); len(s) != 1 || len(s[0].IncludeFields) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestRegisterFailuresAreStableCodesAndNeverSecretsOrURLs(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	e.shop.FailGraphQL("WebhookList", "ACCESS_DENIED")
	got, err := e.register(cb, productTopics, channel.WebhookModeEnsure)
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range productTopics {
		if got.Topics[tp].State != channel.TopicFailed || got.Topics[tp].Code != "scope_missing" {
			t.Fatalf("%s: %+v", tp, got.Topics[tp])
		}
	}
	e.shop.RefuseTopic()
	got, _ = e.register(cb, []string{"products/update"}, channel.WebhookModeEnsure)
	if g := got.Topics["products/update"]; g.State != channel.TopicFailed || g.Code != "topic_not_allowed" {
		t.Fatalf("%+v", g)
	}
	// THROTTLED is retried by the host before the plugin sees it (emulator and cloud alike).
	out, _ := json.Marshal(got)
	logs := ""
	for _, l := range e.em.Host.Logs {
		logs += l.Code + " " + toJSON(l.Fields) + "\n"
	}
	for _, s := range []string{"hooks/channel", "ABCDEFGHIJ", "invented-access-token", "invented-client-secret", "cloud.example.invalid"} {
		if strings.Contains(logs, s) || strings.Contains(string(out), s) {
			t.Fatalf("%q leaked into a log line or the answer", s)
		}
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestRegisterRefusesAnAddressThatIsNotOurHookShape(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	for name, url := range map[string]string{
		"http":         "http://cloud.example.invalid/hooks/channel/p/r",
		"userinfo":     "https://a:b@cloud.example.invalid/hooks/channel/p/r",
		"query":        "https://cloud.example.invalid/hooks/channel/p/r?x=1",
		"fragment":     "https://cloud.example.invalid/hooks/channel/p/r#x",
		"other path":   "https://cloud.example.invalid/admin",
		"empty":        "",
		"traversal":    "https://cloud.example.invalid/hooks/channel/../admin",
		"space":        "https://cloud.example.invalid/hooks/channel/p /r",
		"double slash": "https://cloud.example.invalid//hooks/channel/p/r",
	} {
		if _, err := e.register(url, productTopics, channel.WebhookModeEnsure); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(e.shop.Requests()) != 0 {
		t.Fatal("a refused address still reached the shop")
	}
}

func TestRegisterRemoveIsNotSupportedBecauseNothingIsEverDeleted(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	if _, err := e.register(cb, productTopics, channel.WebhookModeEnsure); err != nil {
		t.Fatal(err)
	}
	got, err := e.register(cb, productTopics, channel.WebhookModeRemove)
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range productTopics {
		if got.Topics[tp].State != channel.TopicUnsupported {
			t.Fatalf("%s: %+v", tp, got.Topics[tp])
		}
	}
	if len(e.shop.Subscriptions()) != 3 {
		t.Fatal("remove changed the shop")
	}
}

func TestRegisterOnlyWritesWebhookSubscriptionsAndAnswersAllAskedTopics(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	got, err := e.register(cb, append([]string{"made/up"}, productTopics...), channel.WebhookModeEnsure)
	if err != nil || len(got.Topics) != 4 || got.Topics["made/up"].State != channel.TopicUnsupported {
		t.Fatalf("%v %+v", err, got)
	}
	for _, r := range e.shop.Requests() {
		if r.Op != "WebhookList" && r.Op != "WebhookCreate" {
			t.Fatalf("unexpected operation %s", r.Op)
		}
	}
}
