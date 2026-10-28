# Docksmith follow-ups (2026-10-28) — Design

Status: draft for review
Date: 2026-10-28
Repo: `docksmith`, branch `main`
Input: `docs/handoffs/2026-10-28-followups.md`

## Intent

Four independent follow-ups from a production incident: the SPA can hang forever on a dead
connection, returning to the tab fires duplicate background refreshes, notification emails
carry no links because the Base URL is unset, and icon-only buttons have no reliable tooltip.

None of the four changes the other three; they can ship in any order.

## Goals

1. **No indefinite UI hang.** Every `fetchAPI` call has a deadline; the Containers page's
   initial load fails visibly (error banner) instead of spinning forever when the backend or
   proxy is unreachable. Long-running operations keep budgets large enough for the server, and
   their responses remain deliverable past the server's 120s write deadline.
2. **One refresh per trigger.** A tab return produces one `/api/trigger-check`, one
   `/api/explorer`, and one bounded `/api/status` poll loop, regardless of how many events
   (`visibilitychange`, `focus`, timer, SSE, reconnect) fire.
3. **Email links work, and their absence is visible.** A summary email with a configured Base
   URL links every item in both HTML and text parts. When services are configured and Base URL
   is blank, the Notifications settings page says so. Behavior is locked by Go tests.
4. **Every icon-only control has a visible tooltip** on hover and keyboard focus, plus a
   non-empty accessible name.

## Non-goals

- No one-click "update now" links; notification links stay GET-only navigation.
- No server-side derivation of the external URL, and no UI prefill from `window.location.origin`
  (user decision: keep it clean).
- Base URL does **not** become required, globally or conditionally.
- No changes to `/api/events` (`EventSource`) reconnection logic in `useEventStream`.
- No global `fetch` interception for operation pollers/executors; they already manage their own
  abort signals and polling.
- No UI unit-test runner will be added; Playwright specs are optional.
- No new tooltip dependency (no floating-ui/tippy).
- Tooltips are not added to non-interactive `title`-bearing spans (status badges, icon labels).

## Anti-goals (failure even if it "works")

- A tooltip that shifts layout, clips inside a popover, or leaves a duplicate native `title`
  tooltip behind.
- Refresh coalescing that silently drops a user-initiated Refresh click.
- A client timeout shorter than legitimate synchronous work (bulk container actions, prune),
  which would surface a timeout error and cancel the handler mid-way.
- A Base URL warning shown when no notification service exists (pure noise).

## Constraints

- Go 1.25 backend, React 19 + TypeScript UI (Vite), no UI unit-test runner.
- Baseline UI build passes (`npm run build`, verified 2026-10-28).
- Baseline Go suite on Windows has two pre-existing failures:
  `internal/update.TestCheckPermissions_FailsWithoutAccess` (assumes Unix permissions) and
  `internal/api.TestHandleOperations_WithData` (nil panic at `handlers_test.go:887`), plus
  `TestHandleOperations_StorageError`, `TestIntegration_Operations`, and
  `TestIntegration_StorageErrors`. Do not treat them as regressions; gate locally on the
  specific tests added/changed and run the full suite on Linux/CI.
- Baseline UI lint is dirty: `npm run lint` reports 42 errors and 6 warnings in files unrelated
  to this plan. Gate on "changed files add no new errors" (scoped eslint).
- `fetchAPI` (`ui/src/api/client.ts`) is the single request wrapper for most calls, but
  `/api/trigger-check` is called through raw `fetch` in two places.
- `internal/api/server.go:191` sets `http.Server.WriteTimeout = 120s`; only the SSE handler
  extends its write deadline. A synchronous handler that runs longer cannot send its response.
- Deployment is behind Traefik at `https://docksmith.engager.ts.rooday.com`; the UI is served
  from the same origin users browse.
- The UI is dark-only (no light theme in `ui/src/styles`); tooltip colors use existing tokens.

