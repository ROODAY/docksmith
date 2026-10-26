# Notifications — Design

Status: draft for review
Date: 2026-10-26
Repo: fork `ROODAY/docksmith` (upstream `chrisae9/docksmith`)

## Intent

Docksmith already detects new container versions according to each container's labels, but the only way to learn about them is to open the UI. The goal is to get told — by email first, other services optionally — when something worth acting on happens, in the style of the "Connect"/notification settings in Sonarr, Radarr, Bazarr and Overseerr.

The user runs most containers on `:latest` today and is moving them to pinned semver tags via Docksmith. Once pinned, they want to hear about real version changes (`major.minor.patch`), not image rebuilds or digest churn, and they want to tune that per container where needed.

## Goals

- Notify when a new version is detected, using the version rules the container's labels already define (`version-pin-*`, `tag-regex`, `version-min/max`, `allow-prerelease`).
- Notify on other operational events, each of which the user can enable/disable per service.
- Support multiple notification services at once, each configured independently.
- Email (SMTP) is first-class; other services (ntfy, Discord, Telegram, Pushover, Gotify, Slack, generic webhook, …) available as options.
- Each service delivers either immediately or as a scheduled summary.
- Never notify about the same thing twice, including across restarts.
- Distinguish packaging rebuilds (`-ls201`, `-r4`) from real releases so they don't generate noise by default.

## Non-goals

- Notifications do not change what the UI shows. The UI shows what the checker sees; notification settings only decide what gets sent. (Exception: the new "build" change level below is a classification fix and the UI will display it.)
- No reminders/nagging for updates already notified about.
- No auto-updating, and no "apply update" action inside the notification itself (a link click must never change anything — Docksmith has no auth, and mail scanners pre-fetch links). Notifications instead carry buttons/links that open the Docksmith page where the user can review and apply the update (see Message content).
- No multi-host aggregation. Each Docksmith instance notifies for its own host.
- No per-event-type schedules.

## Anti-goals (failure even if it "works")

- An email every 5 minutes for the same pending update.
- Weekly emails for every LinuxServer image because of `-lsNNN` rebuilds.
- A failing notification service (bad SMTP password, ntfy down) blocking or slowing checks or updates.
- Secrets (SMTP password, tokens) leaking into logs or API responses.

## Constraints

- Go backend, React/TS UI, SQLite with embedded numbered migrations (`internal/storage/migrations/`, next free number is `000024`).
- Background check runs every `CHECK_INTERVAL` (default 5m) in `internal/update/background_checker.go`.
- No built-in auth; Docksmith is expected to run on a trusted network (user runs it behind Traefik on Tailscale).
- Runtime image is `alpine:3.23` with no `tzdata`.
- CI workflows publish to `ghcr.io/chrisae9/docksmith`; the fork needs to publish to `ghcr.io/rooday/docksmith`.

## Approaches considered

1. **Built-in, using shoutrrr** (`github.com/nicholas-fedor/shoutrrr`, the maintained fork; original `containrrr/shoutrrr` is abandoned). Pure Go, ~20 services via URL scheme. We build structured forms for common services and translate them to shoutrrr URLs; an "Advanced" type accepts a raw URL. Risk: single-maintainer fork — mitigated by pinning the version, and we can fork it ourselves if it stalls.
2. **Apprise** (what Bazarr uses). 100+ services but Python — either bloats the image or requires a separate Apprise API container.
3. **Hand-rolled SMTP + generic JSON webhook.** No dependency, but "options" become one option plus a webhook the user must adapt per service.

**Chosen: 1 (shoutrrr).** The user picked it; worst case we fork the library.

## Decisions

### Version change classification

- Add a **build** change level below patch for packaging-only changes: a change only in `Version.BuildNumber`, which today is populated only from LinuxServer `-lsNNN`. _(Builder-check call: `-rN` suffixes are currently stripped by `normalizeSuffix` and never compared, so `-r4` → `-r5` shows as no change. That behavior is kept; making `-rN` visible would change what the checker reports and is out of scope.)_
- The 4th numeric segment (`Version.Revision`, e.g. Plex `1.42.2.10156`) remains **patch** — it's a real upstream release for the images that use it.
- Ordering: build < patch < minor < major. Digest-only changes (no version change) are a separate, opt-in category, not a level.
- `internal/version.ChangeType` is serialized as an int to the UI (`change_type`). Add the new value without renumbering existing ones (append after `UnknownChange`) and update the UI's `ChangeType` enum and badges (show a "BUILD" badge).
- `version-pin-patch` semantics are unchanged (still allows build changes).

