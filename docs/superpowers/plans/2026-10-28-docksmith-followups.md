# Docksmith Follow-ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix four follow-ups from the 2026-10-28 incident: hung UI requests, duplicate background refreshes, missing notification links, and missing tooltips on icon-only controls.

**Architecture:** Four independent changes. Item 1 adds client request deadlines and a server-side write-deadline extension so long synchronous handlers can still respond. Item 2 coalesces background refresh calls through one in-flight promise. Item 3 adds a Base URL warning and a summary-mode render test. Item 4 adds one shared `Tooltip` component and applies it to every icon-only control.

**Tech Stack:** Go 1.25 (`net/http`, testify), React 19 + TypeScript + Vite, plain CSS with existing design tokens.

**Spec:** `docs/superpowers/specs/2026-10-28-docksmith-followups-design.md`

## Global Constraints

- Do **not** commit, push, or create branches unless the user asks. Work stays in the working tree.
- Do **not** add dependencies. The tooltip is plain React + CSS.
- Do **not** touch `useEventStream`/`EventSource`, the operation pollers, or the operation executors; they keep their own abort logic.
- Base URL stays optional. No prefill. No server-side URL derivation.
- UI is dark-only. Tooltip colors come from existing tokens (`--color-bg-elevated`, `--color-text-primary`, `--color-separator-opaque`, `--shadow-md`, `--z-tooltip`).
- Verification commands:
  - `go test ./internal/notify/...`
  - `go test ./internal/api -run '<specific new test>'`
  - `cd ui && npm run build`
  - scoped eslint on the changed UI files
- Windows baseline failures (pre-existing, not ours): `internal/update.TestCheckPermissions_FailsWithoutAccess` (Unix permission assumption), and in `internal/api`: `TestHandleOperations_WithData` (nil panic at `handlers_test.go:887`), `TestHandleOperations_StorageError`, `TestIntegration_Operations`, `TestIntegration_StorageErrors`. Package-wide `go test ./internal/api/...` cannot pass locally; run the full suite on Linux/CI and gate locally on the specific tests added or changed.
- UI lint baseline is dirty: `npm run lint` reports 42 errors and 6 warnings in files unrelated to this plan. `npm run build` must pass; for lint, the gate is "changed files add no new errors" — run eslint scoped to the changed files (for example `npx eslint src/components/NotificationSettings.tsx`).
- The handoff's suggestion to give `/api/labels/set` a long timeout is obsolete: label endpoints return an operation ID immediately and do their work asynchronously. Use the default timeout for all label calls.

## Review Focus

1. **A response that sends headers then stalls its body** must still hit the client deadline. The timeout timer stays armed until `response.json()` finishes (Task 3, manual offline check).
2. **A synchronous handler still running after 120s** must still deliver its response. The Go `WriteTimeout` is extended before writing (Task 2, Go test).
3. **A Refresh click or an `updated` event during an in-flight refresh** must not be dropped and must not start a duplicate run; it queues exactly one trailing run (Task 8, manual network check).
4. **Tooltips near viewport edges, inside popovers, or on disabled controls** must not clip or shift layout; placements are fixed per site and disabled controls intentionally have none (Tasks 4–7, manual hover pass).
5. **The Base URL warning** must not fire for whitespace-only input, before services finish loading, or when no service exists (Task 1, trim condition + manual states).

---

### Task 1: Summary link test and Base URL warning

**Files:**
- Modify: `internal/notify/render_test.go`
- Modify: `ui/src/components/NotificationSettings.tsx` (Base URL row is around line 399–409)
- Modify: `ui/src/styles/settings.css` (variants around line 220–228)

**Interfaces:**
- Consumes: `Render(Settings, []Event, summary bool) Message` and `Settings.BaseURL`.
- Produces: no new API. UI reads existing `services` state and `draft.base_url`.

- [ ] **Step 1: Write the failing/characterization test**

Add to `internal/notify/render_test.go`:

