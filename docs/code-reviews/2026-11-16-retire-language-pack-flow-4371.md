# Retire the language-pack flow (ut-docs#4371)

**What:** ADR-0160 section 9.3, owner rule "no leftovers". German, Spanish and Portuguese now ship as Weblate bundles (`com.universaltill.language-*`); the hand-made `ut-plugin-language-*` repos are archived. This repo drops its references to them.

**Change:** Comment-only reword of scripts/check-version-bump*.sh: the former language packs are no longer named as repos. Not in SHIPPED_PATTERNS, no version bump.

**Review:** self-review of the diff against the Architect design (WP-C); the pack-name grep is empty outside historical review records.
