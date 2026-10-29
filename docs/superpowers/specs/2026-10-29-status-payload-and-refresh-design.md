# Design: Slim status payload, capped checks, and a refresh indicator

Date: 2026-10-29

## Why

The deployed instance (`docksmith.engager.ts.rooday.com`, currently `index-CRELfhd3.js`)
hangs intermittently, showing "Request timed out" and blank/stale pages.

Measured on the live instance (20 containers):

- `GET /api/status` is **573,402 B** and costs **~0.6 s** of server CPU. `/api/explorer`,
  which talks to Docker, is 23 KB / ~0.32 s.
- Every container is serialised **twice**: once in `containers` and again inside `stacks`
  (198 KB of duplication the UI never reads).
- Each copy carries `available_tags` (up to 23 KB/container, **322 KB total**); no UI code
  reads it (`TagFilterPage` uses `/api/registry/tags/{imageRef}`).
- The JSON is pretty-printed; compact would be 397 KB.
- The Containers page fetches status twice on mount (badge in `App.tsx` + list in
  `useContainersData`), and each tab triggers a full server check every 60 s.
- Under 20 concurrent connections, large responses (including a static file, so not the JSON
  handler) stall for 25 s+ on the Tailscale→Traefik path. The 15 s client timeout then
  surfaces "Request timed out"; a cold page has no snapshot, so it stays blank until the next
  refresh succeeds.

The `file:///` console error is not produced by docksmith; no `file:` URL exists in the app.
It is Firefox blocking a navigation to a local document (the browser back button returning to
a locally-opened email). Out of scope.

## Goals

1. Cut `/api/status` to the data the UI actually uses (~38 KB raw, ~5 KB gzipped).
2. Make the JSON response a single compact write with `Content-Length`.
3. Compress API JSON and static text assets, since the proxy does not.
4. At most one **client-triggered** check per 60 s across all tabs and devices, without
   client-side coordination.
5. Keep explicit refreshes immediate; add a visible "last refreshed / checking" control,
   which also gives desktop an explicit refresh affordance.
6. Show skeletons in the container page while its detail data loads.

## Non-goals

- Streaming per-container status (the payload is served from an in-memory snapshot; there is
  no incremental compute to stream). Rows already paint from `/api/explorer` before status
  arrives; badges are gated on `has_update_data`.
- Cross-tab `localStorage`/`BroadcastChannel` coordination (the server is authoritative).
- Changing `CHECK_INTERVAL` or the CLI output format.
- Removing `stacks`/`available_tags` from `/api/check` (kept full for external clients).

## Approaches considered

- **A (chosen): slim + capped auto-checks + gzip + UI polish.** Server slims `/api/status`,
  compacts JSON, adds a gzip middleware, and caps non-forced `trigger-check` calls at one per
  60 s. UI dedupes simultaneous status requests, honours `{triggered}`, adds the refresh
  indicator, and uses section skeletons on the container page. Addresses bytes, CPU, check
  stampedes, and perceived latency.
- **B: minimal.** Slim `/api/status` + compact JSON only. Small and safe, but leaves
  per-tab check stampedes, uncompressed responses, and the bare spinner.
- **C: progressive streaming.** NDJSON/SSE per-container status. Rejected: the data is
  already cached server-side, so streaming does not reduce time-to-data; the cost is bytes
  and serialisation, which A removes.

## Decisions

### Server

1. **`/api/status` slims the cached result.** `handleGetStatus` builds the existing deep copy
   but sets `Stacks` to nil and nils `AvailableTags` on every container in `Containers` and
   `StandaloneContainers` (both tags use `omitempty`). All other fields — including
   `update_order`, labels, counts, timestamps, `checking` — are preserved. `/api/check` keeps
   the full shape (a `slim bool` parameter on the status writer).
2. **Compact JSON with `Content-Length`.** API responses are marshalled compactly
   (`output` gains a compact encoder); `respond.go` sets `Content-Length` and writes the
   bytes once. CLI output keeps the indented encoder.