```go
func TestRender_SummaryIncludesLinks(t *testing.T) {
	evs := []Event{
		{Kind: EventNewVersion, Container: "sonarr", FromVersion: "4.0.10", ToVersion: "4.0.11"},
		{Kind: EventNewVersion, Container: "cloudflare-ddns", FromVersion: "2.3.0", ToVersion: "2.3.1"},
	}
	m := Render(Settings{BaseURL: "https://docksmith.example.com"}, evs, true)

	for _, name := range []string{"sonarr", "cloudflare-ddns"} {
		want := "https://docksmith.example.com/container/" + name
		if !strings.Contains(m.HTML, `href="`+want+`"`) {
			t.Fatalf("summary HTML missing link for %s:\n%s", name, m.HTML)
		}
		if !strings.Contains(m.Text, want) {
			t.Fatalf("summary text missing link for %s:\n%s", name, m.Text)
		}
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/notify -run TestRender_SummaryIncludesLinks -v`
Expected: PASS. This is a characterization test — if it fails, the render path changed and the Task 1 UI work must stop.

- [ ] **Step 3: Add the warning**

In `ui/src/components/NotificationSettings.tsx`, immediately after the Base URL `setting-row` (the `<div>` containing `id="notify-base-url"`), add:

```tsx
{services.length > 0 && draft.base_url.trim() === '' && (
  <div className="setting-info setting-info-warning" role="status">
    <i className="fa-solid fa-triangle-exclamation"></i>
    <span>Notification links are disabled because no Base URL is set. Set one and save so each item opens the matching container page.</span>
  </div>
)}
```

It follows the unsaved `draft`, so typing clears it immediately. It counts configured services whether enabled or not.

- [ ] **Step 4: Add the warning style**

In `ui/src/styles/settings.css`, next to `.setting-info-success` / `.setting-info-error`:

```css
.setting-info-warning {
  background: var(--color-warning-dim);
  color: var(--color-warning-bright);
}
```

- [ ] **Step 5: Verify UI**

Run: `cd ui && npm run build` → exit 0. Run scoped `npx eslint <changed files>` → no new errors (the repo-wide lint baseline is dirty).

- [ ] **Step 6: Manual state check**

Open Notifications settings with: (a) no services → no warning; (b) one service + blank Base URL → warning visible; (c) whitespace-only Base URL → warning visible; (d) type a URL → warning disappears immediately.

---

### Task 2: Server write-deadline extension

**Files:**
- Create: `internal/api/respond_test.go`
- Modify: `internal/api/respond.go`

**Interfaces:**
- Consumes: `http.NewResponseController` (Go 1.20+), existing responders.
- Produces: unexported `extendWriteDeadline(w http.ResponseWriter)` used by all responders. No signature changes.

- [ ] **Step 1: Write the failing test**

Create `internal/api/respond_test.go` with `package api`:

```go
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRespondSuccess_ExtendsWriteDeadline(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		RespondSuccess(w, map[string]string{"ok": "yes"})
	}))
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"ok"`)
}
```

- [ ] **Step 2: Run it to confirm the failure**

Run: `go test ./internal/api -run TestRespondSuccess_ExtendsWriteDeadline -v`
Expected: FAIL — `http.Get` returns an error (EOF) because the 100ms write deadline expires while the handler sleeps.

- [ ] **Step 3: Implement the helper and call it**

In `internal/api/respond.go`:

```go
const responseWriteGrace = 10 * time.Second

