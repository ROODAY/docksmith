# Handoff: Docksmith follow-ups (2026-10-28)

Four independent follow-ups from a production incident session. Each is self-contained;
they can be done in any order. Read the whole item before starting — item 3 in particular
is probably a configuration issue rather than a code change.

**Repo:** `docksmith` (branch `main`). Use the normal project workflow: failing test first
for Go changes, `npm run build && npm run lint` for UI changes.

**Verification commands**

```bash
go test ./...
cd ui && npm run build && npm run lint     # no UI unit-test runner is configured
```

**Deployment context (for manual verification only):** the `engager` stack runs docksmith
from `/home/rooday/docker-compose/engager` (top-level `docker-compose.yaml` uses `include:`),
served at `https://docksmith.engager.ts.rooday.com` through `traefik-ts`.

---

## 1. `fetchAPI` has no timeout — a dead connection leaves the UI loading forever (P1)

**Symptom.** After the proxy restarted, the SPA sat on its spinner indefinitely. Reloading
after the proxy settled fixed it. Console showed `Background refresh failed: TypeError:
NetworkError when attempting to fetch resource.` and Traefik logged `499` for an aborted
`POST /api/trigger-check`.

**Cause.** `fetchAPI` in `ui/src/api/client.ts:23` calls `fetch` with no `AbortController`
and no deadline. The Containers page's initial load is:

```ts
// ui/src/hooks/useContainersData.ts:129
const [explorerRes, statusRes] = await Promise.all([getExplorerData(), getContainerStatus()])
```

`loading` only clears in the `finally` of that promise. If either request is opened just as
the proxy is torn down and never resolves, the promise never settles and the page is stuck
until the browser gives up — or the user reloads.

**Fix.** Add an optional timeout to `fetchAPI`:

- Default it (e.g. 15s) for normal API calls.
- Return the same shape callers already handle: `{success: false, error: "Request timed out", ...}`
  so existing `if (!result.success)` branches work.
- **Do not apply the short default to known long-running calls.** Pass a longer explicit
  timeout (or opt out) at these call sites:
  - `POST /api/labels/set` — rewrites the compose file, restarts/recreates a container.
  - `POST /api/restart/container/{name}` and `POST /api/restart/stack/{name}` — synchronous,
    with health-check waiting (~60s budget).
  - `GET /api/check` (`checkContainers`) — runs a live registry check.
  - `POST /api/notifications/services/{id}/test` — SMTP/webhook delivery.

**Acceptance.** Simulating a hung backend (e.g. `docker pause` the container, or DevTools
offline mid-load) makes the initial load fail with a visible error instead of spinning forever.
`/api/events` is an `EventSource` (`ui/src/hooks/useEventStream.ts:68`), not `fetchAPI` — leave
its reconnection logic alone.

---

## 2. Duplicate background refresh bursts (P2)

**Symptom.** Returning to the tab fires two overlapping refresh cycles. Observed in the
Traefik access log within ~3s: 2× `POST /api/trigger-check`, 2× `GET /api/explorer`,
4× `GET /api/status`.

**Cause.** `useContainersData.ts:236-247` registers handlers for **both**
`visibilitychange` and `window.focus`; both fire on the same tab switch. Each calls
`backgroundRefresh()` (`:162`), which POSTs `/api/trigger-check`, polls `/api/status`
every 300ms for up to 15s, then re-fetches explorer + status. Two runs interleave.

**Fix.** Add an in-flight guard (a `useRef<boolean>` reset in `finally`) so concurrent
`backgroundRefresh()` calls are dropped, and/or debounce the two listeners (skip if the
last run started <1s ago). `usePeriodicRefresh` (`ui/src/hooks/usePeriodicRefresh.ts:39`)
calls the same function every 60s and should respect the guard too.

**Acceptance.** Switching away and back produces one `/api/trigger-check`, one
`/api/explorer`, and one bounded poll loop. The 60s periodic refresh still fires.

---

## 3. Scheduled summary email has no per-item links (P1 — check config first)

**Important: the feature already exists.** `Render` adds a per-row URL whenever
`Settings.BaseURL` is a valid http(s) URL:

- `internal/notify/render.go:105-107` — `row.URL = base + "/container/" + url.PathEscape(name)`
- HTML template emits `<a href="{{.URL}}">Open in Docksmith</a>` per row (`:59`)
- Text part emits the URL on its own line (`:176-178`)
- Summary/global buttons: `/` and `/history` (`:112-121`)
- SMTP actually receives HTML: `sender.go:39-42` picks `msg.HTML` for `smtp`, and
  `buildSMTP` sets `usehtml=yes` (`internal/notify/services.go:235`). Shoutrrr v0.18.0 then
  sends `multipart/alternative` (text + HTML).

Links are gated by `linkBase` (`render.go:203`): empty/invalid/relative/query/fragment URLs
produce no links. The setting is exposed in the UI at
`ui/src/components/NotificationSettings.tsx:399-409` ("Base URL").

