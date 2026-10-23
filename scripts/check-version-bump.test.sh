#!/usr/bin/env bash
# Tests for scripts/check-version-bump.sh (ut-docs#1940, ut-docs#1948; the
# compiled-plugin variant ported from ut-plugin-tax-de for ut-docs#4032).
#
# Each case builds a throwaway git repo (real commits, so BASE_SHA/HEAD_SHA
# and `git diff`/`git show` all behave exactly as they do against a real
# PR) with just enough of this repo's layout for the script under test to
# operate on, then runs the real script against a BASE_SHA..HEAD_SHA pair
# and asserts both the exit code and that the failure output actually names
# the changed shipped file(s) and the version -- a script that exits
# non-zero for the wrong reason must not be mistaken for a passing test of
# the real check.
#
# This fixture mirrors the COMPILED-plugin shape (WASI module built from
# src/ via go build), unlike ut-plugin-language-{de,es}'s asset-only
# fixture -- see check-version-bump.sh's own header for why that matters
# (bin/ is gitignored and undiffable, so the guard keys off src/**/go.mod/
# go.sum instead of the literal bundled path).
set -euo pipefail
cd "$(dirname "$0")/.."

REAL_SCRIPT="$(pwd)/scripts/check-version-bump.sh"
PACKAGE_SH="$(pwd)/scripts/package.sh"
BUILD_SH="$(pwd)/scripts/build.sh"
# Repo-relative form of BUILD_SH -- the build recipe is itself an input to
# the compiled artifact, so SHIPPED_PATTERNS has to cover this exact path
# (see the gitignored-entry branch of the "entries mirror" case below).
BUILD_SH_REL="scripts/build.sh"
FAILS=0

work_dir=""
cleanup() {
    # `|| true`: under `set -e`, a trap function's own exit status can
    # become the script's final exit status -- without this, a clean run
    # that happens to fire EXIT while work_dir is still "" (nothing to
    # clean up yet) would report failure via `[ -n "" ]`'s exit 1, even
    # though every case passed.
    [ -n "$work_dir" ] && rm -rf "$work_dir"
    true
}
trap cleanup EXIT

# fresh_repo
# Builds a throwaway git repo ($case_dir) with an initial commit carrying a
# minimal but complete compiled-plugin layout (manifest.json v0.1.0,
# src/main.go, go.mod, go.sum, locales/de.json, README.md, LICENSE, a
# .gitignore excluding bin/, a workflow file, a docs file). Sets $base_sha
# to that commit. The script under test is copied in (not symlinked -- it
# does `cd "$(dirname "$0")/.."`, which resolves relative to ITS OWN path,
# so it must run from inside the fixture).
fresh_repo() {
    [ -n "$work_dir" ] && rm -rf "$work_dir"
    work_dir="$(mktemp -d)"
    case_dir="${work_dir}/repo"
    mkdir -p "${case_dir}/scripts" "${case_dir}/src" "${case_dir}/locales" \
        "${case_dir}/.github/workflows" "${case_dir}/docs/code-reviews"
    cp "$REAL_SCRIPT" "${case_dir}/scripts/check-version-bump.sh"
    chmod +x "${case_dir}/scripts/check-version-bump.sh"

    cat >"${case_dir}/manifest.json" <<'JSON'
{
  "id": "com.universaltill.integration-ai-fixture",
  "name": "Fixture AI Plugin",
  "version": "0.1.0",
  "canonical_type": "integration",
  "runtime": "wasm",
  "entrypoint": "./bin/plugin.wasm"
}
JSON
    echo 'package main' >"${case_dir}/src/main.go"
    echo 'func main() {}' >>"${case_dir}/src/main.go"
    cat >"${case_dir}/go.mod" <<'GOMOD'
module github.com/universaltill/integration-ai-fixture

go 1.23
GOMOD
    echo "" >"${case_dir}/go.sum"
    echo '{"a.one": "Eins"}' >"${case_dir}/locales/de.json"
    echo "# Fixture AI plugin" >"${case_dir}/README.md"
    echo "MIT" >"${case_dir}/LICENSE"
    cat >"${case_dir}/.gitignore" <<'GITIGNORE'
/bin/
/dist/
GITIGNORE
    echo "name: CI" >"${case_dir}/.github/workflows/ci.yml"
    cat >"${case_dir}/scripts/build.sh" <<'BUILD'
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
GOOS=wasip1 GOARCH=wasm go build -o bin/plugin.wasm ./src
BUILD
    cat >"${case_dir}/scripts/package.sh" <<'PACKAGE'
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
entries=(manifest.json README.md bin)
[ -f LICENSE ] && entries+=(LICENSE)
[ -d locales ] && entries+=(locales)
tar -czf dist/out.tar.gz "${entries[@]}"
PACKAGE

    (
        cd "$case_dir"
        git init -q
        git config user.email test@example.com
        git config user.name Test
        git add -A
        git commit -q -m "initial"
    )
    base_sha=$(cd "$case_dir" && git rev-parse HEAD)
}