// extendWriteDeadline refreshes the response write budget. A long synchronous
// handler can outlive http.Server.WriteTimeout, which would make the response
// unwritable even though the work succeeded.
func extendWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteGrace))
}
```

Call `extendWriteDeadline(w)` as the first statement of `RespondSuccess`, `RespondError`,
`RespondErrorWithData`, and `RespondNoContent`. Add the `time` import. Ignoring
`ErrNotSupported` keeps `httptest.ResponseRecorder`-based tests working.

- [ ] **Step 4: Run the test and the package**

Run: `go test ./internal/api -run TestRespondSuccess_ExtendsWriteDeadline -v`
Expected: PASS.
Run: `go test ./internal/api -run TestRespondSuccess_ExtendsWriteDeadline`
Expected: PASS. Do not gate on the package-wide suite: `TestHandleOperations_WithData` is a pre-existing Windows failure.

---

### Task 3: Client request timeouts

**Files:**
- Modify: `ui/src/api/client.ts`
- Modify: `ui/src/hooks/useContainersData.ts` (trigger call ~line 165; status polls ~line 175)
- Modify: `ui/src/components/Settings.tsx` (trigger call ~line 127)

**Interfaces:**
- Produces:
  - `fetchAPI<T>(endpoint: string, options?: APIFetchOptions): Promise<APIResponse<T>>` where `APIFetchOptions extends RequestInit { timeoutMs?: number }`.
  - `triggerCheck(): Promise<APIResponse<unknown>>` (POST `/trigger-check`).
  - `getContainerStatus(timeoutMs?: number): Promise<CheckResponse>`.
- Consumes: existing response shapes; callers keep their `if (!result.success)` branches.

- [ ] **Step 1: Implement the timeout wrapper**

At the top of `ui/src/api/client.ts`, add the budget constants and options type, then rewrite `fetchAPI`:

```ts
const DEFAULT_TIMEOUT_MS = 15_000;
const LONG_TIMEOUT_MS = 120_000;
const PRUNE_TIMEOUT_MS = 300_000;
const BATCH_TIMEOUT_MS = 600_000;

interface APIFetchOptions extends RequestInit {
  timeoutMs?: number;
}

async function fetchAPI<T>(endpoint: string, options?: APIFetchOptions): Promise<APIResponse<T>> {
  const { timeoutMs = DEFAULT_TIMEOUT_MS, signal: callerSignal, ...init } = options ?? {};
  const controller = new AbortController();
  let timedOut = false;
  const timer = timeoutMs > 0
    ? window.setTimeout(() => { timedOut = true; controller.abort(); }, timeoutMs)
    : null;
  const onCallerAbort = () => controller.abort();
  callerSignal?.addEventListener('abort', onCallerAbort);

  try {
    const response = await fetch(`${API_BASE}${endpoint}`, {
      ...init,
      headers: { 'Content-Type': 'application/json', ...(init.headers as Record<string, string> | undefined) },
      signal: controller.signal,
    });
    // ...existing response.ok / JSON handling unchanged...
  } catch (err) {
    if (timedOut) {
      return {
        success: false,
        error: 'Request timed out',
        timestamp: new Date().toISOString(),
        version: '',
      } as APIResponse<T>;
    }
    throw err;
  } finally {
    if (timer !== null) window.clearTimeout(timer);
    callerSignal?.removeEventListener('abort', onCallerAbort);
  }
}
```

Keep the timer armed through the body parse (the `finally` is outside the whole body), so a response that sends headers and then stalls still times out.

- [ ] **Step 2: Apply the budgets**

| Function | Option to add |
|---|---|
| `checkContainers`, `recheckContainer`, `getRegistryTags`, `testNotifyService` | `timeoutMs: LONG_TIMEOUT_MS` |
| `stopContainer`, `startContainer`, `removeContainer`, `removeNetwork`, `removeVolume`, `removeImage` | `timeoutMs: LONG_TIMEOUT_MS` |
| `pruneContainers`, `pruneImages`, `pruneNetworks`, `pruneVolumes` | `timeoutMs: PRUNE_TIMEOUT_MS` |
| `batchStartContainers`, `batchStopContainers`, `batchRestartContainers`, `batchRemoveContainers` | `timeoutMs: BATCH_TIMEOUT_MS` |

Pass the option as the second argument to `fetchAPI` (merge into the existing options object where one exists). Leave every other call untouched, including all label calls.

- [ ] **Step 3: Add `triggerCheck` and the status timeout parameter**

```ts
export async function triggerCheck(): Promise<APIResponse<unknown>> {
  return fetchAPI('/trigger-check', { method: 'POST' });
}

