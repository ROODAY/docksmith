# Floating Tags & lscr.io Registry Routing — Design

Status: approved 2026-10-28
Date: 2026-10-28
Repo: fork `ROODAY/docksmith` (upstream `chrisae9/docksmith`)

## Intent

The user runs eight LinuxServer containers from `lscr.io/...:latest` (prowlarr, qbittorrent,
tautulli, radarr, plex, sonarr, librespeed, bazarr) and everything else on explicit version
tags. They want docksmith to make that end state deliberate: flag every container still on a
floating tag, suggest the exact tag to pin it to, and let them migrate in bulk so the pins and
`docksmith.version-pin-*` labels actually mean something.

Today docksmith never flags those eight. Radarr shows its current version
(`6.4.4.10685-ls324`) and a green pin (the user set `docksmith.version-pin-patch=true`), but no
PIN/Pinnable badge and no migration target. The state also hides a latent bug: if the pin label
were removed, the checker could recommend the newest tag it can see — an ancient
`3.0.0.4039-ls29` — as the tag to pin to.

## Background: how it works today

- **Statuses.** The checker (`internal/update/checker.go`) reports `UP_TO_DATE_PINNABLE` when a
  container is on `:latest`, up to date, and a semantic tag can be resolved for the running
  digest. `RecommendedTag` carries the target. The list shows a purple PIN badge and the
  container page shows a blue Pin action that rewrites the compose `image:` via the normal
  update flow.
- **Pin labels.** `docksmith.version-pin-major|minor|patch` only filter candidate *tags* in
  `findLatestVersion` (`checker.go:1113-1184`). For a `:latest` container the digest branch
  (`checker.go:615-707`) runs first, sets `digestCheckComplete`, and skips tag selection
  entirely. So on a floating tag the pin labels are inert — the image moves whenever `latest`
  moves.
- **Registry routing.** `registry.Manager.getClient` (`manager.go:227-236`) maps `docker.io`
  and `ghcr.io` to dedicated clients and everything else to the generic V2 `HTTPClient`.
  `lscr.io` is not mapped, so LinuxServer's vanity host uses the generic client.
- **Why lscr.io fails.** The generic client does one unpaginated `/tags/list` request
  (`client.go:91-147`). GHCR returns tags in ascending order, so the first page is the oldest
  100 (`amd64-v0.2.0.1504-ls81` … `3.0.0.4039-ls29`); `6.4.4.*` never appears, and the
  `Link: rel="next"` header is ignored. Its `ListTagsWithDigests` is unimplemented
  (`client.go:326-330`), so digest→tag resolution always fails. The GHCR client avoids this by
  using the GitHub Packages API (newest first) plus a V2 fallback, but that API requires
  authentication — anonymous requests return 401.
- **Credentials.** `GITHUB_TOKEN` is read in `cmd/docksmith/api.go:160`; if absent,
  `NewGHCRClient` reads `auths["ghcr.io"]` from `$HOME/.docker/config.json`
  (`ghcr.go:35-49`, `:106-144`). The container runs as root, so the path is
  `/root/.docker/config.json`. The user's container has neither a token nor a `ghcr.io` entry
  (only Docker Hub), and the generic client never reads Docker credentials at all
  (`manager.go:259` passes a nil config).
- **lscr.io is a registry-level alias of GHCR.** Verified: `lscr.io/v2/...` and `ghcr.io/v2/...`
  return the same manifest digest for `linuxserver/radarr:latest`, and lscr's auth challenge
  points at `realm="https://ghcr.io/token", service="ghcr.io"`. Any repository path is proxied,
  so the alias belongs at the registry level, not on the `linuxserver` owner.

## Goals

- `lscr.io/linuxserver/*` images get correct tag lists and digest→tag resolution, so radarr et
  al. become `UP_TO_DATE_PINNABLE` with a concrete target (`6.4.4.10685-ls324`).
- Every container on a floating tag is visibly flagged, even when no target can be resolved.
- A pin label on a floating tag is surfaced as inert, instead of looking like it is working.
- A bulk "migrate off floating tags" flow shows `current → target` per container and applies
  through the existing update flow (no new endpoint).
- Never recommend a tag older than the running version.

## Non-goals

- No generic V2 pagination work or per-tag digest crawling for arbitrary registries. A is a
  GHCR/lscr.io routing fix only.
- No new update endpoint; migration reuses `target_version` and the existing compose rewrite.
- No automatic migration. The user reviews and confirms the batch.
- No change to pin-label semantics (`version-pin-patch` still allows build/suffix changes).
- No auth system in docksmith; the PAT is a registry credential, not app login.
- No label-based fallback for registries or credentials that cannot resolve a tag. The
  supported path is registry credentials; without them a floating container is flagged but gets
  no migration target.

## Anti-goals (failure even if it "works")

- Recommending `3.0.0.4039-ls29` (or any older tag) as a pin target.
- Silently reporting a floating-tag container as plain up-to-date when the registry could not
  be read.
- Regressing images that resolve today. Without a PAT, lscr.io rows stay targetless as they are
  now, but must be flagged `FLOATING` instead of silently up-to-date.