# commit_change - stages whatever the caller already edited in $case_dir
# and commits it, setting $head_sha.
commit_change() {
    (cd "$case_dir" && git add -A && git commit -q -m "change")
    head_sha=$(cd "$case_dir" && git rev-parse HEAD)
}

run_check() {
    (cd "$case_dir" && BASE_SHA="$base_sha" HEAD_SHA="$head_sha" bash scripts/check-version-bump.sh)
}

assert_pass() {
    local name="$1"
    local out rc
    set +e
    out="$(run_check 2>&1)"
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
        echo "FAIL [$name]: expected exit 0, got $rc. Output:"
        echo "$out"
        FAILS=$((FAILS + 1))
        return
    fi
    echo "ok   [$name]"
}

assert_fail_containing() {
    local name="$1"
    shift
    local out rc
    set +e
    out="$(run_check 2>&1)"
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then
        echo "FAIL [$name]: expected non-zero exit, got 0. Output:"
        echo "$out"
        FAILS=$((FAILS + 1))
        return
    fi
    local needle
    for needle in "$@"; do
        if ! grep -qF -- "$needle" <<<"$out"; then
            echo "FAIL [$name]: exited non-zero (good) but output did not mention expected reason ('$needle'). Output:"
            echo "$out"
            FAILS=$((FAILS + 1))
            return
        fi
    done
    echo "ok   [$name] (exit $rc, mentions: $*)"
}

# --- case 1: locale changed, version NOT bumped -> FAIL, names the file ---
fresh_repo
echo '{"a.one": "Eins", "a.two": "Zwei"}' >"${case_dir}/locales/de.json"
commit_change
assert_fail_containing "locale changed, no bump" \
    "locales/de.json" "manifest.json" "0.1.0" "FAIL"

# --- case 2: locale changed, version bumped -> PASS -----------------------
fresh_repo
echo '{"a.one": "Eins", "a.two": "Zwei"}' >"${case_dir}/locales/de.json"
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '0.1.1'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
commit_change
assert_pass "locale changed, version bumped"

# --- case 3: README changed, version NOT bumped -> FAIL --------------------
fresh_repo
echo "# Fixture AI plugin, updated" >"${case_dir}/README.md"
commit_change
assert_fail_containing "README changed, no bump" "README.md" "FAIL"

# --- case 4: only manifest.json touched (version bump itself) -> PASS -----
# The bump is itself a shipped-file change, and the version differs -- must
# not require a SECOND shipped file to also have changed.
fresh_repo
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '0.1.1'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
commit_change
assert_pass "manifest-only version bump"

# --- case 5: docs-only change -> PASS, no bump required --------------------
fresh_repo
echo "# review" >"${case_dir}/docs/code-reviews/2026-09-10-example.md"
commit_change
assert_pass "docs-only change, no bump required"

# --- case 6: workflow-only change -> PASS, no bump required ----------------
fresh_repo
echo "name: CI (updated)" >"${case_dir}/.github/workflows/ci.yml"
commit_change
assert_pass "workflow-only change, no bump required"

# --- case 7: manifest.json changed but NOT the version (e.g. permissions)
# still counts as a shipped-file change, still requires the version to move.
fresh_repo
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['description'] = 'updated blurb'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
commit_change
assert_fail_containing "manifest changed without version bump" "manifest.json" "FAIL"

