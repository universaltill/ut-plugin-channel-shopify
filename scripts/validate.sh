#!/usr/bin/env bash
# Manifest sanity for the Shopify sales-channel plugin (ADR-0169, contract
# sales-channel-v1.md section 1). ut-cloud re-validates the same rules when it
# signs and again when its channel host loads a release; this is the early,
# local copy of the ones that are cheap to check here.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import json, os, re, sys
m = json.load(open("manifest.json"))
errs = []
if m.get("id") != "com.universaltill.channel-shopify": errs.append("id must stay com.universaltill.channel-shopify (the cloud's auth-rules table and every connection key on it)")
for f in ("name", "version", "description"):
    if not m.get(f): errs.append(f"missing {f}")
if not re.match(r'^\d+\.\d+\.\d+$', m.get("version", "")): errs.append("bad version")
if m.get("canonical_type") != "channel": errs.append("canonical_type must be 'channel' (ADR-0169 section 3)")
if m.get("hosts") != ["cloud"]: errs.append('hosts must be ["cloud"]: a channel never runs on a till')
if m.get("channel_contract") != 1: errs.append("channel_contract must be 1")
if m.get("runtime") != "wasm": errs.append("runtime must be 'wasm' (ADR-0001)")
if m.get("permissions") != ["net:{account}"]: errs.append('permissions must be exactly ["net:{account}"]: no db:own, secret:*, http:* or other net:')
if not m.get("locales"): errs.append("the marketplace upload gate needs at least one locale")
for loc in m.get("locales", []):
    if not os.path.isfile(f"locales/{loc}.json"): errs.append(f"manifest declares locale {loc} but locales/{loc}.json is missing")
# ADR-0169 section 5 (least scopes, owner-fixed list for the Shopify MVP): exactly these, no more.
WANT = ["read_products","write_products","read_inventory","write_inventory","read_locations","read_orders","write_orders","read_merchant_managed_fulfillment_orders","write_merchant_managed_fulfillment_orders"]
if (m.get("channel") or {}).get("auth", {}).get("scopes") != WANT: errs.append("auth.scopes must be exactly the ADR-0169 least-scope list (no read_customers until customer sync, 4774)")
if (m.get("channel") or {}).get("webhooks", {}).get("verify") != {"header": "X-Shopify-Hmac-Sha256", "alg": "hmac-sha256", "encoding": "base64"}: errs.append("webhooks.verify must be X-Shopify-Hmac-Sha256 / hmac-sha256 / base64")
if (m.get("channel") or {}).get("account_suffix") != ".myshopify.com": errs.append("account_suffix must be .myshopify.com")
ep = (m.get("entrypoint") or "")
if not ep.startswith("./") or not ep.endswith(".wasm"): errs.append("wasm runtime needs a ./...wasm entrypoint")
elif not os.path.isfile(ep[2:]): errs.append(f"module not found: {ep} (run scripts/build.sh)")
extra = set(m) - {"id","name","version","description","author","website","entrypoint","runtime","canonical_type","device_arch","min_pos_version","permissions","hosts","channel_contract","channel","locales","platforms","capabilities"}
if extra: errs.append(f"a channel manifest may not declare {sorted(extra)}: it runs in the cloud host and reads only the channel block")

c = m.get("channel") or {}
if c.get("kind") not in ("webshop", "marketplace"): errs.append("channel.kind is webshop or marketplace")
acct, suffix = c.get("account", ""), c.get("account_suffix", "")
if not (acct.startswith("^") and acct.endswith("$")): errs.append("channel.account must be an anchored pattern")
if not re.match(r'^(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?){2,}$', suffix): errs.append("channel.account_suffix must be a dot and at least two DNS labels")
else:
    try:
        rx = re.compile(acct)
        if not rx.match("shop-a" + suffix): errs.append("the account pattern must accept <label>" + suffix)
        for evil in ("evil.example/x" + suffix, "a@b" + suffix, "a:b" + suffix, "a/b" + suffix, "127.0.0.1", "x" + suffix + ".evil.example"):
            if rx.match(evil): errs.append(f"the account pattern accepts {evil!r}")
    except re.error as e:
        errs.append(f"account pattern: {e}")
# The adapter validates the account itself too: keep its suffix equal.
src = open("src/app/api.go").read()
am = re.search(r'accountRe = regexp\.MustCompile\(`([^`]*)`\)', src)
if not am or am.group(1) != acct: errs.append("src/app/api.go accountRe must be identical to channel.account (a trailing hyphen must be refused by both or neither)")
cap = c.get("capabilities") or {}
# The adapter can only push a price to an EXISTING variant; it cannot create or edit an item
# (ut-docs#4774). Declaring items push/both would make the cloud queue listing.upsert for every new item and dead-letter it.
if cap.get("items") not in ("pull", None): errs.append("capabilities.items must be 'pull' until item creation lands (ut-docs#4774)")
if cap.get("variants") not in ("pull", "both"): errs.append("capabilities.variants is 'pull' or 'both' (price push to existing variants only)")
# The adapter does order.cancel and refund.create (ut-docs#4774): without push the cloud never queues them.
for k in ("fulfilment", "cancel", "refunds"):
    if cap.get(k) != "push": errs.append(f"capabilities.{k} must be 'push': the adapter implements it")
if f'AccountSuffix = "{suffix}"' not in src: errs.append("src/app/api.go AccountSuffix must equal channel.account_suffix")

a = c.get("auth") or {}
for k in ("authorize_url", "token_url"):
    if not str(a.get(k, "")).startswith("https://{account}/"): errs.append(f"auth.{k} must start with https://{{account}}/")
if "code" in a.get("grants", []) and not a.get("callback_verify"): errs.append("auth.callback_verify is required with the code grant")
cv = a.get("callback_verify") or {}
if cv and (cv.get("alg") != "hmac-sha256" or cv.get("encoding") not in ("hex", "base64") or cv.get("canonical") != "sorted-query-without-param"):
    errs.append("callback_verify must be hmac-sha256, hex|base64, sorted-query-without-param")
if not (a.get("inject") or {}).get("header"): errs.append("auth.inject.header is required")
w = c.get("webhooks") or {}
v = w.get("verify") or {}
if w.get("topics") and (v.get("alg") != "hmac-sha256" or v.get("encoding") not in ("hex", "base64") or not v.get("header")):
    errs.append("webhooks.verify must be {header, alg: hmac-sha256, encoding}")
for s in c.get("schedules", []):
    if s.get("every_s", 0) < 30: errs.append("schedules every_s is at least 30")
if errs:
    print("FAIL: " + "; ".join(errs)); sys.exit(1)
print(f"ok {m['id']} v{m['version']}")
PY