- One registry failure (lscr, GHCR) slowing or failing checks for unrelated images.

## Constraints

- Go 1.25 backend, React 19/TypeScript UI. The UI has no test framework (`ui/package.json` has
  no test script); verify with `tsc -b` / `vite build` plus manual checks.
- Registry responses cached 15 min, digests 5 min, with a per-registry circuit breaker.
- GHCR Packages API requires a classic PAT with `read:packages` (verified 401 anonymously).
- The Docker config is read from `$HOME/.docker/config.json`; image runs as root.
- LinuxServer tag shapes: `6.4.4.10685-ls324`, arch-prefixed (`amd64-…`, `arm64v8-…`), and
  `version-…` variants. The parser already normalizes the `-lsNNN` build number and skips
  non-versioned prefixes.

## Approaches considered

### A. Registry access for lscr.io

1. **Alias `lscr.io` to the GHCR client (chosen).** Registry-level mapping in `getClient`;
   repository path unchanged. Gets newest-first tags and the Packages-API digest map. Requires a
   PAT for the efficient path; its V2 fallback is capped and still misses `6.4.4.*`.
2. **Add pagination and digest lookups to the generic V2 client.** Correct for every registry,
   but LSIO publishes thousands of tags and digest discovery would be one `HEAD` per tag.
   Expensive and unnecessary for this goal.
3. **Rewrite `lscr.io` to `ghcr.io` in the user's compose.** Rejected: invasive, changes what
   the user wrote, and does not help other lscr.io users.

### B. Recommendation when resolution fails

1. **Supported path: registry credentials plus a downgrade guard (chosen).** Require a
   `read:packages` PAT (env or Docker config) so GHCR's tag and digest APIs resolve a real
   target. No heuristic tag construction.
2. **Label-based fallback.** Build a candidate from the running image's version label and
   verify it with a V2 `HEAD`. Rejected: the user prefers the supported path, and heuristics can
   pin a stale or wrong tag.
3. **Keep today's behavior.** Rejected: produces the downgrade recommendation.

### C. UI surfacing

1. **Reuse `using_latest_tag` and existing fields (chosen).** Badges, warnings, and the bulk
   flow are computed in the UI; no new status or endpoint. `using_latest_tag`,
   `current_tag`, `recommended_tag`, `labels`, and `note` are already in the payload.
2. **New `FLOATING_TAG` status in the backend.** Rejected: adds state without new information;
   the UI already knows the tag is floating.

## Decisions

### A. lscr.io is a GHCR alias

- `Manager.getClient`: map registry `lscr.io` to `ghcrClient` (same client instance), with a
  comment that lscr.io is LinuxServer's proxy to GHCR. Do not rewrite image references; cache
  keys and circuit-breaker keys stay keyed on `lscr.io`.
- `readGHCRCredsFromDockerConfig`: try `auths["ghcr.io"]` first, then `auths["lscr.io"]`, so a
  user who ran `docker login lscr.io` is still authenticated.
- Docs: update the `GITHUB_TOKEN` row in `README.md` (not just private images — also required
  for reliable LinuxServer/lscr.io tag resolution), add an lscr.io note to
  `docs/registries.md` (alias, `read:packages` classic PAT, mount at `/root/.docker/config.json`),
  and fix the stale `/home/docksmith/.docker/config.json` mount in
  `docker-compose.example.yml` to `/root/.docker/config.json`.
- Expected result with a PAT: radarr's current digest resolves to `6.4.4.10685-ls324`,
  status `UP_TO_DATE_PINNABLE`, existing Pin/bulk update flow migrates the compose image tag.

### B. Never recommend older (downgrade guard)

- **Guard (always).** Apply the check where `LatestResolvedVersion`/`RecommendedTag` are
  assigned, not only to `RecommendedTag`, so the field shown in the UI and used as the
  notification target cannot carry an older tag. There are two sources:
  - Candidates from digest resolution (`resolveVersionFromDigest`, `checker.go:641`, `:672`,
    `:869`) point at the running digest and cannot be a downgrade; leave them alone.
  - Candidates from `findLatestVersion` (`checker.go:769-787`, `:920-933`) are accepted only
    when `CurrentVersion` parses and the candidate is equal to or newer. If `CurrentVersion` is
    empty or unparsable, do not set `LatestResolvedVersion`/`RecommendedTag` from a tag-list
    scan; leave the status up-to-date. This removes the downgrade hazard while preserving
    digest-resolved recommendations for label-less images.

### C. Floating-tag visibility and bulk migration

- **Floating definition.** "Floating" means any non-versioned tag, not just `latest`: `latest`,
  `latest-*`, and the meta tags the checker already recognizes (`stable`, `main`, `master`,
  `edge`, `nightly`, `dev`, `beta`, `rc`). The backend exposes a new boolean
  `using_floating_tag` alongside the existing `using_latest_tag`, so the UI does not duplicate
  the parser's rules. `docksmith.allow-latest=true` still suppresses the migration prompt.