# --- case 8: SHIPPED_PATTERNS mirrors package.sh's own bundle entries,
# generalized for a COMPILED plugin -----------------------------------------
# package.sh's `entries=(...)` line is the actual source of truth for what
# ships. For each entry, if it is a path git would actually track (checked
# with `git check-ignore` against THIS FIXTURE's own .gitignore -- the same
# check the real repo's tree would give), require it (or "<entry>/*") in
# SHIPPED_PATTERNS as before. But `bin` is gitignored -- it is build.sh's
# OUTPUT, not a tracked file, and can never appear in `git diff --name-only`
# -- so for a gitignored entry this instead requires that build.sh's own
# `go build ... <SOURCE_DIR>` argument (with a leading "./" stripped) has a
# matching "<source_dir>/*" pattern in SHIPPED_PATTERNS, AND that go.mod,
# go.sum and the build script itself are all covered too (a dependency bump
# or a build-flag change alters the compiled binary without touching
# anything under src/ at all). This is what a real
# change to package.sh's bundle, build.sh's source directory, or
# check-version-bump.sh's SHIPPED_PATTERNS would be caught by drifting from
# each other.
entries_line=$(grep -m1 '^entries=' "$PACKAGE_SH") || entries_line=""
if [ -z "$entries_line" ]; then
    echo "FAIL [entries mirror]: could not find package.sh's entries=(...) line"
    FAILS=$((FAILS + 1))