### What counts as "new version available"

A container's `latest_version` as computed by the existing checker (labels already applied) that differs from what was last notified for that container, and whose change level (current → latest) is at or above the effective notify level. Statuses `UPDATE_AVAILABLE` and `UPDATE_AVAILABLE_BLOCKED` qualify. `UP_TO_DATE_PINNABLE` does not (it's a migration suggestion, not a new version).

The user's end state is: no containers on `:latest`; everything pinned to real version tags; notified when a version they care about appears. `:latest` handling below is for the transition period.

Floating tags (`:latest`, `:16`, etc.), where the checker reports `LatestVersion` as the tag itself (`internal/update/checker.go` ~616-690):

- **Digest change with a resolved version** (`LatestResolvedVersion` set, and current version resolved): treat as a "new version" event. Compute the change level from resolved current → resolved latest, filter by the notify level, and dedup on the resolved version.
- **Digest change with no resolved version:** only the "digest changed" event (off by default; opt-in globally or via `docksmith.notify-digest=true`). Dedup on digest.
- **`UnknownChange`** (versions present but not comparable, e.g. composite tags like `14-vectorchord0.4.3-pgvectors0.2.0`): never raises a "new version" event; falls through to the digest rule above. _(Claude's call; user did not object.)_
- **`Downgrade`:** never notifies.

### Notify level: global setting + optional per-container label

- Global setting **minimum change level** for "new version" notifications: `major | minor | patch | build`, default **patch**.
- Global setting **notify on digest-only changes** (for `allow-latest` / non-versioned containers): off by default.
- Per-container label overrides (optional; most containers have none):
  - `docksmith.notify=false` — never send any notification about this container.
  - `docksmith.notify-level=major|minor|patch|build` — overrides the global minimum level for this container.
  - `docksmith.notify-digest=true|false` — overrides the global digest setting for this container.
- Labels were chosen over UI-stored per-container overrides because the user keeps compose files in git; per-container config lives with the container. Docksmith's existing label editor in the UI should support the new labels (same mechanism as the existing ones in `internal/api/handlers_labels.go`).
- Document the new labels in `docs/labels.md`, and also document the existing but undocumented `version-pin-patch`.

### Event types

| Event                              | Default timing | Notes                                                                                                                                                                  |
| ---------------------------------- | -------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| New version available              | summary        | see above                                                                                                                                                              |
| Digest changed (opt-in)            | summary        | only if enabled globally or by label                                                                                                                                   |
| Update succeeded                   | summary        | per container (from batch detail status), not per operation                                                                                                            |
| Update failed                      | immediate      | per container: a batch is only marked `failed` when every container fails (`update_orchestrator.go` ~1237), so a 3-ok/2-failed batch must still raise 2 failure events |
| Rollback performed                 | immediate      | auto-rollback or manual rollback completed; a failed update immediately followed by its auto-rollback is sent as one combined message                                  |
| Update blocked by pre-update check | immediate      | see dedup note below                                                                                                                                                   |
| Version check failing              | immediate      | a container's check has been `CHECK_FAILED`/`METADATA_UNAVAILABLE` for N consecutive checks (builder's call on N; ~3), notify once until it recovers                   |
| Compose mismatch                   | summary        | container entered `COMPOSE_MISMATCH`; notify once per entry into that state                                                                                            |

Each event type has a global **timing** setting: "immediate" or "summary". The defaults are as in the table; the user can change any of them.

Note: pre-update checks run during every background check (`internal/update/checker.go`, sets `UPDATE_AVAILABLE_BLOCKED`). The "blocked" event must fire on the transition into blocked for a given container+target version, not every check.

### Notification services

- A list of services, each with: name, type, enabled toggle, type-specific settings, per-event-type enable checkboxes, and a **delivery mode**: `immediate` or `summary`.
- Delivery semantics:
  - Service in `immediate` mode: sends every enabled event as it happens; ignores schedule and per-event timing.
  - Service in `summary` mode: events whose timing is "immediate" are sent right away; all other enabled events are batched and sent on the global schedule.
