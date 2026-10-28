# Handoff: UI resilience during self-inflicted outages + shared-netns ordering (2026-10-28)

Follow-up to `docs/handoffs/2026-10-28-followups.md`. That document covered request
timeouts, duplicate refresh bursts, email links, and tooltips. This one covers what the
production testing session exposed **after** those fixes were written but before they were
deployed.

**Repo:** `docksmith`, branch `main`. There is a large **uncommitted working tree** (see
"State of the working tree" at the end). Build on it; do not revert it.

**Verification**

```bash
go test ./...
cd ui && npm run build && npx eslint <changed files>   # no UI unit-test runner
```

Known Windows/local baseline failures (pre-existing, not yours): `internal/update.TestCheckPermissions_FailsWithoutAccess`,
and in `internal/api`: `TestHandleOperations_WithData`, `TestHandleOperations_StorageError`,
`TestIntegration_Operations`, `TestIntegration_StorageErrors`. Package-wide
`go test ./internal/api/...` cannot pass locally on Windows; rely on Linux/CI for that package.
Repo-wide `npm run lint` is dirty (42 errors, 6 warnings in unrelated files); the gate is
"changed files add no new errors".

**Deployment context (for manual reproduction only)**

- `engager` stack lives at `/home/rooday/docker-compose/engager` (top-level
  `docker-compose.yaml` uses `include:` of `network-infra.yaml`, `private-services.yaml`,
  `public-services.yaml`). It is a git checkout of `ROODAY/docker-compose`; docksmith writes
  label edits directly into that working tree, so it is usually dirty.
- Docksmith is served at `https://docksmith.engager.ts.rooday.com` through
  **tailscale → traefik-ts**. `traefik-ts` runs with `network_mode: service:tailscale`
  (`network-infra.yaml`), so recreating `traefik`, `traefik-ts`, or `tailscale` makes the UI
  unreachable while it happens.
- `docksmith` itself is `docksmith.ignore=true`, so it is excluded from update batches, but
  it can still be selected for label/pin actions.
- The Docker config mount added on 2026-10-28 lives in the **infra repo**, not here
  (`/home/rooday/.docker/config.json:/root/.docker/config.json:ro` in
  `engager/private-services.yaml`).

---

## Observed incident timeline (from the live `/api/operations`)

All times UTC, 2026-10-28, taken from the instance's own operation records:

| Time | Operation | Duration | Notes |
|---|---|---|---|
| 21:18:26–21:18:33 | `fix_mismatch` cleanuparr | 7s | user pressed **Done** afterwards |
| 21:21:57–21:22:09 | batch update gluetun, kometa, flaresolverr | 12s | |
| 21:22:20–21:22:21 | `label_change` (pin) kometa, gluetun, flaresolverr | **<1s each** | pin actually applied |
| 21:24:03–21:25:27 | batch update of **13 containers**, incl. `traefik`, `traefik-ts`, `tailscale` | **84s** | `qbittorrent` failed |
| 21:25:30–21:26:03 | `fix_mismatch` PlexAutoLanguages | 33s | |

**Symptom.** The browser showed "a broken page": a skeleton, with `/api/status`,
`/api/explorer`, and `/api/trigger-check` all sitting *pending* for 1–2 minutes, then the UI
appeared normally. The 84s window (including both Traefik instances and Tailscale) matches
the reported duration; the pin itself completed in under a second and cannot explain it.

`qbittorrent` failure text from the 13-container batch:

```
Compose recreation failed: docker compose up failed: exit status 1
Output:  Container qbittorrent  Creating
 Container qbittorrent  Created
 Container qbittorrent  Starting
Error response from daemon: cannot join network namespace of container:
Container 42fb503e6b6f... is restarting, wait until the container is running
```

That container is `gluetun`, which `qbittorrent` uses as `network_mode: service:gluetun`.

---

## 1. Neither the list nor the container page may blank when docksmith becomes unreachable (P1)

**Cause.** Both pages paint-gate on a single fetch and have no stale-data fallback.

Containers list:

- `ui/src/hooks/useContainersData.ts:132-162` — `fetchData()` awaits
  `Promise.all([getExplorerData(), getContainerStatus()])`; `loading` is cleared only in the
  promise's `finally`.
- `ui/src/components/Containers.tsx:1125-1162` — while `loading`, the whole page is replaced
  by a skeleton.
- `ui/src/components/Containers.tsx:1164-1177` — the error branch only renders if
  `containers.length === 0`, so a stale list cannot be shown on a failed refresh.

Container detail page (reported as "clicking into a container hangs"):

- `ui/src/pages/ContainerPage.tsx:286-289` — mount calls `fetchInspect()` and
  `fetchDocksmithData()`.
