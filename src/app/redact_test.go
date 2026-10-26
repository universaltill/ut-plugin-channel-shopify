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

// realRedact is the shape of Shopify's customers/redact payload with invented
// values (ut-docs#4929).
const realRedact = `{"shop_id":954889,"shop_domain":"shop-a.myshopify.com","customer":{"id":191167,"email":"invented@example.invalid","phone":"+440000000000"},"orders_to_redact":[299938,280263]}`

func askRedact(e *env, topic, body string) (channel.WebhookResult, error) {
	b := []byte(body)
	in := channel.WebhookInput{Headers: e.shop.WebhookHeaders("wh-r", topic, b), Body: b}
	r := e.em.Ask(channel.AskWebhook, in)
	var got channel.WebhookResult
	if r.Err != nil {
		return got, r.Err
	}
	return got, json.Unmarshal(r.Answer, &got)
}

func TestCustomerRedactCarriesCustomerAndOrderIDs(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	got, err := askRedact(e, "customers/redact", realRedact)
	if err != nil {
		t.Fatal(err)
	}
	if got.Event != channel.EventCustomerRedact || len(got.ResourceIDs) != 1 || got.ResourceIDs[0] != "gid://shopify/Customer/191167" {
		t.Errorf("customer ids: %+v", got)
	}
	want := []string{"gid://shopify/Order/299938", "gid://shopify/Order/280263"}
	if strings.Join(got.OrderIDs, ",") != strings.Join(want, ",") {
		t.Errorf("order ids %v, want %v", got.OrderIDs, want)
	}
	b, _ := json.Marshal(got)
	for _, pii := range []string{"invented@example.invalid", "+44", "example.invalid", "954889"} {
		if strings.Contains(string(b), pii) {
			t.Errorf("the answer contains %q", pii)
		}
	}
}

func TestCustomerRedactIDsAreExactAndDeduplicated(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	got, err := askRedact(e, "customers/redact", `{"customer":{"id":9007199254740993},"orders_to_redact":[9007199254740993,9007199254740993,5]}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.OrderIDs, ",") != "gid://shopify/Order/9007199254740993,gid://shopify/Order/5" {
		t.Errorf("order ids %v", got.OrderIDs)
	}
}

func TestCustomerRedactWithoutOrdersOrCustomer(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	got, err := askRedact(e, "customers/redact", `{"customer":{"id":7},"orders_to_redact":[]}`)
	if err != nil || len(got.OrderIDs) != 0 || len(got.ResourceIDs) != 1 {
		t.Errorf("empty list: %+v %v", got, err)
	}
	got, err = askRedact(e, "customers/redact", `{"orders_to_redact":[11]}`)
	if err != nil || len(got.ResourceIDs) != 0 || len(got.OrderIDs) != 1 {
		t.Errorf("no customer: %+v %v", got, err)
	}
	// a null customer id is no id, not a refusal that would 503 the redact
	got, err = askRedact(e, "customers/redact", `{"customer":{"id":null},"orders_to_redact":[11]}`)
	if err != nil || len(got.ResourceIDs) != 0 || len(got.OrderIDs) != 1 {
		t.Errorf("null customer id: %+v %v", got, err)
	}
}

func TestCustomerRedactRefusesBadIDs(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	for name, body := range map[string]string{
		"negative order":    `{"orders_to_redact":[-5]}`,
		"float order":       `{"orders_to_redact":[1.5]}`,
		"exponent order":    `{"orders_to_redact":[1e3]}`,
		"string order":      `{"orders_to_redact":["12"]}`,
		"gid string order":  `{"orders_to_redact":["gid://shopify/Order/12"]}`,
		"zero order":        `{"orders_to_redact":[0]}`,
		"huge order":        `{"orders_to_redact":[123456789012345678901]}`,
		"null order":        `{"orders_to_redact":[null]}`,
		"negative customer": `{"customer":{"id":-1}}`,
		"float customer":    `{"customer":{"id":2.0}}`,
		"string customer":   `{"customer":{"id":"x"}}`,
		"not an array":      `{"orders_to_redact":5}`,
		"not json":          `nope`,
	} {
		if _, err := askRedact(e, "customers/redact", body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCustomerRedactOrderLimit(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	mk := func(n int) string {
		var sb strings.Builder
		sb.WriteString(`{"orders_to_redact":[`)
		for i := 1; i <= n; i++ {
			if i > 1 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "%d", i)
		}
		sb.WriteString("]}")
		return sb.String()
	}
	got, err := askRedact(e, "customers/redact", mk(10000))
	if err != nil || len(got.OrderIDs) != 10000 || got.OrderIDsTruncated {
		t.Fatalf("10,000 orders: %d truncated=%v %v", len(got.OrderIDs), got.OrderIDsTruncated, err)
	}
	// 10,001 entries with one duplicate are 10,000 distinct: not truncated.
	dup := strings.Replace(mk(10000), "[1,2,", "[1,1,2,", 1)
	if got, err := askRedact(e, "customers/redact", dup); err != nil || len(got.OrderIDs) != 10000 || got.OrderIDsTruncated {
		t.Errorf("10,001 with a duplicate: %d truncated=%v %v", len(got.OrderIDs), got.OrderIDsTruncated, err)
	}
	// 10,001 distinct: first 10,000 in payload order, flag set, customer kept.
	over := strings.Replace(mk(10001), `{"orders_to_redact"`, `{"customer":{"id":77},"orders_to_redact"`, 1)
	got, err = askRedact(e, "customers/redact", over)
	if err != nil || len(got.OrderIDs) != 10000 || !got.OrderIDsTruncated {
		t.Fatalf("10,001 orders: %d truncated=%v %v", len(got.OrderIDs), got.OrderIDsTruncated, err)
	}
	if got.OrderIDs[0] != "gid://shopify/Order/1" || got.OrderIDs[9999] != "gid://shopify/Order/10000" {
		t.Errorf("not the first 10,000 in order: %s .. %s", got.OrderIDs[0], got.OrderIDs[9999])
	}
	if len(got.ResourceIDs) != 1 || got.ResourceIDs[0] != "gid://shopify/Customer/77" {
		t.Errorf("customer id lost: %v", got.ResourceIDs)
	}
	// A malformed id anywhere, even past the cap, still refuses the body.
	bad := strings.Replace(mk(10005), "10005]", `"x"]`, 1)
	if _, err := askRedact(e, "customers/redact", bad); err == nil {
		t.Error("bad id past the cap accepted")
	}
}

func TestOtherTopicsNeverReadTheGDPRBody(t *testing.T) {
	e := newEnv(t, fakeshop.Options{})
	for _, topic := range []string{"shop/redact", "customers/data_request", "app/uninstalled"} {
		got, err := askRedact(e, topic, `{"customer":{"id":5},"orders_to_redact":[1,2]}`)
		if err != nil || len(got.OrderIDs) != 0 || len(got.ResourceIDs) != 0 {
			t.Errorf("%s: %+v %v", topic, got, err)
		}
		// Even a garbage body is not read.
		if _, err := askRedact(e, topic, `nope`); err != nil {
			t.Errorf("%s: body was read: %v", topic, err)
		}
	}
}