## Approaches considered

### 1. Client request timeouts

- **A (chosen):** default deadline inside `fetchAPI`, explicit longer budgets at slow call
  sites, and route `/api/trigger-check` through the wrapper. Review found the server's 120s
  `WriteTimeout` silently invalidates any client budget above 120s, so A also extends the
  server write deadline before responding (see Decisions).
- B: per-call timeouts only, no default — misses any forgotten call and does not fix the
  reported spinner class.
- C: a global `fetch` wrapper — drags in operation pollers/executors that already manage their
  own abort signals; more risk for no extra coverage.

### 2. Duplicate background refreshes

- **A (chosen):** a shared in-flight promise; automatic callers join it (plus a short
  debounce), explicit callers queue one trailing run. Nothing is dropped, and a second
  listener reuses the first run.
- B: boolean drop-guard — simplest, but silently discards the user's Refresh click and still
  allows a burst right after a run completes.

### 3. Base URL

- **A (chosen, reduced):** warning only when at least one service is configured and Base URL is
  blank. No prefill, no requirement.
- Prefill from `window.location.origin` was rejected by the user to keep the change small.
- Conditional/global requirement was rejected: the server can only validate "absolute http(s)",
  not reachability, so it is a rubber stamp.

### 4. Tooltips

- **A (chosen):** one shared `Tooltip` component, no dependency, applied to every icon-only
  control.
- B: `title` + `aria-label` only — the user reports `title` not appearing, so this fails the
  reported problem.
- C: add floating-ui/tippy — unnecessary for ~38 sites; new dependency and bundle cost.

## Decisions

### 1. Client request timeouts

**`fetchAPI` signature.** Extend the options type with `timeoutMs?: number`; destructure it out
before passing options to `fetch`. Keep the current success/failure shapes so existing
`if (!result.success)` branches work. On timeout return:

```ts
{ success: false, error: 'Request timed out', timestamp: new Date().toISOString(), version: '' }
```

**Mechanics.** Use an `AbortController` and a timer. Distinguish an intentional timeout from a
caller abort with a `timedOut` flag. Keep the timer armed until the response body has been
parsed, so a stalled body also times out. If a caller passes `signal`, abort the same
controller when it aborts. `timeoutMs: 0` disables the deadline (escape hatch; no call uses it).

**Server write deadline.** `WriteTimeout` is 120s. Past it, the response write fails and the
client sees a dropped connection even though the handler is still working. Fix globally: before
writing any response, reset the connection's write deadline to `now + responseWriteGrace` using
`http.NewResponseController(w).SetWriteDeadline(...)`. Put the helper in
`internal/api/respond.go` and share it by `RespondSuccess`, `RespondError`,
`RespondErrorWithData` and `RespondNoContent`. Define `serverWriteTimeout = 120s` and
`responseWriteGrace = serverWriteTimeout + 10s` in `internal/api/constants.go`, and use
`serverWriteTimeout` in `server.go`. The grace **must exceed** the server budget: `SetWriteDeadline`
replaces the deadline, so a 10s grace applied to a prompt response would cut its remaining 120s
budget to 10s. An invariant test (`responseWriteGrace > serverWriteTimeout`) pins this. Ignore
`ErrNotSupported` (httptest recorders and non-standard writers). The SSE handler keeps its own
`time.Time{}` extension. This changes no context deadlines and does not keep a dead connection
alive; a failed write still closes it.

**Budgets.**

| Budget | Calls |
|---|---|
| 15s default | explorer, status, operations/history, settings, docker-config, inspect, logs, scripts, notify CRUD/settings, cron validation, `triggerCheck`, **all label calls** (`setLabels`, `removeLabels`, `batchSetLabels`, `rollbackLabels` return an operation ID immediately; the work is async), async starters (`/update`, `/rollback`, `/update/batch`, `/restart/start/*`, `/fix-compose-mismatch`) |
| 120s (`LONG`) | `checkContainers`, `recheckContainer`, `getRegistryTags`, `testNotifyService` (server allows ~110s), single container stop/start/restart/remove, image/network/volume removal |
| 300s (`PRUNE`) | the four prune calls |
| 600s (`BATCH`) | `batchStartContainers`, `batchStopContainers`, `batchRestartContainers`, `batchRemoveContainers` (serial Docker calls; the cap is the fail-safe) |