- **Row badge.** When `using_floating_tag` is true, `docksmith.allow-latest` is not true, and
  the status is `UP_TO_DATE`, `METADATA_UNAVAILABLE`, or `CHECK_FAILED` (no recommendation),
  show an amber `FLOATING` badge with a tooltip naming the tag and explaining that no exact
  version tag could be resolved, plus how to get one (PAT/credentials).
  `UP_TO_DATE_PINNABLE` keeps the existing PIN badge and `tag → target` version display.
- **Inert pin warning.** When any `docksmith.version-pin-*` label is set and
  `using_floating_tag` is true, show a warning next to the existing pin icon ("Version pin has
  no effect while running a floating tag — migrate to a version tag first"). On the container
  page, show the same notice next to the "Pin to Version" control.
- **Selection summary.** Add `pinnableCount` to `selectedAnalysis`; stop bucketing pinnable
  containers as "Rebuild". Show a "N Pinnable" badge. When every actionable selected container
  is pinnable, label the primary action "Pin to recommended tags" and show a confirm list of
  `current → target` per container before navigating to the operation page.
- **Counters and filters.** Keep "updates" and "pinnable" separate. The sidebar total and the
  Updates filter count only `UPDATE_AVAILABLE`/`UPDATE_AVAILABLE_BLOCKED`. `UP_TO_DATE_PINNABLE`
  gets its own "Pinnable" count, badge, and filter. `isUpdatable` stays true for pinnable (it is
  actionable); add `isVersionUpdate` and `isPinnable` helpers and use those for counts/filters.
- **No-target containers.** Floating containers with no recommendation are not bulk-actionable;
  the summary reports them separately ("N on floating tags, no target") with a hint to provide
  registry credentials.
- Migration itself stays the existing operation: `target_version = recommended_tag`, compose
  `image:` rewritten to `repo:tag`, container recreated. After migration the pin labels become
  effective and future updates follow `version-pin-*`.
- Reference: `docs/handoffs/2026-10-28-ui-resilience-and-netns-ordering.md` item 6 already calls
  out the "Pin"/"Pinnable"/"Update" naming collision; this design implements its fix direction.

## Files expected to change

- Backend: `internal/registry/manager.go`, `internal/registry/ghcr.go`,
  `internal/registry/manager_test.go`, `internal/update/checker.go`, `internal/update/types.go`
  (the new `using_floating_tag` field) and checker tests (mock registry + table cases for the
  guard).
- UI: `ui/src/components/Containers.tsx` (badges, analysis, bulk flow),
  `ui/src/pages/ContainerPage.tsx` (FLOATING badge, inert-pin notice, no-target state),
  `ui/src/App.tsx` (separate updates/pinnable counters), `ui/src/utils/status.ts` (helpers),
  `ui/src/styles/*.css` (badge/notice styles), `ui/src/types/api.ts` (new field).
- Docs: `README.md`, `docs/registries.md`, `docker-compose.example.yml`.

## Risks and validation

- **Packages-API digest shape.** Confirm the version digest returned by the GitHub Packages API
  equals the multi-arch manifest-list digest Docker reports for a pulled image; otherwise
  digest→tag resolution will miss. Validate end-to-end with a real PAT on the user's host
  (radarr must show PIN with `6.4.4.10685-ls324`).
- **Rate limits and cache behavior.** Packages API is authenticated (5,000 req/h); negative
  results and empty lists are not cached today — check that repeated checks do not hammer the
  API, and that the circuit breaker is keyed sensibly for the alias.
- **No credentials.** Without a PAT, lscr.io rows remain targetless. C still flags them
  `FLOATING`; this is accepted behavior, not a regression from today's state.
- **Guard and existing tests.** The guard can change checker expectations where a tag-list scan
  previously produced `LatestResolvedVersion` from an unknown or older current version. Update
  those tests and add cases for candidate equal, newer, older, and current-version-unknown.
- **UI verification.** `npm run build` (tsc + vite) and a manual pass against a stack with a
  `:latest` LSIO image, with and without a recommendation.

## Success criteria

- On the user's host with a PAT: radarr, prowlarr, sonarr, plex, qbittorrent, bazarr, tautulli,
  and librespeed all show PIN with a concrete target tag.
- The bulk flow migrates all eight in one confirmed operation, rewriting `image:` to version
  tags; afterwards their pin labels govern updates.
- No container is ever offered an older tag as a pin target.
- With no PAT, those containers are at least flagged `FLOATING` rather than silently "up to date".

## Setup: GHCR credentials

1. Create a GitHub **classic** personal access token with the `read:packages` scope. Fine-grained
   tokens are fussier for another org's public packages; classic is the supported path.
2. On the Docker host:
   `echo "$PAT" | docker login ghcr.io -u <github-username> --password-stdin`. The docksmith
   compose must mount the host config at `/root/.docker/config.json:ro`. Alternatively set
   `GITHUB_TOKEN` in docksmith's environment; it takes precedence over the mounted config.
3. Restart docksmith — credentials are read once at startup (`NewGHCRClient`).
4. Docker persists the PAT until logout, config replacement, or PAT expiry/revocation; the
   PAT's own expiration date is the only clock. If a `credsStore`/`credHelpers` is configured,
   Docker may store the credential in the helper instead of the file, and docksmith will not
   see it — inline `auths` entries are required.