- `ui/src/pages/ContainerPage.tsx:140-154` — `loading` is cleared **only** in
  `fetchInspect`'s `finally`, i.e. the whole page waits on
  `GET /api/containers/{name}/inspect`.
- `ui/src/pages/ContainerPage.tsx:551-569` — while `loading`, the body is a bare spinner.
- `ui/src/pages/ContainerPage.tsx:572-592` — the error state uses
  `window.location.reload()` as "Retry", and only renders when `error && !inspectData`.
- `fetchDocksmithData` (`:157-224`) adds `GET /api/status` (313KB) and
  `GET /api/labels/{name}`; neither gates rendering, but both fail silently.

**This is not a server-side lock.** Measured on the live instance (idle): all 20 containers'
`/api/containers/{name}/inspect` return 200 in **0.23–0.9s** (network RTT dominates), and
`/api/explorer` in ~0.32s. When the path to docksmith is down (proxy/tunnel recreate) or the
Docker daemon is saturated (batches run up to 12 concurrent `docker compose` invocations), the
request simply stays pending — and with the deployed client's unbounded `fetch`, so does the
spinner. The server-side handlers have no Docker-call deadline either
(`internal/api/handlers_container_ops.go:246-266` uses `r.Context()` directly), so a stalled
daemon can hold the request up to the server's 120s `WriteTimeout`.

So one slow or unreachable request blanks the entire UI, and there is no "reconnecting"
affordance even though `useEventStream` already exposes `connected`, `reconnecting`, and
`wasDisconnected` (`ui/src/hooks/useEventStream.ts:33-36`, consumed through
`ui/src/context/EventStreamContext.tsx`).

**Fix direction.**

- Render last-known data while refreshing. Keep a module-level (or App-level context)
  last-good `{explorerData, statusData}` snapshot so remounting `/` after an operation paints
  immediately instead of showing a skeleton. Only show the skeleton when no snapshot exists.
- Do the same for `ContainerPage`: cache the last `ContainerInspect` per container name,
  render the header/tabs immediately, and let the body show cached data (or a skeleton only
  for that region) while `inspect` is in flight. The loading state does render a Back button
  (`:555-559`), so the page is escapable, but it shows a bare spinner with no indication of
  what it is waiting on — which reads as a freeze.
- Decouple the two list calls: `loading` should clear when either source returns, and the page
  should render whatever data it has.
- Surface connectivity: a small persistent banner (reuse `.active-operation-banner` styles in
  `App.tsx:32-45`) or a header chip when `!connected || reconnecting`: "Reconnecting to
  Docksmith…". Do not auto-reload.
- The working tree already adds per-request timeouts and refresh coalescing — do not redo
  those. They turn an infinite hang into a failed request, but the page must still show
  stale data when that failure happens.

**Acceptance.**

- With DevTools offline (or while `traefik-ts` is being recreated), the container list stays
  visible with a reconnecting indicator; when connectivity returns the list refreshes in
  place, no reload.
- A cold first load with no network still shows an error state (not an infinite skeleton).
- Pressing **Done** on `/operation` returns to `/` immediately, showing cached data.
- Clicking into a container while the proxy is down (or while a batch is recreating
  containers) shows the page shell and either cached details or an explicit "waiting for
  response" state — never a bare spinner that only resolves on reload.

---

## 2. Warn (and order last) when a batch targets the UI's own entry path (P2)

**Context.** `traefik`, `traefik-ts`, and `tailscale` are selectable in the bulk toolbar like
any app container. Recreating any of them intentionally disconnects the UI. Users should know
before they click, and the operation should not look "stuck" afterwards.

**Confirmed topology.** `docksmith.engager.ts.rooday.com` is a CNAME for
`engager.ts.rooday.com`, which resolves to **100.108.62.114** — a Tailscale address
(100.64.0.0/10). So the whole site lives behind the `tailscale` container's tailnet node, and
`traefik-ts` shares that netns. Recreating `tailscale` therefore removes the site's route for
as long as the node takes to reconnect (tens of seconds), and recreating `traefik-ts` removes
the proxy.

**Observed consequence.** The bulk update finishing at 21:41:49Z included `tailscale`,
`traefik`, `cloudflare-ddns`, and `PlexAutoLanguages`. While it ran, opening a `/container/...`
link from an email in a **new tab** failed with a server-not-available/DNS-style browser error,
then worked once the batch completed. Note the limitation: the stale-data/reconnecting UX from
item 1 cannot help a new tab or an external link — the only mitigations for that case are not
recreating the ingress as part of a routine batch, warning before doing so, and keeping that
window as short as possible (ingress last).

