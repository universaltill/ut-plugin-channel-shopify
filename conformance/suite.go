//go:build !wasip1

package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Hostile modes a channel's fake server must be able to switch into. They
// model a malicious or broken CHANNEL (the adapter is trusted code that
// could still be tricked by its server).
const (
	EndlessPages = "endless_pages" // list calls never end
	ForeignLinks = "foreign_links" // absolute links to another host, ids with path tricks
	EchoHeaders  = "echo_headers"  // data echoes every request header, credential included
	HugeBody     = "huge_body"     // a list answer over the 8 MiB response cap
)

// Shop is the adapter's own fake channel server, as the suite needs it.
type Shop interface {
	http.Handler
	// Writes counts requests that were not GET or HEAD.
	Writes() int
	// Fingerprint changes whenever anything the shop holds changes.
	Fingerprint() string
	// Hostile switches the shop into one of the modes above.
	Hostile(mode string)
}

// Target is an adapter under test plus its invented shop.
type Target struct {
	Handlers channel.Handlers
	// NewShop returns a fresh, seeded shop (no hostile mode).
	NewShop func() Shop
	// Account is the connection's account host; CredentialHeader the
	// manifest's auth.inject.header; Credential the token the shop accepts.
	Account, CredentialHeader, Credential string
	// IsWrite says whether a request changes the shop. Default: any method
	// but GET and HEAD. A GraphQL channel sends reads as POST and overrides
	// this to look at the operation.
	IsWrite func(Seen) bool
	// MaxCostHint, when set, is the channel's single-query cost limit: every
	// request must carry a cost hint above 0 and not above it.
	MaxCostHint int
	// ExpectedProducts is how many products NewShop seeds, for check 2.
	ExpectedProducts int
}

func (tg Target) isWrite(s Seen) bool {
	if tg.IsWrite != nil {
		return tg.IsWrite(s)
	}
	return s.Method != "GET" && s.Method != "HEAD" && s.Method != ""
}

// operationText is the GraphQL `query` field of a request body, or "".
func operationText(s Seen) string {
	var b struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(s.Body, &b) != nil {
		return ""
	}
	return b.Query
}

var deleteRe = regexp.MustCompile(`(?i)delete|destroy`)

// rig is one fresh connection. t installs the SDK's fake host for its life.
func (tg Target) rig(t TB) (Shop, *Emulator) {
	t.Helper()
	shop := tg.NewShop()
	return shop, NewEmulator(t, tg.Handlers, shop, tg.Account, tg.CredentialHeader, tg.Credential)
}

// Run is the whole suite: nine checks, in the order of the contract's
// section 6. A check whose dependency has not been built yet is a
// clearly-marked TODO that skips with the card it waits for; it never passes
// silently.
func Run(t *testing.T, tg Target) {
	report := func(t *testing.T, violations []string) {
		t.Helper()
		for _, v := range violations {
			t.Error(v)
		}
	}
	t.Run("1_connect_test_reads_and_writes_nothing", func(t *testing.T) { report(t, Check1ConnectReadsAndWritesNothing(t, tg)) })
	t.Run("2_import_50_products_second_run_zero_changes", func(t *testing.T) { report(t, Check2ImportIsDeterministic(t, tg)) })
	t.Run("3_webhook_verification", func(t *testing.T) {
		t.Skip("TODO ut-docs#4762 (webhook ingress): valid, bad signature, wrong account, unknown connection, oversized body (identical 401) and the signed duplicate (200, no second effect) are the HOST's route. " +
			"Adapter side (parse only, no tenant context) is pinned in src/app TestWebhookIsParsedWithoutTenantContext; the fake shop signs bodies with SignWebhook for the host-side test.")
	})
	t.Run("4_order_creates_exactly_one_channelorder", func(t *testing.T) {
		t.Skip("TODO ut-docs#4767 (order intake): one ChannelOrder after a duplicate and an out-of-order delivery is the cloud's store. " +
			"Adapter side (contract order record, no personal data) is pinned in src/app TestOrdersAreSubmittedWithoutPersonalData.")
	})
	t.Run("5_stock_set_absolute_stale_expect_rereads", func(t *testing.T) {
		t.Skip("TODO ut-docs#4771 (write-back): needs the cloud outbox to issue stock.set with expect. " +
			"Adapter side is pinned in src/app TestStockSetIsAbsoluteAndAStaleExpectReReads.")
	})
	t.Run("6_fulfilment_cancel_refund_exactly_once", func(t *testing.T) {
		t.Skip("TODO ut-docs#4771 (write-back): exactly-once under the host's retry needs the host's outbox attempts. " +
			"Adapter side (Idempotency-Key, already-done is success) is pinned in src/app TestFulfilmentReachesShopifyOnceAndAlreadyFulfilledIsSuccess; cancel and refund.create are ut-docs#4774 for Shopify.")
	})
	t.Run("7_no_outbox_operation_deletes", func(t *testing.T) { report(t, Check7NoOperationDeletes(t, tg)) })
	t.Run("8_order_edit_with_restock_moves_stock_exactly", func(t *testing.T) {
		t.Skip("TODO ut-docs#4767 (order intake) and the till's channel-orders card: stock moves are computed from stored revisions by the cloud and the till, not by the adapter. " +
			"The adapter's part (qty never lowered, refunded and restocked quantities passed through) Shopify refunds and edits are mapped (src/app TestOrderEditAndRestockedRefundArePassedThroughNeverLoweringQty).")
	})
	t.Run("9_hostile_channel_and_limits", func(t *testing.T) { report(t, Check9HostileChannelAndLimits(t, tg)) })
}