export async function getContainerStatus(timeoutMs?: number): Promise<CheckResponse> {
  return fetchAPI('/status', { timeoutMs });
}
```

- [ ] **Step 4: Use `triggerCheck` and skip a useless poll**

In `ui/src/hooks/useContainersData.ts`, replace the raw `fetch('/api/trigger-check', ...)` with
`const triggerRes = await triggerCheck();`. If `!triggerRes.success`, log the error and skip the
poll loop (no check was started); still do the single explorer/status fetch afterwards. In the
poll loop, call `getContainerStatus(5000)`.

In `ui/src/components/Settings.tsx`, replace the raw trigger `fetch` with `triggerCheck()` and
throw on `!result.success` so the existing `catch` sets the page error.

- [ ] **Step 5: Verify**

Run: `cd ui && npm run build` → exit 0. Run scoped `npx eslint <changed files>` → no new errors (the repo-wide lint baseline is dirty).

- [ ] **Step 6: Manual check**

In DevTools, set the network to Offline during the Containers page's initial load. The error
banner ("Request timed out") appears within ~15s; reloading online recovers.

---

### Task 4: Tooltip component and shared ActionMenuButton

**Files:**
- Create: `ui/src/components/shared/Tooltip.tsx`
- Create: `ui/src/components/shared/Tooltip.css`
- Modify: `ui/src/components/shared/index.ts`
- Modify: `ui/src/components/shared/ActionMenu.tsx`

**Interfaces:**
- Produces: `Tooltip({ label, placement?: 'top'|'bottom', align?: 'start'|'center'|'end', children: ReactElement })`. Children must be a single DOM element that accepts `className`, `aria-label`, `aria-describedby`, and `children`.
- Consumes: design tokens.

- [ ] **Step 1: Create the component**

`Tooltip.tsx`:

```tsx
import { cloneElement, isValidElement, useId, type ReactElement, type ReactNode } from 'react';
import './Tooltip.css';

export interface TooltipProps {
  label: string;
  placement?: 'top' | 'bottom';
  align?: 'start' | 'center' | 'end';
  children: ReactElement;
}

type TooltipChildProps = {
  className?: string;
  children?: ReactNode;
  'aria-label'?: string;
  'aria-describedby'?: string;
};

export function Tooltip({ label, placement = 'bottom', align = 'center', children }: TooltipProps) {
  const id = useId();
  if (!isValidElement(children)) return children;
  const child = children as ReactElement<TooltipChildProps>;
  const props = child.props;
  const className = [props.className, 'tooltip-host'].filter(Boolean).join(' ');
  const describedBy = [props['aria-describedby'], id].filter(Boolean).join(' ');

  return cloneElement(child, {
    className,
    'aria-label': label,
    'aria-describedby': describedBy,
    children: (
      <>
        {props.children}
        <span id={id} role="tooltip" className={`tooltip-bubble tooltip-${placement} tooltip-align-${align}`}>
          {label}
        </span>
      </>
    ),
  });
}
```

No wrapper element: the bubble is the child's own child, so flex layout and
`position: absolute` children (the logs scroll button) are unaffected.

- [ ] **Step 2: Create the CSS**

`Tooltip.css`:

```css
:where(.tooltip-host) { position: relative; }

.tooltip-bubble {
  position: absolute;
  z-index: var(--z-tooltip);
  pointer-events: none;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-md);
  background: var(--color-bg-elevated);
  color: var(--color-text-primary);
  border: 1px solid var(--color-separator-opaque);
  box-shadow: var(--shadow-md);
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  line-height: var(--leading-tight);
  white-space: nowrap;
  opacity: 0;
  visibility: hidden;
  transition: opacity var(--transition-fast), visibility var(--transition-fast);
}

.tooltip-bottom { top: calc(100% + 6px); }
.tooltip-top { bottom: calc(100% + 6px); }
.tooltip-align-center { left: 50%; transform: translateX(-50%); }
.tooltip-align-start { left: 0; }
.tooltip-align-end { right: 0; }