An already-open `/operation` page does not lose the operation: the work runs server-side on
`context.Background()` and keeps going. The page freezes at the last SSE event it received
(no new activity-log lines, progress stalls), then catches up when connectivity returns —
`useOperationPoller` keeps polling `/api/operations/{id}` at 2s while SSE is disconnected, and
`OperationProgressPage` re-fetches the batch group on `wasDisconnected`
(`ui/src/pages/OperationProgressPage.tsx:68-99`). Terminal per-container statuses are
restored, but **stage log lines emitted during the outage are not replayed** — the log jumps
from the last pre-outage entry to the completion entries. That is acceptable for a transient
ingress restart; document it rather than trying to replay stage events.

**Fix direction.**

- Identify ingress/self containers. Two workable options:
  - Client-side: from the merged container data, flag containers whose image/name is a
    Traefik or Tailscale instance, and whose labels show `traefik.enable=true`; plus the
    container named `docksmith` (or expose `is_self` / `is_ingress` on the API types — the
    server already has `selfupdate.IsSelfContainer` and `ContainerExplorerItem` in
    `internal/docker/service.go:466-475`).
  - Server-side on `/api/explorer` and/or `/api/status` so the definition lives in one place.
- In the bulk-actions panel (`ui/src/components/Containers.tsx:1430-1450`) show an inline
  warning when an ingress container is selected: "This container serves the Docksmith UI.
  You will lose access until it restarts."
- In `executeBatchUpdate` (`internal/update/update_orchestrator.go:1099-1173`), after the
  existing dependency ordering, sort ingress containers **last** so one disconnect covers the
  fewest possible operations. The dependency graph already orders `network_mode` providers
  first (`internal/graph/parser.go:64-100`); this is an additional, independent ordering.
- The operation page already handles a dropped SSE (`useSelfRestartRecovery`,
  `pending_restart`); extend the message for ingress containers so it says
  "Waiting for traefik-ts to come back" rather than looking stalled.

**Acceptance.** Selecting `traefik-ts` shows a warning before the action; the operation page
keeps a coherent message through the disconnect and recovers without a reload.

---

## 3. `/api/explorer` is uncached and pays for volume disk usage on every request (P2)

**Cause.** `handleExplorer` recomputes everything per request
(`internal/api/handlers_explorer.go:23-90`), including
`ListVolumesWithContainers` → `s.cli.DiskUsage(...)` for volume sizes
(`internal/docker/service.go:400-429`). Idle cost measured on the live instance: ~0.3s
(24KB); under daemon churn (12 concurrent recreations, image pulls) it can be much worse, and
it is the call the Containers page gates on.

**Fix direction.**

- Add a short-TTL (2–5s) single-flight cache around the explorer computation, keyed on the
  whole endpoint. Concurrent requests share one Docker round-trip.
- Or make volume sizes lazy/optional (`?include_volume_sizes=false` by default, with the
  Volumes tab requesting them), so the containers list never waits on `disk usage`.
- Keep the response shape unchanged; `ExplorerResponse` is consumed in
  `ui/src/api/client.ts` and `ui/src/types/api.ts`.

**Acceptance.** Ten concurrent `GET /api/explorer` calls produce one Docker `DiskUsage` call
(instrument or fake client in a unit test); cached responses stay under the TTL; the
Containers page does not lose data when one sub-call is slow.

---

## 4. Retry/order around restarting `network_mode` providers (P1 correctness)

**Cause.** `RecreateWithCompose` unconditionally does `docker stop`, `docker rm`, then
`docker compose up -d --force-recreate --no-deps <service>`
(`internal/compose/recreate.go:46-89`). If the target has
`com.docker.compose.network_mode=service:X` and `X` is not running (e.g. it was just recreated
or is crash-looping), the daemon rejects the start with
`cannot join network namespace of container: ... is restarting, wait until the container is
running`. There is no retry, so the whole container is marked failed.

The dependency graph understands `network_mode` **inside a batch**, but not an external
provider that happens to be restarting (here `gluetun`, updated ~2 minutes earlier by a pin).

**Fix direction.**

- Before creating a container, inspect its `com.docker.compose.network_mode` label. If it is
  `service:X`, wait (bounded, e.g. up to 60s, 1s poll) until `X` reports
  `State.Running == true` before invoking compose. Fail with a message naming `X` if it never
  comes up.
- Additionally, retry the compose command on exit status 1 whose output contains
  `is restarting, wait until the container is running` (bounded backoff). This covers races
  between the pre-check and the actual start.
- Apply the same protection to the label path: `restartContainerByService`
  (`internal/api/handlers_labels.go:625-661`) calls the same recreator.
- This must not mask a genuinely unhealthy provider: after the retry budget, fail the
  container and surface "provider X is not running" in `batch_details`.

