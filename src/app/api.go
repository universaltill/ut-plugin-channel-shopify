// Package app is the Shopify channel adapter: it speaks Shopify's Admin
// GraphQL API through the host's one network path (channel.HTTP) and hands
// records to the host with channel.Submit. It holds no credential, no account
// name and no state of its own beyond the host's cursors.
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// base is the manifest's own placeholder for the connection's verified
// account host. A plugin is never told its account: the host fills it in
// before the egress rules run (ut-cloud channelhost expandAccount).
const base = "https://{account}"

// AccountSuffix is the manifest's channel.account_suffix; validate.sh keeps
// the two equal.
const AccountSuffix = ".myshopify.com"

// APIVersion is the Admin GraphQL API version this adapter is written
// against (the owner's app uses webhooks API 2026-10). One constant, one
// endpoint: nothing else is ever requested.
const APIVersion = "2026-10"

const graphqlPath = "/admin/api/" + APIVersion + "/graphql.json"

var (
	accountRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.myshopify\.com$`)
	// cursorRe is a Shopify page cursor (base64). It only ever travels as a
	// GraphQL variable or inside the host's cursor store, never in a URL.
	// Treated as opaque: Shopify documents no cursor format, so the pattern
	// only keeps out whitespace, quotes and control characters, and bounds the
	// length (three cursors must fit the host's 64 KiB store).
	cursorRe = regexp.MustCompile(`^[A-Za-z0-9+/=_.~:-]{1,1000}$`)
	timeRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)
	// opIDRe is what an outbox operation id must look like to be used as an
	// idempotency key.
	opIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	curRe  = regexp.MustCompile(`^[A-Z]{3}$`)
	// gidRes validates a Shopify global id per resource type. An id is
	// accepted only in this exact shape, so nothing but digits ever reaches
	// a GraphQL variable (and no id ever reaches a URL at all).
	gidRes = map[string]*regexp.Regexp{}
)

// The resource types the adapter handles.
const (
	tProduct   = "Product"
	tVariant   = "ProductVariant"
	tOrder     = "Order"
	tLineItem  = "LineItem"
	tLocation  = "Location"
	tInvItem   = "InventoryItem"
	tCustomer  = "Customer"
	tFulfOrder = "FulfillmentOrder"
	tTxn       = "OrderTransaction"
	tJob       = "Job"
)

func init() {
	for _, t := range []string{tProduct, tVariant, tOrder, tLineItem, tLocation, tInvItem, tCustomer, tFulfOrder, tTxn, tJob} {
		gidRes[t] = regexp.MustCompile(`^gid://shopify/` + t + `/[0-9]{1,20}$`)
	}
}

// okGID reports whether id is a well-formed Shopify global id of type t.
func okGID(t, id string) bool {
	re, ok := gidRes[t]
	return ok && re.MatchString(id)
}

// maxTitleBytes bounds a line's title: the cloud refuses an order whose
// lines[].name is over 256 bytes AFTER JSON decoding, so 250 leaves room. clip
// works on the decoded string (see there).
const maxTitleBytes = 250

// Page sizes keep the REQUESTED GraphQL cost of every query under Shopify's
// 1,000-point single-query limit; the per-ask budgets (budget.go) bound how
// much of that an ask may spend.
const (
	productPage     = 4
	variantPage     = 50
	maxVariants     = 100 // manifest limits.max_variants
	maxOptions      = 3   // manifest limits.max_options
	maxProductPages = 40  // per import ask; the rest resumes from the cursor
	stockPage       = 50
	maxStockPages   = 40
	linePage        = 20
	maxExtraLinePgs = 2 // an order over 60 lines is refused, logged, never truncated
)

// statusError is a non-2xx answer. It carries the status only, never a body:
// bodies may hold personal data.
type statusError struct{ Status int }

func (e *statusError) Error() string { return fmt.Sprintf("channel answered %d", e.Status) }

// gqlError is a GraphQL error answer. Only the error CODE is kept; messages
// can quote customer data.
type gqlError struct{ Code string }

func (e *gqlError) Error() string { return "graphql error " + e.Code }

// errThrottled is Shopify's THROTTLED answer that reached the plugin (the
// host retries it first, so this means the host gave up or passed it on).
var errThrottled = errors.New("shopify throttled the request")