.tooltip-host:hover > .tooltip-bubble,
.tooltip-host:focus-visible > .tooltip-bubble {
  opacity: 1;
  visibility: visible;
}
```

` :where()` keeps zero specificity, so `.logs-scroll-btn`'s `position: absolute` still wins.

- [ ] **Step 3: Export it**

Add `export { Tooltip } from './Tooltip';` to `ui/src/components/shared/index.ts`.

- [ ] **Step 4: Apply it to `ActionMenuButton`**

In `ui/src/components/shared/ActionMenu.tsx`, import `Tooltip` from `./Tooltip`, change the
default `ariaLabel` to `'More actions'`, and wrap the returned `<button>`:

```tsx
return (
  <Tooltip label={ariaLabel} placement="bottom" align="end">
    <button ...existing props...>...</button>
  </Tooltip>
);
```

This covers all six `ActionMenuButton` uses in one edit.

- [ ] **Step 5: Verify**

Run: `cd ui && npm run build` → exit 0; scoped `npx eslint <changed files>` → no new errors.
Open any container row's actions menu button: hover and Tab to it; the tooltip appears with the
label. No layout shift.

---

### Task 5: ContainerPage tooltips

**Files:**
- Modify: `ui/src/pages/ContainerPage.tsx`
- Modify: `ui/src/styles/container-page.css` (logs scroll button)

**Interfaces:**
- Consumes: `Tooltip` from Task 4.
- Produces: none.

- [ ] **Step 1: Back buttons and header actions**

Wrap each icon-only `<button>` in `<Tooltip>` and remove its `title` attribute:

| Line (approx) | Label | Placement / align |
|---|---|---|
| 554, 573, 594 | `Back` | bottom / start |
| 611 | `Stop container` | bottom / end |
| 619 | `Restart container` | bottom / end |
| 629 | `Start container` | bottom / end |
| 640 | `Confirm remove` | bottom / end |
| 648 | `Cancel remove` | bottom / end |
| 658 | `Remove container` | bottom / end |

- [ ] **Step 2: Error banner and help buttons**

| Line (approx) | Label | Placement / align |
|---|---|---|
| 1107 (text "×" button in `error-banner`) | `Dismiss error` | bottom / end |
| 1114 | `Help: Ignore container` | bottom / end |
| 1139 | `Help: Allow latest tag` | bottom / end |
| 1165 | `Help: Version pin` | bottom / end |
| 1195 | `Help: Tag filter` | bottom / end |
| 1232 | `Help: Notifications` | bottom / end |
| 1257 | `Help: Notification level` | bottom / end |
| 1289 | `Help: Digest notifications` | bottom / end |
| 1319 | `Help: Pre-update script` | bottom / end |
| 1358 (`precheck-refresh-btn`) | `Re-run pre-update check` | bottom / end |
| 1380 | `Help: Restart dependencies` | bottom / end |

Help buttons keep their existing `onClick`/`aria-label` behaviour; the Tooltip replaces the
accessible name with the same topic wording. Remove `title` where present (only the precheck
button has one).

- [ ] **Step 3: Logs buttons and scroll button**

| Line (approx) | Label | Placement / align |
|---|---|---|
| 1476 | `Refresh logs` | bottom / end |
| 1479 | `Download logs` | bottom / end |
| 1491 | `Scroll to bottom` | top / end |

Remove the `title` attributes. In `ui/src/styles/container-page.css`, add visibility gating so the
hidden scroll button leaves the tab order and cannot show its tooltip:

```css
.container-page .logs-scroll-btn { visibility: hidden; }
.container-page .logs-scroll-btn.visible { visibility: visible; }
```

- [ ] **Step 4: Verify**

Run: `cd ui && npm run build` → exit 0; scoped `npx eslint <changed files>` → no new errors.
On a container page, hover and Tab through the header actions, a help button, the logs actions,
and the scroll-to-bottom button (make logs overflow first). The scroll button's tooltip appears
above it and only while visible.

---

### Task 6: Dashboard and Explorer tooltips

**Files:**
- Modify: `ui/src/components/Containers.tsx`
- Modify: `ui/src/components/Explorer/ExplorerSettings.tsx`
- Modify: `ui/src/components/Explorer/ImageItem.tsx`
- Modify: `ui/src/components/Explorer/NetworkItem.tsx`
- Modify: `ui/src/components/Explorer/VolumeItem.tsx`
- Modify: `ui/src/components/Explorer/ContainerItem.tsx`

**Interfaces:**
- Consumes: `Tooltip` and the updated `ActionMenuButton` from Task 4.
- Produces: none.

- [ ] **Step 1: Containers toolbar and popover**

Wrap these buttons and remove their `title` attributes:

| Line (approx) | Label | Placement / align |
|---|---|---|
| 943 | `selected ? 'Deselect all in stack' : 'Select all in stack'` | bottom / end |
| 1229 | `View options` | bottom / end |
| 1258 | `selectedContainers.size > 0 ? 'Deselect all' : 'Select all'` | bottom / end |
| 1262 | `collapsedStacks.size > 0 ? 'Expand all' : 'Collapse all'` | bottom / end |
| 1267 | `View options` | bottom / end |
| 1276 | `Close` | bottom / end |

Dynamic labels must read the same state the `title` used.

- [ ] **Step 2: Explorer settings and action menus**

- `ExplorerSettings.tsx:141`: wrap with `Tooltip label="Reset to defaults" placement="bottom" align="end"`; keep the existing `aria-label`.
- `ImageItem.tsx:46`, `NetworkItem.tsx:46`, `VolumeItem.tsx:50`, `ContainerItem.tsx:84`: pass `ariaLabel="Image actions"`, `"Network actions"`, `"Volume actions"`, `"Container actions"` respectively to `ActionMenuButton`. The shared component renders the tooltip.
- `Containers.tsx:831`: pass `ariaLabel="More actions"`; `Containers.tsx:955`: pass `ariaLabel="Stack actions"`.

- [ ] **Step 3: Verify**

Run: `cd ui && npm run build` → exit 0; scoped `npx eslint <changed files>` → no new errors.
Hover and Tab through the dashboard toolbar, the View Options popover, and an Explorer action
menu. No clipping.

---

### Task 7: Remaining tooltips

**Files:**
- Modify: `ui/src/App.tsx`
- Modify: `ui/src/components/shared/SearchBar.tsx`
- Modify: `ui/src/components/Toast/Toast.tsx`
- Modify: `ui/src/pages/RestartDependenciesPage.tsx`
- Modify: `ui/src/pages/ScriptSelectionPage.tsx`
- Modify: `ui/src/pages/TagFilterPage.tsx`

**Interfaces:**
- Consumes: `Tooltip` from Task 4.
- Produces: none.

- [ ] **Step 1: Wrap each control**

| File:line | Label | Placement / align |
|---|---|---|
| `App.tsx:40` | `Dismiss` | bottom / end |
| `components/shared/SearchBar.tsx:29` | `Clear search` | bottom / end |
| `components/Toast/Toast.tsx:24` | `Dismiss` | bottom / end |
| `pages/RestartDependenciesPage.tsx:176` | `Remove ${name}` | bottom / center |
| `pages/RestartDependenciesPage.tsx:197` | `Clear search` | bottom / end |
| `pages/ScriptSelectionPage.tsx:150` | `Clear search` | bottom / end |
| `pages/TagFilterPage.tsx:239` | `Clear pattern` | bottom / end |

For `RestartDependenciesPage.tsx:176`, the chip's name is a sibling outside the button; use the
container name in the label. Keep any existing `aria-label` behaviour by letting Tooltip set it
from `label`. Import `Tooltip` with the correct relative path in each file.

- [ ] **Step 2: Verify**

Run: `cd ui && npm run build` → exit 0; scoped `npx eslint <changed files>` → no new errors.
Trigger a toast and hover its dismiss button; the tooltip appears below it, on-screen.

---

### Task 8: Refresh coalescing

**Files:**
- Modify: `ui/src/hooks/useContainersData.ts`

**Interfaces:**
- Produces: `backgroundRefresh(force = false): Promise<void>` internally; the hook's returned
  `refresh` stays `() => Promise<void>` and always forces.
- Consumes: `triggerCheck` and `getContainerStatus(timeoutMs?)` from Task 3.

- [ ] **Step 1: Implement the guard**

Add two refs next to `mountedRef`:

```ts
const inFlightRefresh = useRef<Promise<void> | null>(null);
const lastRefreshStart = useRef(0);
```

Rename the existing body to `runRefresh` (`useCallback(async () => { ...existing body... }, [])`).
Then:

```ts
const startRefresh = useCallback(() => {
  lastRefreshStart.current = Date.now();
  const run = runRefresh().finally(() => { inFlightRefresh.current = null; });
  inFlightRefresh.current = run;
  return run;
}, [runRefresh]);