**Tests.** Unit-test the retry classifier with a fake command runner (no Docker needed):
first invocation returns the daemon error text, second succeeds; and the provider pre-check
with a fake inspect that reports `Restarting` then `Running`. Add a graph/ordering test only
if item 2's ingress-last ordering touches shared code.

**Acceptance.** A batch that includes a dependent whose netns provider is mid-restart either
succeeds after the provider settles or fails with a message naming the provider — never with
a bare `exit status 1`.

---

## 5. Label batches can race on the same compose file (P2, verify first)

Bulk label actions start one background goroutine per container
(`internal/api/handlers_labels.go:952-1021` → `executeLabelOperationAsync:309-379`). Each
goroutine runs load → modify → `composeFile.Save()` on whichever file owns its service. Two
services in the same file (e.g. `gluetun` and `flaresolverr` both live in
`private-services.yaml`) therefore do a concurrent read-modify-write, and whichever saves
last wins. The 2026-10-28 pin happened to keep all three labels, but that is luck, not
design.

**Fix direction.** Serialize label operations per resolved compose file (or per stack) with a
keyed mutex, and re-read the file inside the critical section before saving. Add a unit test
that performs two concurrent modifications of one temp compose file and asserts both labels
survive.

**Acceptance.** Concurrent `POST /api/labels/batch` for two services in the same file leaves
both labels present.

---

## 6. Bulk "migrate off a floating tag" exists but is invisible (P2, UX only)

**It already works.** `UP_TO_DATE_PINNABLE` is counted as updatable
(`ui/src/utils/status.ts:4-10`), so selecting those containers and pressing the bulk **Update**
button sends them through the normal update flow with
`target_version: c.recommended_tag` (`ui/src/components/Containers.tsx:464-523`,
button at `:1453-1458`). The container detail page's blue **Pin** button
(`ui/src/pages/ContainerPage.tsx:763-792`) does the same thing for one container.

**Why nobody can tell.** In the selection summary the pinnable containers are bucketed as
`rebuildCount` (their `change_type` is NoChange/UnknownChange) and show a **"Rebuild"** badge
(`ui/src/components/Containers.tsx:1100-1122`, `:1397-1406`); the button says only "Update";
the row action menu also says "Update" (`:842-844`). The only place the action is called
"Pin" is the least discoverable one, and it now collides with the label-based
Pin/Unpin actions in the bulk Actions dropdown (`:1438-1442`).

**Fix direction (naming, not new backend work).**

- Add `pinnableCount` to `selectedAnalysis` and a distinct badge, e.g. "N Pinnable" /
  "N Migrate off :latest".
- When every actionable selected container is pinnable, label the primary button
  accordingly ("Pin to recommended tags") and show a small confirm listing
  `current_tag → recommended_tag` per container.
- Keep the two concepts visibly separate: the Actions dropdown's "Pinning" section operates
  on **labels** (`docksmith.version-pin-*`), while migrating a `:latest` container changes
  the **compose image tag**. Rename one of them (e.g. "Version pin" vs "Pin to tag") so the
  same thumbtack does not mean two different things.
- Reuse the update operation; do not add a new endpoint.

**Acceptance.** Selecting only pinnable containers produces a clearly labelled action that
shows the target tags before confirming, and the resulting history entry records an update to
`recommended_tag`.

---

## State of the working tree (do not discard)

Uncommitted work already implements most of `2026-10-28-docksmith-followups.md`:

- `ui/src/api/client.ts` — `fetchAPI` timeout/AbortController + per-endpoint budgets
  (`DEFAULT_TIMEOUT_MS`, `LONG_TIMEOUT_MS`, etc.).
- `ui/src/hooks/useContainersData.ts` — refresh coalescing/debounce (`runRefresh`,
  `startRefresh`, `backgroundRefresh(force)`), `getContainerStatus(5000)` in the poll loop.
- `ui/src/components/shared/Tooltip.*`, `ActionMenu`, `ContainerPage` header buttons,
  `NotificationSettings`, `Settings`, `ContainerPage`/`Containers` cleanups.
- `internal/api/respond.go` + `respond_test.go`, `internal/notify/render_test.go`.

Build on top of these. The operation pollers/executors
(`ui/src/operation/hooks/useOperationPoller.ts`, `executors/*`) keep their own abort logic —
do not fold them into `fetchAPI`.

## Suggested order

1. Item 4 (correctness; small, testable without Docker).
2. Item 1 (biggest UX win; builds directly on the uncommitted `useContainersData` work).
3. Item 2 (needs item 1's banner to be meaningful).
4. Item 3 (server-side cache; independent).
5. Item 6 (pure naming/discoverability; pairs well with item 2's warnings).
6. Item 5 (verify with a test before changing anything).