3. **gzip middleware.** Enables gzip when the client sends `Accept-Encoding: gzip`, for
   `application/json` and static text types (`text/html`, `text/css`, `text/javascript`,
   `application/javascript`, `image/svg+xml`) above a small size threshold. It never wraps
   `text/event-stream` (SSE) or `text/plain` (log streams), honours `gzip;q=0`, sets
   `Vary: Accept-Encoding`, drops `Content-Length` when compressing, and implements
   `Unwrap()` so `http.ResponseController` (write deadlines, flushing) keeps working.
4. **Capped client checks.** `BackgroundChecker` gains an atomic claim:
   - `checkInternal` (container-update debounce, `/api/check`): if a check is running, set
     `pendingRecheck`; otherwise claim.
   - `checkClient(force=false)`: if running, return false **without** queueing a rerun; if
     `now - lastBackgroundRun < 60 s`, return false; otherwise claim.
   - `checkClient(force=true)`: if running, return false (join the running check); otherwise
     claim regardless of the interval.
   - The periodic `checkLoop` uses `checkInternal`.
   `POST /api/trigger-check` takes `?force=true`, calls `checkClient(force)`, and returns
   `{triggered, checking, last_background_run}`.

### UI

5. **Status request dedupe.** `getContainerStatus` shares an in-flight promise (module-level),
   so the badge and the list request on mount collapse into one HTTP request.
6. **Honour `{triggered}`.** `runRefresh` calls `triggerCheck(force)`. If `triggered` is
   false, it skips the status poll and refetches immediately; if true, it polls `/api/status`
   every 1 s (was 300 ms) up to 15 s, then refetches explorer + status. Automatic
   (60 s / focus / visibility) refreshes pass `force=false`; pull-to-refresh and Settings →
   Background Refresh pass `force=true`; pull ≥120 / Cache Refresh keep using `/api/check`.
7. **Refresh indicator.** In the Containers header: spinner + "Checking…" when
   `statusData.checking`; otherwise "Checked {relative}" from `last_background_run`, refreshed
   every 30 s, next to a refresh icon button that forces a background refresh. This is the
   first desktop-visible refresh control. The container list continues to render from
   `/api/explorer` while status is absent.
8. **Container page skeletons.** While `loading` with no cached inspect, render the header
   plus `SkeletonDetailSection` blocks instead of a bare spinner, keeping the
   "waiting / Reconnecting / Retry now" copy.

## Constraints

- `/api/status`'s documented consumers are the UI and the Homepage widget
  (`data.total_checked`, `data.updates_found`, `data.last_cache_refresh`); all are retained.
- Existing pull-to-refresh thresholds (70 px background, 120 px cache) and the Settings
  refresh buttons keep their meaning; only the stale floor is new.
- No new dependencies.

## Testing and verification

- Go: status response omits `stacks`/`available_tags`, keeps counts/labels/timestamps, and
  `Content-Length` equals the body length; `/api/check` stays full; compact JSON is valid.
- Go: stale guard — first client trigger runs, an immediate second is a no-op, `force` runs,
  an internal trigger during a run queues exactly one follow-up; `/api/trigger-check` returns
  `triggered` correctly.
- Go: gzip middleware compresses JSON/static when accepted, skips SSE and `text/plain`, and
  sets `Vary`.
- UI: `npm run build`, lint changed files.
- Live verification after deploy: `/api/status` ≤ ~40 KB (≤ ~6 KB gzipped), ~0.05 s CPU;
  five concurrent `POST /api/trigger-check` produce one run; indicator shows relative time;
  no blank page when status is slow.

## Left to the builder

- Exact gzip size threshold and the content-type allowlist.
- Whether `/api/check`'s full response should also be compact (yes) while keeping its fields.
- Names/signatures of the compact encoder and the claims helpers.
- Whether the indicator lives in the header's top row or beside the title; keep it unobtrusive
  on mobile.