**First step — verify configuration.** Open Settings → Notifications → Base URL and confirm
it is set to `https://docksmith.engager.ts.rooday.com`. If it is empty, that alone explains
the missing links; setting it is the fix. Only continue to code changes if it is set and
links still do not appear (then check the SMTP service config and the rendered HTML in
`render_test.go` style, and the service's `last_error`).

**If you want links to work without manual configuration**, the smallest sensible change is
UI-side: when the Base URL field is empty, prefill it with `window.location.origin` on the
settings page (the UI is served from the same origin users browse docksmith at). Do not try
to derive it server-side — docksmith cannot know its external URL behind a proxy.

**Optional polish (if requested):** make the container name itself the anchor in the HTML
row (currently a trailing "Open in Docksmith" cell) so the link is more obvious in email
clients that strip table cell content. Keep the text part's URL line.

**Out of scope.** Links must stay GET-only navigation. A one-click "update now" link would
need an authenticated mutation endpoint and is deliberately not part of this.

**Acceptance.** A summary email for a `new_version` event contains
`https://<base>/container/<name>` for each item in both the HTML and text parts; the
container page is where updates are triggered (`App.tsx:232` route, update buttons on
`ContainerPage`).

**Tests.** `internal/notify/render_test.go` already has `TestRender_LinksOnlyWithBaseURL`;
add a summary-mode case (`summary=true`) asserting the per-row URL is present, and a text-part
assertion. `internal/notify/services_test.go` should assert the SMTP URL contains `usehtml=yes`.

---

## 4. Hover tooltips for every icon-only button (P2)

**User report.** They clicked the container restart button without knowing what it did and
said there was no tooltip on hover. Note the header action buttons on `ContainerPage` already
carry `title` attributes (`Restart` at `:623`, `Stop` `:615`, `Start` `:633`, `Remove` `:662`),
so native `title` may be too slow/inconsistent in their browser. Treat "a visible tooltip"
as the requirement, not "a `title` attribute exists".

**Strict icon-only inventory** (buttons whose entire content is one `<i>`):

| File:line | Action | `title` | `aria-label` |
|---|---|---|---|
| `ui/src/pages/ContainerPage.tsx:554` | Back (loading state) | — | — |
| `ui/src/pages/ContainerPage.tsx:573` | Back (error state) | — | — |
| `ui/src/pages/ContainerPage.tsx:594` | Back (normal) | — | — |
| `ui/src/pages/ContainerPage.tsx:640` | Confirm remove | `Confirm remove` | — |
| `ui/src/pages/ContainerPage.tsx:1479` | Download logs | `Download` | — |
| `ui/src/pages/ContainerPage.tsx:1491` | Scroll logs to bottom | `Scroll to bottom` | — |
| `ui/src/components/Toast/Toast.tsx:24` | Dismiss | — | `Dismiss` |
| `ui/src/components/Explorer/ExplorerSettings.tsx:141` | Reset to defaults | `Reset to defaults` | `Reset to defaults` |

**Recommended approach.** Build one small shared tooltip component
(`ui/src/components/shared/Tooltip.tsx`) that wraps a button, shows a positioned label on
hover **and** focus, and sets `aria-describedby`. Use it for the eight buttons above and for
the `ContainerPage` header action buttons (Stop/Start/Restart/Remove/Cancel/Confirm), so the
restart button the user complained about gets a reliable tooltip. If a full component is too
much, the minimum acceptable change is `title` + `aria-label` on all eight plus the header
actions — but verify in a real browser that the tooltip actually appears, since that is the
reported failure.

**Notes.**
- The app already has a settings-help pattern: `.help-tooltip` in
  `ui/src/styles/container-page.css:1166` (used with `activeHelp` state in `ContainerPage`).
  Reuse its look or extract it, but do not reuse the click-to-toggle behaviour for hover
  tooltips.
- Tooltips must not intercept pointer events (`pointer-events: none`) and must be readable
  in both themes.
- Keep wording action-oriented and, for state-dependent buttons, label the action
  (`Stop`, `Start`, `Restart`) rather than the icon.

**Acceptance.** Every icon-only button in the app shows a visible tooltip on hover and on
keyboard focus, and has a non-empty accessible name. Add a short note in the PR listing the
buttons covered so a reviewer can spot any missed.

---

## Suggested order

1. **Item 3** — confirm the Base URL first; it may be a five-minute configuration fix.
2. **Item 1** — the real robustness gap from the incident.
3. **Item 4** — user-visible, independent.
4. **Item 2** — low risk, small, do last.

## Context you do not need to re-litigate

The incident that prompted this was **not** a docksmith bug: `traefik-ts` only routes to the
`private` Docker network and was dialing docksmith's `public` address because neither Traefik
instance set `providers.docker.network`. Both proxies now set it (`public`/`private`) and
`providers.docker.exposedbydefault=false`. `qbittorrent` and `traefik-ts` carry
`docksmith.restart-after` labels because they share another container's network namespace.
Those changes are committed in the compose repo and are unrelated to the work above.