Client budgets must exceed the expected work, because aborting the client cancels the handler's
`r.Context()` and any remaining Docker calls. The handoff's instruction to give `/api/labels/set`
a long timeout is obsolete: that endpoint responds immediately.

**Trigger-check.** Add `triggerCheck(): Promise<APIResponse<unknown>>` to the client and use it
in `useContainersData.ts` (background refresh) and `Settings.tsx`, replacing raw `fetch`.

**Background refresh failure path.** If the trigger request fails (timeout or HTTP error), skip
the 300ms poll loop (no check was started) and fall through to a single explorer/status fetch.
Status polls inside the loop use a 5s timeout so a hung status endpoint cannot blow past the
15s deadline.

**Out of scope.** `useEventStream` (`EventSource`), `useOperationPoller`, `useRecovery`,
`useSelfRestartRecovery`, and the operation executors. They keep their own abort/poll logic.

**Acceptance.** With the backend paused or the browser offline mid-load, the Containers page
shows the error banner ("Request timed out") within ~15s instead of loading forever. Label
operations and restarts still complete.

### 2. Refresh coalescing

In `useContainersData`:

- `inFlightRefresh = useRef<Promise<void> | null>(null)`
- `lastRefreshStart = useRef(0)`

`backgroundRefresh(force = false)`:

1. If a run is in flight:
   - automatic caller (`force=false`): return the in-flight promise (join, never drop);
   - explicit caller (`force=true`): await the in-flight promise, then re-check. If another
     explicit caller has already started a trailing run, join that one; otherwise start exactly
     one trailing run.
2. No run in flight: automatic callers skip if the last run started less than 1s ago (tail
   debounce); explicit callers never skip.
3. Record the start time, run the existing body, store the promise with
   `.finally(() => { inFlightRefresh.current = null })`, and return it.

Automatic callers: `visibilitychange`, `focus`, the 60s periodic timer. Explicit callers: the
exported `refresh` (Refresh button), `containerUpdated` with status `updated` (avoids joining a
run that started before the update finished), and `wasDisconnected` reconnection.
`containerUpdated` with any other status still calls `fetchData`. Reset both refs on unmount.

**Acceptance.** Switching away and back produces one trigger-check, one explorer fetch, and one
bounded status poll loop. A Refresh click always causes a run, even immediately after a
periodic or focus refresh. The 60s refresh still fires when the page stays visible.

### 3. Notification links and Base URL warning

**Rendering is already correct.** `Render` adds a per-row URL when `Settings.BaseURL` is a valid
http(s) URL (`internal/notify/render.go:105`), emits it in both HTML and text parts, and SMTP
sets `usehtml=yes`. No production Go change.

**UI warning.** In `ui/src/components/NotificationSettings.tsx`, directly after the Base URL
setting row, render when `services.length > 0 && draft.base_url.trim() === ''`:

```tsx
<div className="setting-info setting-info-warning" role="status">
  <i className="fa-solid fa-triangle-exclamation"></i>
  <span>Notification links are disabled because no Base URL is set. Set one and save so each
  item opens the matching container page.</span>
</div>
```

- The condition follows the unsaved field, so typing a URL clears the warning immediately.
  Wording is future-facing to match.
- Add `.setting-info-warning` to `ui/src/styles/settings.css` using
  `--color-warning-dim` / `--color-warning-bright`.
