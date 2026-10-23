#!/usr/bin/env bash
# Packages the plugin into the canonical marketplace release artifact:
#   dist/<plugin-id>_<version>_universal.tar.gz
# A WASI module (bin/plugin.wasm, built by scripts/build.sh) runs on every
# till os/arch => one universal artifact. manifest.json sits at the archive
# root; no "./" members (the POS importer rejects them as path traversal).
set -euo pipefail
cd "$(dirname "$0")/.."

scripts/validate.sh

ID=$(python3 -c "import json;print(json.load(open('manifest.json'))['id'])")
VERSION=$(python3 -c "import json;print(json.load(open('manifest.json'))['version'])")
OUT="dist/${ID}_${VERSION}_universal.tar.gz"
mkdir -p dist

entries=(manifest.json README.md bin)
[ -f LICENSE ] && entries+=(LICENSE)
# locales/*.json carry the entry label, the panel's text and the job
# progress messages (architecture/plugin-architecture.md §7); the till reads
# them from the INSTALLED bundle, so they must ship (ut-docs#1883 review).
[ -d locales ] && entries+=(locales)
# COPYFILE_DISABLE stops macOS tar shipping AppleDouble ._* junk (the
# marketplace bundle-hygiene gate rejects it).
COPYFILE_DISABLE=1 tar -czf "$OUT" "${entries[@]}"
# cd into dist/ first so the recorded checksum names the bare filename, not
# "dist/<file>" — a self-hoster downloading the artifact + .sha256 pair into
# one directory runs `sha256sum -c`, which fails to find a "dist/..." path
# (ut-docs#166).
if command -v sha256sum >/dev/null 2>&1; then (cd "$(dirname "$OUT")" && sha256sum "$(basename "$OUT")") > "${OUT}.sha256"; else (cd "$(dirname "$OUT")" && shasum -a 256 "$(basename "$OUT")") > "${OUT}.sha256"; fi
echo "packaged $OUT"