else
    array_body="${entries_line#entries=(}"
    array_body="${array_body%)}"
    read -r -a bundle_entries <<<"$array_body"
    # Scoped to the SHIPPED_PATTERNS=(...) array body ONLY, with comments
    # stripped. Scanning the whole script for any single-quoted token (this
    # check's original form) let a quoted mention in a COMMENT stand in for a
    # real array entry -- and check-version-bump.sh is a deliberately
    # comment-heavy script. Proved by mutation in this guard's 2026-09-10
    # review: replacing the `'go.sum'` array line with a comment containing
    # `'go.sum'` left this case green, and since LICENSE and go.sum had no
    # behavioural case of their own at the time, the ENTIRE suite stayed green
    # while the guard silently stopped covering them. `# ...` is stripped
    # before the quoted tokens are read (no SHIPPED_PATTERNS entry contains a
    # '#'), so neither a comment outside the array nor one inside it can
    # masquerade as an entry.
    patterns=$(awk '/^SHIPPED_PATTERNS=\(/ {inside = 1; next}
                    inside && /^\)/ {inside = 0}
                    inside' "$REAL_SCRIPT" | sed 's/#.*//' | grep -oE "'[^']*'" | tr -d "'")

    build_line=$(grep -m1 'go build ' "$BUILD_SH") || build_line=""
    if [ -z "$build_line" ]; then
        echo "FAIL [entries mirror]: could not find build.sh's 'go build' line"
        FAILS=$((FAILS + 1))
        build_line=""
    fi
    # Last whitespace-separated token on the go build line is its source
    # package argument (e.g. "./src"); strip a leading "./".
    source_dir=""
    if [ -n "$build_line" ]; then
        source_dir="${build_line##* }"
        source_dir="${source_dir#./}"
    fi

    # Use a throwaway repo purely to evaluate `git check-ignore` against
    # this fixture's own .gitignore -- reuses fresh_repo's already-committed
    # $case_dir, which already has .gitignore committed.
    mismatch=0
    for entry in "${bundle_entries[@]}"; do
        # Try both the bare name and a trailing-slash form: a directory-only
        # gitignore pattern (e.g. "/bin/") only matches git check-ignore's
        # bare-name query once that path exists on disk as a directory --
        # which a bundled-but-not-yet-built entry like "bin" need not, in a
        # fresh checkout that has never run scripts/build.sh.
        if (cd "$case_dir" && { git check-ignore -q "$entry" 2>/dev/null || git check-ignore -q "$entry/" 2>/dev/null; }); then
            # Gitignored (compiled-artifact) entry: require the source dir
            # that PRODUCES it, plus go.mod/go.sum, instead of the literal
            # (undiffable) entry name.
            if [ -z "$source_dir" ]; then
                echo "FAIL [entries mirror]: '$entry' is gitignored (a compiled artifact) but build.sh's source directory could not be determined"
                mismatch=1
                continue
            fi
            if ! grep -qxF "${source_dir}/*" <<<"$patterns"; then
                echo "FAIL [entries mirror]: package.sh bundles gitignored '$entry' (built from '$source_dir' per build.sh) but SHIPPED_PATTERNS has no '${source_dir}/*' entry"
                mismatch=1
            fi
            if ! grep -qxF "go.mod" <<<"$patterns"; then
                echo "FAIL [entries mirror]: '$entry' is a compiled artifact but SHIPPED_PATTERNS has no 'go.mod' entry (a dependency bump changes the binary without touching $source_dir)"
                mismatch=1
            fi
            if ! grep -qxF "go.sum" <<<"$patterns"; then
                echo "FAIL [entries mirror]: '$entry' is a compiled artifact but SHIPPED_PATTERNS has no 'go.sum' entry (a dependency bump changes the binary without touching $source_dir)"
                mismatch=1
            fi
            # The build RECIPE is an input to the artifact too: a new
            # -ldflags/-trimpath/-tags, a different GOOS/GOARCH or -o path
            # changes the shipped binary with every file under $source_dir
            # byte-identical.
            if ! grep -qxF "$BUILD_SH_REL" <<<"$patterns"; then
                echo "FAIL [entries mirror]: '$entry' is a compiled artifact but SHIPPED_PATTERNS has no '${BUILD_SH_REL}' entry (a build-flag change rebuilds the binary without touching $source_dir)"
                mismatch=1
            fi
        else
            # Tracked entry: same check as the asset-only plugins' guard.
            if ! grep -qxF "$entry" <<<"$patterns" && ! grep -qxF "${entry}/*" <<<"$patterns"; then
                echo "FAIL [entries mirror]: package.sh bundles '$entry' but SHIPPED_PATTERNS in check-version-bump.sh has no matching entry"
                mismatch=1
            fi
        fi
    done
    # LICENSE ships conditionally ([ -f LICENSE ] && entries+=(LICENSE)),
    # so it never appears on package.sh's own entries=(...) line -- checked
    # separately here rather than assumed.
    if ! grep -qxF "LICENSE" <<<"$patterns"; then
        echo "FAIL [entries mirror]: package.sh conditionally bundles LICENSE (when present) but SHIPPED_PATTERNS has no LICENSE entry"
        mismatch=1
    fi
    # locales/ also ships conditionally ([ -d locales ] && entries+=(locales)).
    if ! grep -qxF "locales/*" <<<"$patterns"; then
        echo "FAIL [entries mirror]: package.sh conditionally bundles locales/ (when present) but SHIPPED_PATTERNS has no 'locales/*' entry"
        mismatch=1
    fi
    if [ "$mismatch" -eq 0 ]; then
        echo "ok   [entries mirror] (package.sh: ${bundle_entries[*]} + conditional LICENSE/locales; compiled from: ${source_dir} via ${BUILD_SH_REL})"
    else
        FAILS=$((FAILS + 1))
    fi
fi

# --- case 9: base branch ALSO moved forward independently, with its own --
# shipped change + bump (multi-lane / ut-docs#1940-review B1 regression).
# A two-dot diff against a moving base.sha would let that OTHER PR's
# landed bump silently cover for THIS PR's own missing one. This PR's own
# change (relative to the true merge-base) does NOT bump -- must still
# FAIL even though base_sha's tip now carries a higher version than this
# PR's own unbumped head.
fresh_repo
mergebase_sha="$base_sha"
# "main" advances independently with its own shipped change + bump.
echo '{"a.one": "Eins", "a.three": "Drei"}' >"${case_dir}/locales/de.json"
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '0.1.1'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
(cd "$case_dir" && git add -A && git commit -q -m "main advances, bumped")
main_tip_sha=$(cd "$case_dir" && git rev-parse HEAD)
# This PR's own head branches from the ORIGINAL base, not main's advanced
# tip, and changes a shipped file without bumping.
(cd "$case_dir" && git checkout -q "$mergebase_sha")
echo '{"a.one": "Eins", "a.two": "Zwei"}' >"${case_dir}/locales/de.json"
(cd "$case_dir" && git add -A && git commit -q -m "PR change, no bump")
pr_head_sha=$(cd "$case_dir" && git rev-parse HEAD)
base_sha="$main_tip_sha"
head_sha="$pr_head_sha"
assert_fail_containing "base moved forward independently, PR itself didn't bump" \
    "locales/de.json" "FAIL"