// queryDef is one GraphQL operation. Text is a compile-time constant:
// every value, id and cursor travels in the variables object, never in the
// text (contract section 3, "never interpolated").
type queryDef struct {
	Name     string
	Text     string
	Cost     int // a hand-counted upper bound of the requested cost, the host's pacing hint
	Mutation bool
}

// lastCost is the throttle bucket Shopify reported with the latest answer.
var lastCost struct{ Available, Maximum float64 }

type gqlEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
	Extensions struct {
		Cost struct {
			Throttle struct {
				Maximum   float64 `json:"maximumAvailable"`
				Available float64 `json:"currentlyAvailable"`
			} `json:"throttleStatus"`
		} `json:"cost"`
	} `json:"extensions"`
}

// run sends one operation and decodes its data into out. idemKey, when not
// empty, is sent as the Idempotency-Key header. Shopify does not honour that
// header: it is a signal to the HOST that the POST is safe to re-send after a
// lost answer. Use it only for mutations that are safe to repeat.
func run(q queryDef, vars map[string]any, idemKey string, out any) error {
	body, err := json.Marshal(map[string]any{"operationName": q.Name, "query": q.Text, "variables": vars})
	if err != nil {
		return err
	}
	h := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	if idemKey != "" {
		h["Idempotency-Key"] = idemKey
	}
	spent.http++
	spent.host++
	spent.cost += q.Cost
	resp, err := channel.HTTP(channel.HTTPRequest{Method: "POST", URL: base + graphqlPath, Headers: h, Body: body, CostHint: q.Cost})
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return &statusError{Status: resp.Status}
	}
	var env gqlEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return errors.New("channel answered something that is not the documented JSON")
	}
	lastCost.Available, lastCost.Maximum = env.Extensions.Cost.Throttle.Available, env.Extensions.Cost.Throttle.Maximum
	for _, e := range env.Errors {
		if e.Extensions.Code == "THROTTLED" {
			return errThrottled
		}
	}
	if len(env.Errors) > 0 {
		return &gqlError{Code: clip(env.Errors[0].Extensions.Code, 40)}
	}
	if out == nil {
		return nil
	}
	if len(env.Data) == 0 || string(env.Data) == "null" || json.Unmarshal(env.Data, out) != nil {
		return errors.New("channel answered without the documented data")
	}
	return nil
}

// userError is a mutation's userErrors entry. Only the code is kept.
type userError struct {
	Code string `json:"code"`
}

// userErrCode returns the first user error's code, or "" when none; an
// error without a code gives "unknown".
func userErrCode(es []userError) string {
	if len(es) == 0 {
		return ""
	}
	if es[0].Code == "" {
		return "unknown"
	}
	return clip(es[0].Code, 60)
}

// clip makes s safe to put in a record under a byte limit of n AFTER JSON
// encoding and decoding: invalid UTF-8 is dropped (json.Marshal would turn
// each bad byte into a 3-byte U+FFFD, growing the string past the limit the
// cloud checks on the decoded value) and the cut falls on a rune boundary, so
// the result is valid UTF-8 of at most n bytes and decodes to itself.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cursor reads a cursor key; a missing or malformed one is "".
func cursor(key string, re *regexp.Regexp) string {
	spent.host++
	b, err := channel.CursorGet(key)
	if err != nil || !re.Match(b) {
		return ""
	}
	return string(b)
}

// submit hands a batch to the host and reports how many it rejected. Reasons
// are logged by code only.
func submit(kind string, records any) (rejected int, err error) {
	spent.host++
	res, err := channel.Submit(kind, records)
	if err != nil {
		return 0, err
	}
	for _, r := range res {
		if r.Status == channel.ResultRejected {
			rejected++
		}
	}
	if rejected > 0 {
		hostLog(channel.LevelWarn, "submit.rejected", map[string]any{"kind": kind, "rejected": rejected, "of": len(res)})
	}
	return rejected, nil
}

func logErr(code string, err error) {
	var se *statusError
	var ge *gqlError
	f := map[string]any{}
	switch {
	case errors.As(err, &se):
		f["status"] = se.Status
	case errors.As(err, &ge):
		f["gql"] = ge.Code
	default:
		f["error"] = fmt.Sprintf("%T", err)
	}
	hostLog(channel.LevelWarn, code, f)
}