- Configured-but-disabled services count as "configured" and trigger the warning. (Call: the
  warning is about configuration, not delivery. Changing to `services.some(s => s.enabled)` is
  a one-line edit if preferred.)
- No prefill; no server-side requirement; the field can still be left blank deliberately.

**Go tests (characterization, pass before and after):**

- `internal/notify/render_test.go`: add a summary-mode case (`summary=true`) asserting the
  per-row URL in HTML and in the text part.
- `internal/notify/services_test.go:51` already asserts the built SMTP URL contains
  `usehtml=yes`; no new SMTP test is needed.

**Acceptance.** A summary email with Base URL set contains
`https://<base>/container/<name>` for each item in both parts; the settings page warns whenever
services exist and the field is empty.

### 4. Tooltips for icon-only controls

**Component.** `ui/src/components/shared/Tooltip.tsx` + `Tooltip.css`, exported from
`ui/src/components/shared/index.ts`.

```ts
interface TooltipProps {
  label: string;
  placement?: 'top' | 'bottom';   // default 'bottom'
  align?: 'start' | 'center' | 'end'; // default 'center'
  children: ReactElement;
}
```

**Implementation.**

- No wrapper element (a wrapper would break `position: absolute` children and flex-child
  selectors). `cloneElement` the single child:
  - merge a `tooltip-host` class;
  - set `aria-label` to `label`; the visible tooltip text is the single source of truth, so
    existing labels ("Actions", "Help for Ignore Container", …) are replaced by the inventory's
    wording. `ActionMenuButton` gains its context label at each call site and uses it for both
    the tooltip and `ariaLabel`;
  - leave any existing `aria-describedby` untouched; the accessible name from `aria-label`
    already carries the meaning, so the bubble is visual only;
  - append `<span aria-hidden="true" className="tooltip-bubble tooltip-{placement} tooltip-align-{align}">{label}</span>`
    to the child's children.
- CSS: `:where(.tooltip-host) { position: relative; }` (zero specificity, so an absolutely
  positioned child such as `.logs-scroll-btn` keeps its own `position`). Bubble is absolute,
  `pointer-events: none`, `z-index: var(--z-tooltip)`, background `--color-bg-elevated`, text
  `--color-text-primary`, border `--color-separator-opaque`, `box-shadow: var(--shadow-md)`,
  `font-size: var(--text-sm)`, radius `--radius-md`,
  `max-width: min(320px, calc(100vw - 16px))`, and wrapping with centered text so long dynamic
  labels (e.g. `Remove <container>`) do not clip at the viewport edge.
- Show on `.tooltip-host:hover > .tooltip-bubble` and
  `.tooltip-host:focus-visible > .tooltip-bubble`; hidden with `opacity`/`visibility` and
  `transition: var(--transition-fast)`.
- Placement/align rulings: header actions bottom/end; back buttons bottom/start; Toast dismiss
  and App banner dismiss bottom/end (both sit at the top of the viewport); logs action buttons
  bottom/end; logs scroll-to-bottom top/end; popover controls bottom/end; toolbar icons
  bottom/end; help buttons bottom/end; chip remove bottom/center.
- Remove the now-redundant `title` attribute on every converted element so the native tooltip
  does not double up.
- The logs scroll button gets `visibility: hidden` when it lacks `.visible`, so it leaves the tab
  order and its tooltip cannot appear while hidden.
- Disabled buttons suppress hover/focus in the browser, so a disabled control shows no tooltip
  while disabled. Accepted; nothing is lost because the action is unavailable.
- Help buttons keep their click-to-open inline panel; the hover tooltip is separate and
  uses the same topic wording.

**Sites.** Appendix A is the complete inventory (37 places: 36 icon-only buttons plus the
shared `ActionMenuButton` definition, which covers 6 uses; 38 if the text-glyph "×" dismiss
button is included — include it). Dynamic labels use current state (e.g.
`selected ? 'Deselect all' : 'Select all'`, `collapsed ? 'Expand all' : 'Collapse all'`).

