#!/usr/bin/env bash
#
# Requires a manifest.json version bump on any PR that touches a shipped
# file (ut-docs#1940, rolled out here per ut-docs#1948; the compiled-plugin
# variant from ut-plugin-tax-de since this plugin became a WASM module,
# ut-docs#4032 -- before that it was config-only and shipped only
# manifest.json, README.md and LICENSE).
#
# WHY THIS EXISTS: scripts/package.sh bundles manifest.json, README.md,
# locales/ (when present) and bin/ (the compiled WASI module) into the
# marketplace release artifact. None of this repo's other checks (build,
# validate, authors) look at the version field, so a PR that changes what
# ships but forgets to bump manifest.json's version lands on main, passes
# every check, and then SILENTLY NEVER SHIPS: auto-tag-release.yml reads the
# unchanged version, sees v<version> already tagged/released, and correctly
# does nothing. main and the marketplace diverge with no failing signal
# anywhere. Already needed hand repair at least three times in
# ut-plugin-language-de/-es before that guard existed there (ut-docs#1940).
#
# WHAT COUNTS AS "SHIPPED" -- THE COMPILED-PLUGIN VARIANT: this repo builds
# a WASI module (scripts/build.sh: `go build -o bin/plugin.wasm ./src`) and
# package.sh's own entries=(...) line bundles that compiled `bin` directory
# directly -- but `bin/` is gitignored (see .gitignore), so it can NEVER
# appear in `git diff --name-only` no matter how much the plugin's behaviour
# changed. Keying SHIPPED_PATTERNS off the literal bundled path, the way the
# asset-only plugins (ut-plugin-language-{de,es}, ut-plugin-theme-*) do, is
# therefore unsafe here -- it would silently never fire. Instead this keys
# off the SOURCE that PRODUCES bin/plugin.wasm: every file under src/ (the
# `go build ... ./src` argument), plus go.mod/go.sum (a dependency bump
# changes the compiled binary without touching anything under src/ at all),
# plus scripts/build.sh itself -- it is the BUILD RECIPE, so an edit to it
# (a new -ldflags/-trimpath/-tags, a different GOOS/GOARCH or -o path)
# changes the shipped binary while every file under src/ stays byte-identical
# (verified as a real false negative in this guard's 2026-09-10 review).
# See check-version-bump.test.sh's "entries mirror" case for how this is
# verified against package.sh's and build.sh's own source, and
# docs/code-reviews/ for the rollout card (ut-docs#1948) tracking which
# other ut-plugin-* repos still need the same treatment.
#
# One accepted, deliberate over-approximation: `src/*` also matches
# `*_test.go` files, which go build (not go test) never compiles into
# bin/plugin.wasm -- a test-only change will ask for a version bump it does
# not strictly need. That is a false positive (harmless extra bump), never
# a false negative (a real behaviour change shipping unbumped) -- the
# failure mode this guard exists to prevent -- so it is left as-is rather
# than complicating SHIPPED_PATTERNS with an exclusion mechanism no other
# repo in this rollout needs.
#
# Keep this list mirrored to package.sh's own `entries=(...)` line and
# build.sh's own `go build` source argument -- nothing enforces that
# automatically, so a change to either needs a matching edit here (and
# check-version-bump.test.sh's "entries mirror" case exists to catch drift
# between the three by re-reading package.sh's and build.sh's own source).
# The check logic itself lives in scripts/check-version-bump.body.sh, sourced
# at the bottom: one canonical body, byte-identical in every ut-plugin-* repo
# (canonical copy: ut-docs scripts/templates/check-version-bump.body.sh,
# ut-docs#4489; this repo moved off its older inline copy in ut-docs#5155).
# Never edit the body here; only SHIPPED_PATTERNS (and SHIPPED_NOTE) are
# per-repo.
set -euo pipefail
cd "$(dirname "$0")/.."

# SHIPPED_PATTERNS is read by the sourced body, which shellcheck does not
# follow without -x.
# shellcheck disable=SC2034
SHIPPED_PATTERNS=(
    'manifest.json'
    'README.md'
    'LICENSE'
    'locales/*'
    'src/*'
    'go.mod'
    'go.sum'
    'scripts/build.sh'
    # The packaging recipe: it decides what goes into the release artifact
    # (entries, tar flags), so an edit to it changes what ships (ut-docs#5147).
    'scripts/package.sh'
)

# What package.sh bundles, for the failure text in the shared body.
# shellcheck disable=SC2034
SHIPPED_NOTE="these files (or, for src/**/go.mod/go.sum, the compiled bin/plugin.wasm they produce)"

# shellcheck source=scripts/check-version-bump.body.sh
. scripts/check-version-bump.body.sh