// Check2ImportIsDeterministic: the adapter side of "import 50 products with
// variants, a second run gives zero changes". The host is idempotent on
// (connection, kind, external_id, external_version), so a second import must
// submit only records whose key AND content the first one already did; the
// cloud's own unchanged/accepted accounting is ut-docs#4765.
func Check2ImportIsDeterministic(t TB, tg Target) []string {
	t.Helper()
	var v violations
	_, em := tg.rig(t)
	runAll := func(label string) map[string]string {
		for i := 0; i < 50; i++ {
			r := em.Ask(channel.AskImport, struct{}{})
			if r.Err != nil {
				v.addf("%s import failed: %v", label, r.Err)
				break
			}
			var ans struct {
				Done bool `json:"done"`
			}
			_ = json.Unmarshal(r.Answer, &ans)
			if ans.Done {
				break
			}
			if i == 49 {
				v.addf("%s import never finished", label)
			}
		}
		out := map[string]string{}
		for _, b := range em.Host.Submitted {
			for _, rec := range b.Records {
				var id struct {
					A string `json:"external_id"`
					B string `json:"external_version"`
				}
				_ = json.Unmarshal(rec, &id)
				out[b.Kind+"|"+id.A+"|"+id.B] = string(rec)
			}
		}
		em.Host.Submitted = nil
		return out
	}
	first := runAll("first")
	listings := 0
	for k, rec := range first {
		if strings.HasPrefix(k, channel.KindListing+"|") {
			listings++
			var l channel.Listing
			if json.Unmarshal([]byte(rec), &l) != nil || len(l.Variants) == 0 {
				v.addf("a listing without variants was imported: %.80s", k)
			}
		}
	}
	if tg.ExpectedProducts > 0 && listings != tg.ExpectedProducts {
		v.addf("imported %d products, the shop has %d", listings, tg.ExpectedProducts)
	}
	second := runAll("second")
	for k, rec := range second {
		if old, ok := first[k]; !ok {
			v.addf("second import produced a record the first did not: %.80s", k)
		} else if old != rec {
			v.addf("second import changed the content of a record under the same version: %.80s", k)
		}
	}
	if len(second) != len(first) {
		v.addf("second import submitted %d distinct records, the first %d", len(second), len(first))
	}
	return v
}

type violations []string

func (v *violations) addf(format string, args ...any) { *v = append(*v, fmt.Sprintf(format, args...)) }

var allowedMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true}

// Check1ConnectReadsAndWritesNothing: the connect test reports the account
// and writes nothing anywhere: no request but GET or HEAD, the shop is
// byte-for-byte unchanged, nothing submitted, no cursor, no outbox, no ack.
// It returns what it found wrong; empty means the adapter conforms.
func Check1ConnectReadsAndWritesNothing(t TB, tg Target) []string {
	t.Helper()
	var v violations
	shop, em := tg.rig(t)
	before := shop.Fingerprint()
	r := em.Ask(channel.AskConnectTest, struct{}{})
	if r.Err != nil {
		v.addf("connect test failed: %v", r.Err)
		return v
	}
	var ans channel.ConnectTestResult
	if err := json.Unmarshal(r.Answer, &ans); err != nil || ans.AccountName == "" {
		v.addf("connect test must report the account name; got %q (%v)", r.Answer, err)
	}
	if len(em.Seen()) == 0 {
		v.addf("connect test made no request: it must really read the account")
	}
	for _, s := range em.Seen() {
		if tg.isWrite(s) {
			v.addf("connect test sent %s %s", s.Method, s.URL)
		}
	}
	if shop.Writes() != 0 {
		v.addf("the shop saw %d write request(s)", shop.Writes())
	}
	if shop.Fingerprint() != before {
		v.addf("the shop changed during the connect test")
	}
	h := em.Host
	if len(h.Submitted) != 0 {
		v.addf("connect test submitted %d batch(es)", len(h.Submitted))
	}
	if len(h.CursorKeys()) != 0 || h.Calls[channel.FnCursorSet] != 0 {
		v.addf("connect test wrote a cursor")
	}
	if h.Calls[channel.FnOutboxNext] != 0 || h.Calls[channel.FnOutboxAck] != 0 || len(h.Acks) != 0 {
		v.addf("connect test touched the outbox")
	}
	return v
}