**Acceptance.** Every converted control shows a visible tooltip on hover and on keyboard focus
and exposes a non-empty accessible name. No layout shift, no clipping inside popovers, no
duplicate native tooltip. The PR description lists the covered controls by file.

## Testing and verification

- **Go:** `go test ./internal/notify/...` and the specific new `./internal/api` test; full
  `go test ./...` on Linux/CI. The two Windows failures are pre-existing and unrelated.
- **Go, write deadline:** start an `httptest.NewUnstartedServer`, set
  `srv.Config.WriteTimeout = 100 * time.Millisecond`, `srv.Start()`, and register a handler that
  sleeps 300ms before `RespondSuccess`. Assert a 200 with the JSON body. Without the helper the
  response cannot be written; the test is the regression guard.
- **UI:** `cd ui && npm run build` must pass; lint is scoped to changed files because the
  repo-wide lint baseline is already dirty.
- **Manual, item 1:** pause the docksmith container (or go offline in DevTools) during the
  Containers initial load; confirm the error banner appears within ~15s and reload recovers.
- **Manual, item 2:** open the Network panel, switch tabs away and back; expect one
  `POST /api/trigger-check`, one `GET /api/explorer`, and a bounded `/api/status` loop.
- **Manual, item 3:** with services configured and Base URL blank, confirm the warning; type a
  URL and confirm it disappears immediately and stays gone after save; send a test/summary and
  confirm links in HTML and text.
- **Manual, item 4:** hover and Tab through each inventory entry, including controls inside the
  View Options popover, a toast, and the logs tab; confirm no clipping or layout shift.
- **Optional Playwright** (requires the live docker stack, not part of build/lint): a spec that
  dispatches `focus` twice and counts requests, and hover/focus assertions for a few tooltips.
  Recommended as a follow-up, not a ship blocker.

## What is left to the builder

- Exact bubble padding/offset values and any extra placement tweaks discovered in the browser
  (e.g. a toolbar button that sits close to a viewport edge).
- Whether to add the optional Playwright specs.
- The exact write-deadline extension (10s) and whether `RespondNoContent` needs it (it writes a
  header only, but include it for consistency).
- Commit grouping and order (suggested: item order 3 → 1 → 4 → 2, per the handoff).

## Appendix A — icon-only control inventory

Current state as of 2026-10-28. `title` values shown as they exist today; all are removed on
conversion in favour of the shared tooltip.

