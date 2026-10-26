# 2026-10-26 — `approve.yml`: approve an uploaded release by id (ut-docs#5028)

## What changed

- `.github/workflows/approve.yml`: dispatch-only, main-only, runs
  `scripts/approve.sh` for one release id with the same secrets as
  `release.yml`. No upload, no verification code touched.
- `scripts/approve.sh`: `RELEASE_ID` may come from the environment (default
  unchanged: `dist/publish-response.json`); must be a UUID.

## Why

Releases 0.3.0, 0.3.1 and 0.4.0 were uploaded while production kept artifacts
on a container-local `file:///data/blob`; those blobs are gone, so the
approved 0.3.1 cannot load. 0.4.1 was uploaded to Azure Blob but never
approved (production keeps human review, `AUTO_APPROVE` unset). Re-running
the Release workflow would create a new row, not repair the old one.

## Independent review (Sonnet; author Sonnet-5.5 session)

No blocker or major. Minor: the `main` guard is in the file (write access
already allows the same via `release.yml` on a branch). Nits: lowercase-only
UUID regex (kept: the API issues lowercase), no timeout (fixed: 5 min).
Input reaches the script only via `env:`, UUID-validated; secrets at step
scope; `contents: read`.

## Gate

`scripts/check-workflows.sh` ok, `bash -n scripts/approve.sh` ok, bad id
rejected.