# --- case 10: pre-release version suffix must not crash past the FAIL ----
# path (ut-docs#1940-review B2 regression).
fresh_repo
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '0.1.0-beta.1'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
(cd "$case_dir" && git add -A && git commit -q -m "adopt pre-release version scheme")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
echo '{"a.one": "Eins", "a.two": "Zwei"}' >"${case_dir}/locales/de.json"
commit_change
assert_fail_containing "pre-release version suffix, no bump" "FAIL"

# --- case 11: version differs but is ALREADY TAGGED elsewhere ------------
# (ut-docs#1940-review N1). auto-tag-release.yml no-ops on an existing tag,
# so reusing one is the same silent-never-ships failure with a green guard.
fresh_repo
(cd "$case_dir" && git tag "v0.1.1")
echo '{"a.one": "Eins", "a.two": "Zwei"}' >"${case_dir}/locales/de.json"
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '0.1.1'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
commit_change
assert_fail_containing "bumped to an already-tagged version" "FAIL" "v0.1.1" "ALREADY TAGGED"

# --- case 12: src/*.go changed, version NOT bumped -> FAIL ----------------
# This is the whole point of the compiled-plugin variant: a real behaviour
# change to the WASI module's source, with nothing under bin/ ever visible
# to git diff, must still be caught.
fresh_repo
cat >"${case_dir}/src/main.go" <<'GO'
package main

func main() {
	println("changed")
}
GO
commit_change
assert_fail_containing "src/*.go changed, no bump" "src/main.go" "FAIL"

# --- case 13: src/*.go changed, version bumped -> PASS --------------------
fresh_repo
cat >"${case_dir}/src/main.go" <<'GO'
package main

func main() {
	println("changed")
}
GO
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '0.1.1'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
commit_change
assert_pass "src/*.go changed, version bumped"

# --- case 14: go.mod changed (dependency bump), version NOT bumped -> FAIL
# A dependency version bump changes the compiled bin/plugin.wasm without
# touching a single file under src/ -- must be caught on its own.
fresh_repo
cat >"${case_dir}/go.mod" <<'GOMOD'
module github.com/universaltill/integration-ai-fixture

go 1.23

require example.com/newdep v1.0.0
GOMOD
commit_change
assert_fail_containing "go.mod changed, no bump" "go.mod" "FAIL"

# --- case 15: nested src/ path changed, version NOT bumped -> FAIL --------
# Confirms src/* crosses into subdirectories the way locales/* already does
# (this repo's real layout is src/<package>/<file>.go, not flat).
fresh_repo
mkdir -p "${case_dir}/src/app"
echo 'package app' >"${case_dir}/src/app/app.go"
commit_change
assert_fail_containing "nested src/ file added, no bump" "src/app/app.go" "FAIL"

# --- case 16: LICENSE changed, version NOT bumped -> FAIL -----------------
# LICENSE ships conditionally, so it never appears on package.sh's own
# entries=(...) line and the "entries mirror" case was its ONLY coverage.
# Added in this guard's 2026-09-10 review after a mutation showed a mirror-
# test false pass on LICENSE left the whole suite green.
fresh_repo
echo "MIT (2026)" >"${case_dir}/LICENSE"
commit_change
assert_fail_containing "LICENSE changed, no bump" "LICENSE" "FAIL"

# --- case 17: go.sum changed, version NOT bumped -> FAIL ------------------
# Same reason as case 16: go.sum had only "entries mirror" coverage. A
# `go mod tidy` that rewrites go.sum alone still relinks the binary.
fresh_repo
echo "example.com/newdep v1.0.0 h1:abc=" >"${case_dir}/go.sum"
commit_change
assert_fail_containing "go.sum changed, no bump" "go.sum" "FAIL"