| file:line | element | current title | current aria-label | proposed label |
|---|---|---|---|---|
| `App.tsx:40` | `.active-operation-banner-dismiss` | — | Dismiss | Dismiss |
| `components/Containers.tsx:943` | `.stack-select-btn` | dynamic select/deselect | — | Select all in stack / Deselect all in stack |
| `components/Containers.tsx:1229` | `.explorer-settings-btn` | Settings | — | View options |
| `components/Containers.tsx:1258` | `.icon-btn.select-all-btn` | dynamic | — | Select all / Deselect all |
| `components/Containers.tsx:1262` | `.icon-btn` | dynamic expand/collapse | — | Expand all / Collapse all |
| `components/Containers.tsx:1267` | `.icon-btn.dashboard-settings-btn` | View Options | — | View options |
| `components/Containers.tsx:1276` | `.settings-close-btn` | — | — | Close |
| `components/Containers.tsx:831` | `ActionMenuButton` (container row) | — | Actions | More actions |
| `components/Containers.tsx:955` | `ActionMenuButton` (stack header) | — | Actions | Stack actions |
| `components/Explorer/ExplorerSettings.tsx:141` | `.settings-reset` | Reset to defaults | Reset to defaults | Reset to defaults |
| `components/Explorer/ImageItem.tsx:46` | `ActionMenuButton` | — | Actions | Image actions |
| `components/Explorer/NetworkItem.tsx:46` | `ActionMenuButton` | — | Actions | Network actions |
| `components/Explorer/VolumeItem.tsx:50` | `ActionMenuButton` | — | Actions | Volume actions |
| `components/Explorer/ContainerItem.tsx:84` | `ActionMenuButton` | — | Actions | Container actions |
| `components/shared/ActionMenu.tsx:19` | `ActionMenuButton` definition | — | Actions (default) | More actions |
| `components/shared/SearchBar.tsx:29` | `.clear-search` | — | Clear search | Clear search |
| `components/Toast/Toast.tsx:24` | `.toast-dismiss` | — | Dismiss | Dismiss |
| `pages/ContainerPage.tsx:554` | `.back-btn` (loading) | — | — | Back |
| `pages/ContainerPage.tsx:573` | `.back-btn` (error) | — | — | Back |
| `pages/ContainerPage.tsx:594` | `.back-btn` (main) | — | — | Back |
| `pages/ContainerPage.tsx:611` | `.action-btn` stop | Stop | — | Stop container |
| `pages/ContainerPage.tsx:619` | `.action-btn` restart | Restart | — | Restart container |
| `pages/ContainerPage.tsx:629` | `.action-btn` start | Start | — | Start container |
| `pages/ContainerPage.tsx:640` | `.action-btn.danger` confirm | Confirm remove | — | Confirm remove |
| `pages/ContainerPage.tsx:648` | `.action-btn` cancel | Cancel | — | Cancel remove |
| `pages/ContainerPage.tsx:658` | `.action-btn.danger` remove | Remove | — | Remove container |
| `pages/ContainerPage.tsx:1107` | error-banner "×" glyph | — | — | Dismiss error |
| `pages/ContainerPage.tsx:1114` | `.help-btn` | — | Help for Ignore Container | Help: Ignore container |
| `pages/ContainerPage.tsx:1139` | `.help-btn` | — | Help for Allow Latest Tag | Help: Allow latest tag |
| `pages/ContainerPage.tsx:1165` | `.help-btn` | — | Help for Version Pin | Help: Version pin |
| `pages/ContainerPage.tsx:1195` | `.help-btn` | — | Help for Tag Filter | Help: Tag filter |
| `pages/ContainerPage.tsx:1232` | `.help-btn` | — | Help for Notifications | Help: Notifications |
| `pages/ContainerPage.tsx:1257` | `.help-btn` | — | Help for Notification Level | Help: Notification level |
| `pages/ContainerPage.tsx:1289` | `.help-btn` | — | Help for Digest Notifications | Help: Digest notifications |
| `pages/ContainerPage.tsx:1319` | `.help-btn` | — | Help for Pre-Update Script | Help: Pre-update script |
| `pages/ContainerPage.tsx:1358` | `.precheck-refresh-btn` | Re-run pre-update check | — | Re-run pre-update check |
| `pages/ContainerPage.tsx:1380` | `.help-btn` | — | Help for Restart Dependencies | Help: Restart dependencies |
| `pages/ContainerPage.tsx:1476` | logs refresh | Refresh | — | Refresh logs |
| `pages/ContainerPage.tsx:1479` | logs download | Download | — | Download logs |
| `pages/ContainerPage.tsx:1491` | `.logs-scroll-btn` | Scroll to bottom | — | Scroll to bottom |
| `pages/RestartDependenciesPage.tsx:176` | chip remove | — | — | Remove {name} |
| `pages/RestartDependenciesPage.tsx:197` | `.clear-search` | — | — | Clear search |
| `pages/ScriptSelectionPage.tsx:150` | `.clear-search` | — | — | Clear search |
| `pages/TagFilterPage.tsx:239` | `.clear-input-button` | — | Clear pattern | Clear pattern |

Excluded: Explorer sub-tabs (visible count), TabBar (visible label), History copy-ID (visible
ID), and all buttons with visible text. Non-interactive `title`-bearing spans are out of scope.