// opKinds are all outbound operation kinds of the contract (section 3).
var opKinds = []string{
	channel.OpListingUpsert, channel.OpListingUnpublish, channel.OpStockSet, channel.OpFulfilmentCreate,
	channel.OpOrderCancel, channel.OpRefundCreate, channel.OpImageUpsert,
}

// Check7NoOperationDeletes: whatever the cloud queues, the adapter never
// sends a DELETE (the contract has no delete anywhere); a deactivated item
// would be listing.unpublish, a draft or unlisted product at the channel, once
// an adapter declares items push.
func Check7NoOperationDeletes(t TB, tg Target) []string {
	t.Helper()
	var v violations
	_, em := tg.rig(t)
	for i, k := range opKinds {
		em.Host.Outbox = append(em.Host.Outbox, channel.OutboxOp{
			ID: fmt.Sprintf("op-%d", i), Kind: k, ExternalID: "o-9001",
			Payload: json.RawMessage(`{"external_order_id":"o-9001","available":3,"carrier":"c","reason":"other","restock":false}`),
		})
	}
	em.Host.Outbox = append(em.Host.Outbox, channel.OutboxOp{ID: "op-del", Kind: "listing.delete", ExternalID: "p-0001"})
	r := em.Ask(channel.AskOutbox, struct{}{})
	if r.Err != nil {
		v.addf("outbox ask: %v", r.Err)
		return v
	}
	for _, s := range em.Seen() {
		if strings.EqualFold(s.Method, "DELETE") {
			v.addf("the adapter sent DELETE %s", s.URL)
		}
		// A GraphQL channel deletes through a mutation, not a method.
		if q := operationText(s); deleteRe.MatchString(q) {
			v.addf("the adapter sent an operation that deletes: %.80s", q)
		}
	}
	if a, ok := em.Host.Acks["op-del"]; ok && a.Error == nil {
		v.addf("an unknown delete operation was acknowledged as success")
	}
	return v
}

// hostileIDs are ids a hostile channel could hand the adapter, or a
// confused cloud could queue: they must never reach a URL as path, query or
// another host.
var hostileIDs = []string{"../../admin", "o-9001?x=1", "o-9001#frag", "o-9001/../../x", "https://evil.example/o-9001", strings.Repeat("a", 5000), "a b"}

// drive gives the adapter every ask it may be given, hostile ids where ids go.
func drive(em *Emulator) {
	em.Ask(channel.AskConnectTest, struct{}{})
	em.Ask(channel.AskImport, struct{}{})
	em.Ask(channel.AskTick, map[string]string{"event": "reconcile"})
	em.Ask(channel.AskFetch, channel.FetchInput{Event: channel.EventOrderChanged, ResourceIDs: hostileIDs})
	em.Ask(channel.AskContact, channel.ContactInput{OrderExternalID: "../../admin"})
	for i, id := range hostileIDs {
		for _, k := range []string{channel.OpStockSet, channel.OpFulfilmentCreate, channel.OpOrderCancel} {
			em.Host.Outbox = append(em.Host.Outbox, channel.OutboxOp{
				ID: fmt.Sprintf("h-%s-%d", k, i), Kind: k, ExternalID: id,
				Payload: json.RawMessage(fmt.Sprintf(`{"external_order_id":%q,"available":1,"carrier":"c","reason":"other"}`, id)),
			})
		}
	}
	em.Ask(channel.AskOutbox, struct{}{})
}