const backgroundRefresh = useCallback((force = false): Promise<void> => {
  const current = inFlightRefresh.current;
  if (current) {
    if (!force) return current;
    return current.then(() => {
      if (inFlightRefresh.current) return inFlightRefresh.current;
      return startRefresh();
    });
  }
  if (!force && Date.now() - lastRefreshStart.current < 1000) return Promise.resolve();
  return startRefresh();
}, [startRefresh]);

const refresh = useCallback(() => backgroundRefresh(true), [backgroundRefresh]);
```

Reset `inFlightRefresh.current = null` in the unmount cleanup. Return `refresh` (not
`backgroundRefresh`) in the hook result.

- [ ] **Step 2: Mark explicit callers**

- Visibility and focus listeners: `backgroundRefresh()` (automatic).
- `usePeriodicRefresh(backgroundRefresh, ...)`: unchanged (automatic).
- `containerUpdated` with status `updated`: `backgroundRefresh(true)`.
- `wasDisconnected`: `backgroundRefresh(true)`.
- The hook's returned `refresh` is already forced; `Containers.tsx` callers need no change.

- [ ] **Step 3: Verify**

Run: `cd ui && npm run build` → exit 0; scoped `npx eslint <changed files>` → no new errors.

- [ ] **Step 4: Manual network check**

With the Network panel open: switch tabs away and back → exactly one `POST /api/trigger-check`,
one `GET /api/explorer`, and a bounded `/api/status` loop. Click Refresh immediately after a
background refresh completes → a new trigger-check still appears. Wait 60s on a visible page →
the periodic refresh still fires.

---

### Task 9: Final verification

- [ ] **Step 1: Go tests**

Run: `go test ./internal/notify/...` and `go test ./internal/api -run TestRespondSuccess_ExtendsWriteDeadline`
Expected: PASS. The package-wide `./internal/api/...` run has the pre-existing `TestHandleOperations_WithData` failure on Windows.

- [ ] **Step 2: Full Go suite (Linux/CI only)**

Run on Linux/CI: `go test ./...`
Expected: PASS. On Windows, `internal/update.TestCheckPermissions_FailsWithoutAccess` is a known
pre-existing failure.

- [ ] **Step 3: UI build and lint**

Run: `cd ui && npm run build` → exit 0. Run scoped `npx eslint <changed files>` → no new errors (the repo-wide lint baseline is dirty).

- [ ] **Step 4: Manual acceptance pass**

- Item 1: Offline mid-load → error banner within ~15s; online reload recovers. A batch container
  action that takes over 120s still returns a result (or the Go test proves the deadline
  extension).
- Item 2: one trigger per tab return; Refresh click always runs; periodic refresh fires.
- Item 3: warning states behave; a real summary email contains per-item links in HTML and text.
- Item 4: hover and Tab through every Appendix A control; visible tooltip, accessible name, no
  layout shift, no clipping, no native `title` duplicate.

- [ ] **Step 5: Report**

Summarize the changed files, the verification output, and the intentionally uncommitted state.
Ask whether to commit and on which branch.

## Post-review amendments (final fix wave)

The final whole-branch review found one Important defect and several Minors. Applied fixes:

- The write-deadline grace now shares `serverWriteTimeout` from `internal/api/constants.go` and is
  `serverWriteTimeout + 10s`. A bare 10s grace shortened a prompt response's remaining 120s
  budget; the invariant test `responseWriteGrace > serverWriteTimeout` pins the relationship.
- `responseWriter.Unwrap()` added in `internal/api/middleware.go`, so the logging middleware
  cannot silently disable the deadline extension.
- The tooltip bubble is `aria-hidden` and no longer sets `aria-describedby`; the accessible name
  already carries the label. Long labels wrap within `min(320px, 100vw - 16px)`.
- Inert duplicate `aria-label`s removed from converted controls.
- `fetchAPI` builds headers with `new Headers(init.headers)` so any `HeadersInit` works.
- Refresh cleanup resets `lastRefreshStart` and refuses to start a run after unmount.

These supersede the corresponding wording in Tasks 2, 3, 4, 5–7, and 8 above.
