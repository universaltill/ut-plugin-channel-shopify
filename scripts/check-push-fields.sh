#!/usr/bin/env bash
# Fails when a plugin manifest's channel.push_fields lists a value the till's validator does not allow
# (universal-till internal/plugins/manifest_channel.go channelPushFields, ut-docs#4911). A till that rejects
# the manifest refuses the whole plugin (ut-docs#6975). Usage: check-push-fields.sh [manifest.json]
# Keep ALLOWED in step with the till; widen it only after the till's list is widened and released.
set -euo pipefail
m="${1:-$(dirname "$0")/../manifest.json}"
python3 - "$m" <<'PY'
import json, sys
ALLOWED = ["name", "description", "sku", "barcodes", "price", "compare_at", "category_path"]
m = json.load(open(sys.argv[1]))
pf = (m.get("channel") or {}).get("push_fields", [])
bad = [f for f in pf if f not in ALLOWED]
dup = sorted({f for f in pf if pf.count(f) > 1})
if bad or dup:
    print(f"FAIL: channel.push_fields unknown to the till: {bad}, duplicated: {dup} (allowed: {', '.join(ALLOWED)})")
    sys.exit(1)
print("ok push_fields are all accepted by the till validator")
PY
