# ADR-0008: Upload & Identify Pipeline

## Status

Proposed.

## Context

Users want to drop files into Crate by hand — e.g. an album obtained outside
Soulseek — and have them identified, tagged, and filed into the library the same
way downloaded tracks are. Today the only on-ramps are the download queue (which
picks the file itself) and the library importer (which adopts an existing
on-disk tree in place, never moves or tags). Neither fits: uploads need to
*arrive* somewhere unknown to the library, get *identified* even when tags are
missing or wrong, and land under the naming template with tags written.

Identification is the hard part, and its risk profile mirrors the reconcile
problem in [ADR-0007](0007-reconcile-local-import.md): automated matching can
silently mis-file a song, and the blast radius depends on which join is
automated. Three signal tiers exist, in descending confidence:

1. **Embedded MusicBrainz tags** (Picard/beets output). Real IDs; the importer
   already resolves these under the `musicbrainz` provider.
2. **Plain tags** (artist/album/title/duration, no MBIDs). Matchable against
   provider metadata, but title folds and edition drift can mis-bind.
3. **No usable tags.** Requires audio fingerprinting (Chromaprint → AcoustID →
   MusicBrainz recording ID), which adds an external API + `fpcalc` dependency.

A staged upload also interacts badly with existing jobs if it lands inside the
library tree: the importer would adopt half-tagged files, and the scheduler's
integrity check could observe them as `owned` rows. So staging must live outside
`CRATE_LIBRARY_PATH`.

## Decision

**A staged, human-in-the-loop pipeline: upload → identify → review → commit.**

1. **Staging is a scratch area outside the library.** Files land in a dedicated
   dir (new `CRATE_UPLOAD_DIR`, default under the data dir), one subdirectory
   per upload batch. Nothing in staging is ever a DB track; only commit creates
   rows. Stale batches are swept after a TTL.

2. **Identification is tag-first, fingerprint-second, and reuses the
   importer.** The importer's tag readers and (provider, provider_id)
   resolution already implement tiers 1–2 and the `musicbrainz`/`local`
   adoption rules. Extract that core (`tags.go` → entity resolution) into a
   shared helper rather than forking it, then apply it to staged files:
   - MBIDs present → resolve directly (tier 1).
   - Plain tags → group by album, match tracks to the provider album by
     title + duration tolerance (tier 2). Duration matching is new; the
     download scorer ignores duration today — same tolerance helper serves both.
   - No usable tags → `unidentified`, deferred to fingerprinting.

3. **Fingerprinting is an optional second pass behind config.** A new
   `internal/services/acoustid` client + `fpcalc` binary (added to the Docker
   image; feature disabled with a clear UI note when absent, keyed off a new
   `acoustid_api_key` setting). Fingerprint → AcoustID → recording ID lands on
   the provider side of the boundary via a new optional `MusicProvider` RPC
   (`GetRecording` or similar) — the main process stays provider-agnostic per
   the provider architecture; providers that can't resolve recordings return
   `Unimplemented`.

4. **Nothing commits without a review screen.** Following ADR-0003 (manual
   surfaces everything, the human picks) and ADR-0007 (the user anchors the
   risky join): proposed album/track matches are shown per file with confidence
   and reason; the user can retarget a file to any wanted track, mark it
   skipped, or re-pick the album. Album grouping is a UI convenience over the
   batch, not a pipeline assumption — stray singles work the same way.

5. **Commit is the download path, run backwards.** Each confirmed file goes
   through `organizer` (naming-template render, cross-device move,
   `library.Contains` safety) and `tagger.Tag` (non-destructive per
   [ADR-0004](0004-non-destructive-tagging.md)), then persists via the
   importer's claim machinery — `ClaimTrackFile` flips a matching `wanted`
   track to `owned` so an upload can satisfy the download queue directly.
   Duplicate-on-owned defaults to *skip*; `replace` is an explicit per-item
   choice in the review UI (quality-upgrade semantics, old file cleaned up
   through `removeReplacedFile`). Navidrome/MA notifiers fire on commit.

6. **Audit trail.** Activity-log entries for `upload_received`,
   `upload_identified`, `upload_committed`, `upload_failed`; staged-batch state
   (`uploaded → identified|unidentified → committed|skipped|failed`) is stored
   so the review UI survives restarts.

## Consequences

- A fully tagged album uploads, reviews, and files itself in one pass with zero
  new dependencies; fingerprinting is opt-in and degrades gracefully without
  `fpcalc`.
- The risky joins stay human-anchored; the worst automated outcome is an
  `unidentified` file the user handles by hand.
- Staging outside the library keeps the importer and integrity checker from
  ever seeing in-flight files; the cross-device copy path already exists in the
  organizer, so staging on the data volume costs at most one extra copy.
- The provider proto grows an optional RPC — a boundary-preserving extension,
  not a leak of identification into the core.
- A new settings key (`acoustid_api_key`) and env var (`CRATE_UPLOAD_DIR`) need
  README/site docs per the project's documentation convention.