// Check9HostileChannelAndLimits: a hostile channel cannot make the adapter
// leave {account}, cannot make it leak the credential, and cannot push it
// past the host's caps or memory. (The sandbox's own enforcement of the same
// limits on a hostile PLUGIN is ut-cloud internal/channelhost TestEgress_*
// and TestHostile_*; this checks the adapter stays inside them by itself.)
func Check9HostileChannelAndLimits(t TB, tg Target) []string {
	t.Helper()
	var v violations

	// -- egress stays on the account, with and without a hostile channel.
	for _, mode := range []string{"", ForeignLinks} {
		shop, em := tg.rig(t)
		if mode != "" {
			shop.Hostile(mode)
		}
		drive(em)
		if len(em.Seen()) == 0 {
			v.addf("[%s] the adapter made no request at all", mode)
		}
		for _, s := range em.Seen() {
			u, err := url.Parse(s.URL)
			switch {
			case err != nil || u.Scheme != "https" || u.Hostname() != tg.Account || u.User != nil || (u.Port() != "" && u.Port() != "443"):
				v.addf("[%s] request left {account}: %s %.120s", mode, s.Method, s.URL)
				continue
			case !allowedMethods[strings.ToUpper(s.Method)] && s.Method != "":
				v.addf("[%s] method %s is not allowed", mode, s.Method)
			case s.Denied:
				v.addf("[%s] the host had to deny %s %.120s: the adapter must not attempt it", mode, s.Method, s.URL)
			}
			if tg.MaxCostHint > 0 && (s.CostHint <= 0 || s.CostHint > tg.MaxCostHint) {
				v.addf("[%s] request without a usable cost hint (%d, limit %d): %.80s", mode, s.CostHint, tg.MaxCostHint, s.URL)
			}
			for _, id := range hostileIDs {
				if len(id) >= 8 && len(id) < 1000 && bytes.Contains(s.Body, []byte(id)) {
					v.addf("[%s] a hostile id reached a request body: %.60s", mode, id)
				}
			}
			for _, seg := range strings.Split(u.EscapedPath(), "/") {
				if seg == ".." || seg == "." {
					v.addf("[%s] a path segment %q reached a URL: %.120s", mode, seg, s.URL)
				}
			}
			if strings.Contains(u.RawQuery, "x=1") || u.Fragment != "" || strings.Contains(s.URL, "#") {
				v.addf("[%s] an id turned into a query or fragment: %.120s", mode, s.URL)
			}
			for k := range s.Header {
				if strings.EqualFold(k, tg.CredentialHeader) {
					v.addf("[%s] the adapter set the credential header itself", mode)
				}
			}
		}
	}

	// -- the credential never reaches anything the adapter produces.
	{
		shop, em := tg.rig(t)
		shop.Hostile(EchoHeaders)
		drive(em)
		all := []string{string(em.Ask(channel.AskConnectTest, struct{}{}).Answer)}
		for _, b := range em.Host.Submitted {
			for _, r := range b.Records {
				all = append(all, string(r))
			}
		}
		for k, val := range em.Host.Cursors {
			all = append(all, k, string(val))
		}
		for _, l := range em.Host.Logs {
			b, _ := json.Marshal(l)
			all = append(all, string(b))
		}
		for id, a := range em.Host.Acks {
			b, _ := json.Marshal(a)
			all = append(all, id, string(b))
		}
		for _, s := range all {
			if strings.Contains(s, tg.Credential) {
				v.addf("the credential reached adapter output: %.200s", s)
				break
			}
		}
	}

	// -- endless pages stop inside the caps, in memory and in time.
	{
		shop, em := tg.rig(t)
		shop.Hostile(EndlessPages)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < 3; i++ { // the cursor lets the next ask continue
			r := em.Ask(channel.AskImport, struct{}{})
			if r.Err != nil {
				v.addf("endless pages: ask %d failed: %v", i, r.Err)
				break
			}
			if r.Elapsed > time.Duration(channel.ImportCallSecs)*time.Second {
				v.addf("endless pages: ask %d took %s, over the deadline", i, r.Elapsed)
			}
			if c := em.HTTPCalls(); c > MaxHTTPCallsPerAsk {
				v.addf("endless pages: ask %d made %d channel_http calls (cap %d)", i, c, MaxHTTPCallsPerAsk)
			}
		}
		n := 0
		for _, b := range em.Host.Submitted {
			n += len(b.Records)
		}
		if n > 3*MaxRecordsPerAsk {
			v.addf("endless pages: %d records submitted by three asks (cap %d each)", n, MaxRecordsPerAsk)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > channel.MaxMemoryBytes {
			v.addf("endless pages: heap grew by %d bytes (guest memory cap %d)", grew, channel.MaxMemoryBytes)
		}
	}

	// -- an answer over the response cap is an error, not a hang or a partial import.
	{
		shop, em := tg.rig(t)
		shop.Hostile(HugeBody)
		r := em.Ask(channel.AskImport, struct{}{})
		if r.Err == nil {
			v.addf("an answer over the response cap must fail the ask (the host answers -5)")
		}
		if len(em.Host.Submitted) != 0 {
			v.addf("records were submitted from an answer the host refused")
		}
	}
	return v
}