- Supported types with structured forms: **SMTP** (host, port, username, password, encryption: none/STARTTLS/TLS, from, to — multiple recipients allowed), **ntfy**, **Discord**, **Telegram**, **Pushover**, **Gotify**, **Slack**, **generic webhook**. Plus **Advanced** (raw shoutrrr URL). Builder may trim the structured list if a type is awkward to map; SMTP and Advanced are required.
- Each service has a **Send test** button.
- Secrets are stored in the Docksmith SQLite DB (`/data`), never logged, and never returned in API responses — the API returns a masked placeholder and treats an unchanged placeholder on save as "keep existing".
- Send failures: recorded per service (last success time, last error) and shown in the UI; logged. They never block the checker or the update orchestrator. Sending happens asynchronously. Builder's call on retry policy (a small bounded retry is fine; no infinite queue).

### Summary schedule

- One global **cron expression** (standard 5-field). No presets.
- Validated on save: must parse, and the minimum gap between consecutive fire times must be ≥ the current check interval. Invalid → reject with a clear error; nothing is saved.
- Evaluated in the container's timezone from the `TZ` env var. Embed tz data in the binary (`import _ "time/tzdata"`) so `TZ` works on the alpine image. The UI shows the active timezone next to the cron field, and the next few fire times as a preview.
- A summary contains the pending summary-timed events since that service's last summary. For "new version available" items, drop any whose container has since been updated to (or past) that version. If nothing remains, send nothing.

### De-duplication / state

- Persist notification state in SQLite (new migration) so restarts don't re-send:
  - Per container: last notified version (and last notified digest for digest notifications), plus state flags needed for transition events (blocked, check-failing, compose-mismatch).
  - Per service: pending summary items, last summary sent time, last error/success.
- A newer version superseding a notified one generates one new notification for the newer version.
- First run after enabling notifications: builder's call, but must not dump every currently pending update as "new" without intent — recommended behavior is to seed state silently from the current check results and only notify on changes from then on, with a one-time "Send current pending updates" button optional.

### Message content

- **Server name** global setting, default = host's hostname (Docker host name via the Docker API's info, falling back to container hostname). Included in every message subject/title.
- **Base URL** global setting (e.g. `https://docksmith.engager.ts.rooday.com`). When set, messages include action buttons (HTML buttons in email; links in chat services); when unset, no buttons/links.
  - Per item: **Open in Docksmith** → `/container/{name}` (the container page, which already has the update/pin/rollback actions).
  - Per message: **Review all updates** → `/` (the containers list, which shows pending updates) on new-version messages; **View history** → `/history` on failed/rollback messages.
  - Buttons are plain navigation links (GET). They never trigger an update by themselves.
- Grouping: new-version events found by one check are sent as a single message (immediate mode) listing all of them. Summaries list all pending items grouped by event type.
- Email: HTML body (table: container, current → new, change level, link) with plain-text alternative. Subject e.g. `[engager] 3 container updates available`.
- Chat-style services: compact plain text/markdown as the service supports.

### UI

- New **Notifications** section in Settings (`ui/src/components/Settings.tsx` or a sub-page if it gets large): global settings (server name, base URL, min change level, digest toggle, summary cron + timezone + preview, per-event timing), and the services list with add/edit/delete/test, per-service event checkboxes, delivery mode, last status.

### Build / packaging (fork)

- Update `.github/workflows/build.yml` and `release.yml` to publish to `ghcr.io/rooday/docksmith` (preferably derived from `github.repository_owner` rather than hardcoded).
- Docs: `README.md` mentions notifications and `TZ`; `docs/labels.md` gets the new labels.

## Left to the builder

- Package layout (suggested: `internal/notify`), how events are fed in (event bus subscription vs direct calls from the checker/orchestrator), cron library choice (e.g. `robfig/cron/v3` parser), table schemas, API route shapes, exact shoutrrr URL mapping per type, retry policy, the "check failing" threshold, and UI layout details.
- Tests: unit tests for classification (build vs patch incl. Plex 4-segment), notify-level filtering with labels, dedup across simulated restarts, cron min-interval validation, summary pruning of already-applied updates, and secret masking.