# --- case 18: scripts/build.sh changed (build recipe), no bump -> FAIL ----
# The build RECIPE is an input to bin/plugin.wasm just as much as src/ is:
# adding -trimpath/-ldflags/-tags, or changing GOOS/GOARCH, produces a
# different shipped binary with src/ byte-identical. Found as a real false
# negative in this guard's 2026-09-10 review.
fresh_repo
cat >>"${case_dir}/scripts/build.sh" <<'BUILD'
# rebuilt with -trimpath from here on
BUILD
commit_change
assert_fail_containing "build.sh recipe changed, no bump" "scripts/build.sh" "FAIL"

# --- case 19: a shipped file RENAMED OUT of a shipped location -> FAIL ----
# `git diff --name-only` with rename detection (git's default) prints only
# the DESTINATION, so `git mv src/x.go docs/x.go` -- which really does remove
# a file from the compiled package -- reported as a lone `docs/x.go` and the
# guard answered "no shipped file changed". Found as a real false negative in
# this guard's 2026-09-10 review; check-version-bump.sh now passes
# --no-renames so the src/ side shows up as a delete.
fresh_repo
mkdir -p "${case_dir}/src/app"
echo 'package app' >"${case_dir}/src/app/app.go"
(cd "$case_dir" && git add -A && git commit -q -m "add a nested source file")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git mv src/app/app.go docs/app.go)
commit_change
assert_fail_containing "source file renamed out of src/, no bump" \
    "src/app/app.go" "FAIL"

# --- case 20: first PR of a new repo, no manifest.json at the merge-base -> PASS
# A new plugin repo starts with a commit that holds no manifest (licence,
# ignore rules, review record); the plugin's first PR adds manifest.json.
# There is no earlier version to bump from, so the guard passes. A manifest
# that EXISTS at the base but does not parse still fails closed (case 21).
fresh_repo
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q HEAD~1 -- manifest.json)
echo '// the plugin' >>"${case_dir}/src/main.go"
commit_change
assert_pass "first PR: no manifest.json at the merge-base"

# --- case 21: manifest.json present at the merge-base but unparseable -> FAIL
fresh_repo
echo '{ not json' >"${case_dir}/manifest.json"
(cd "$case_dir" && git add -A && git commit -q -m "broken manifest")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
cat >"${case_dir}/manifest.json" <<'JSON'
{"id": "x", "version": "0.1.0"}
JSON
echo '// change' >>"${case_dir}/src/main.go"
commit_change
assert_fail_containing "unparseable base manifest still fails closed" "could not read manifest.json version at merge-base"

# --- case 22: stale branch forked before the first manifest -> FAIL
# main has since shipped manifest.json (and tagged it); a branch forked from
# the pre-manifest commit must not ride the first-release exemption, or it
# merges an identical 0.1.0 + code change that never ships.
fresh_repo
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet")
fork=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q HEAD~1 -- manifest.json && git commit -q -m "first release" && git tag v0.1.0)
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q -b stale "$fork" && git checkout -q "$base_sha" -- manifest.json)
echo '// later change' >>"${case_dir}/src/main.go"
commit_change
assert_fail_containing "stale branch forked before the first manifest" "FAIL" "forked before manifest.json existed"

# --- case 23 (ut-docs#4484): the first-release path still refuses a version
# that is already tagged -- the same silent never-ships failure.
fresh_repo
first_version=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["version"])' "${case_dir}/manifest.json")
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet" && git tag "v${first_version}")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q HEAD~1 -- manifest.json)
echo "first release" >>"${case_dir}/README.md"
commit_change
assert_fail_containing "first PR: version already tagged" "FAIL" "v${first_version}" "ALREADY TAGGED"

# --- case 24 (ut-docs#4484): no manifest.json at the merge-base OR the head
# -> PASS. A new repo's early PR (README, licence) before the manifest
# exists releases nothing, so there is nothing to bump.
fresh_repo
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
echo "early docs" >>"${case_dir}/README.md"
commit_change
assert_pass "no manifest.json at the merge-base or the head"

if [ "$FAILS" -ne 0 ]; then
    echo ""
    echo "$FAILS case(s) failed."
    exit 1
fi
echo ""
echo "All check-version-bump.sh cases passed."
