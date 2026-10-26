# Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Docksmith sends notifications (SMTP first, other services via shoutrrr) for new versions and operational events, delivered immediately or as cron-scheduled summaries, never repeating itself.

**Architecture:** A new `internal/notify` package. A pure `Detect` function diffs each background check result against persisted per-container state to produce events. A subscriber on the existing event bus turns update/rollback progress into events. A `Dispatcher` routes events per service (send now, or store as pending), and a `Scheduler` flushes pending items on the global cron. Storage lives in new SQLite tables behind a narrow `storage.NotifyStore` interface (deliberately not added to `storage.Storage`, so the existing mocks don't change). The UI adds a Notifications section to Settings.

**Tech Stack:** Go 1.25, `github.com/nicholas-fedor/shoutrrr` (pin the latest release), `github.com/robfig/cron/v3` (parser only), SQLite (modernc), testify, React 19 + TS (Vite).

**Spec:** `docs/superpowers/specs/2026-10-26-notifications-design.md`

## Global Constraints

- Notifications never change what the checker or UI reports. The only classification change is the new `BuildChange`.
- `ChangeType` ints are serialized to the UI: append `BuildChange` after `UnknownChange` (value 6). Never renumber.
- Default min level is `patch`. Digest notifications default to off.
- Default event timing: `update_failed`, `rollback`, `update_blocked`, `check_failing` → immediate; `new_version`, `digest_changed`, `update_succeeded`, `compose_mismatch` → summary.
- Service delivery is `immediate` or `summary`.
- The cron is standard 5-field. Reject it unless it parses and the minimum gap between fires is ≥ the check interval.
- The schedule uses `time.Local` (from `TZ`). Embed timezone data via `import _ "time/tzdata"`.
- Labels: `docksmith.notify` (`false` mutes everything for that container), `docksmith.notify-level` (`major|minor|patch|build`), `docksmith.notify-digest` (`true|false`).
- Secrets are never logged or returned by the API. Mask them as `********`, and treat a masked value submitted on save as "keep the stored value".
- Sending is async with a 30s timeout and at most 2 retries (5s, then 15s). It never blocks the checker, the orchestrator or the event bus.
- `check_failing` fires after 3 consecutive failed checks, once, then resets when a check succeeds.
- On first run (state not yet seeded), record the current state silently and emit nothing.
- Message links only appear when `base_url` is set: `/container/{name}`, `/` (Review all updates), `/history` (View history). They are GET-only navigation.
- Auto-rollback is not wired in this codebase (`shouldAutoRollback` is never called). The `rollback` event fires for any completed rollback operation. Do not implement auto-rollback.
- Migrations: next number is `000024`, with both `.up.sql` and `.down.sql`.

## Review Focus

1. **Restart with pending state:** restarting Docksmith with notified versions and pending summary items must not re-send or lose anything. Tested in Task 3 and Task 6.
2. **Masked secret round-trip:** editing an SMTP service without retyping the password must keep the password. Tested in Task 5 and Task 10.
3. **Superseded versions:** 4.0.10 is found, then 4.0.11, before the summary is sent. The summary lists only 4.0.11, and one line. A container that has already been updated is pruned. Tested in Task 8.
4. **Slow or broken services:** an SMTP host that hangs must not delay the next check or the event bus subscriber, and a failure must show up in the service's `last_error`. Tested in Task 8.
5. **Removed or renamed containers:** a container that disappears drops its state and pending rows, and no event fires about it. Tested in Task 6 and Task 8.

---

### Task 1: `BuildChange` classification

**Files:**
- Modify: `internal/version/types.go` (enum and `String()`), `internal/version/comparator.go` (`GetChangeType`)
- Modify: `internal/update/orchestrator.go:~499` (stack priority switch: treat `BuildChange` like `PatchChange`)
- Modify: `ui/src/types/api.ts` (`ChangeType.BuildChange = 6`, `getChangeTypeName` → `"build"`), `ui/src/components/Containers.tsx:~747` (add a `BUILD` badge, class `status-badge build`, styled like patch but muted)
- Test: `internal/version/comparator_test.go`

**Interfaces:**
- Produces: `version.BuildChange ChangeType` (= 6), `String() == "build"`.

- [ ] **Step 1: Write failing tests** `TestGetChangeType_BuildOnly`:
  - `4.0.1-ls200` → `4.0.1-ls201` == `BuildChange`
  - `1.42.2.10156` → `1.42.2.10200` == `PatchChange`
  - `4.0.1-ls200` → `4.0.2-ls201` == `PatchChange`
  - `1.2.3-r4` → `1.2.3-r5` == `NoChange` (unchanged behaviour)
  - `4.0.1-ls201` → `4.0.1-ls200` == `Downgrade`
- [ ] **Step 2:** `go test ./internal/version/ -run TestGetChangeType_BuildOnly` → FAIL
- [ ] **Step 3:** In `GetChangeType`, after the `Downgrade` check, return `BuildChange` when Major/Minor/Patch/Revision/Prerelease are all equal (only `BuildNumber` differs).
- [ ] **Step 4:** `go test ./internal/...` → PASS. `cd ui && npm run build && npm run lint` → succeeds.
- [ ] **Step 5:** Commit `feat(version): classify build-number-only changes as BuildChange`

### Task 2: Notification labels

**Files:**
- Modify: `internal/scripts/manager.go` (constants `NotifyLabel = "docksmith.notify"`, `NotifyLevelLabel = "docksmith.notify-level"`, `NotifyDigestLabel = "docksmith.notify-digest"`)
- Modify: `internal/api/handlers_labels.go`:
  - `SetLabelsRequest` gets `Notify *bool`, `NotifyDigest *bool`, `NotifyLevel *string`
  - add them to `docksmithLabels`
  - add them to the bool-label list
  - apply string labels like `VersionMin`, validating `NotifyLevel` ∈ {major, minor, patch, build} and returning 400 otherwise
  - add a reverse-mapping `switch`
- Modify: `ui/src/operation/types.ts` (the two label request types: `notify?`, `notify_digest?`, `notify_level?`)
- Modify: `docs/labels.md` (quick-reference rows and sections for the 3 new labels, plus the undocumented `docksmith.version-pin-patch`)
- Test: `internal/api/handlers_labels_test.go` (or the existing labels test file)

- [ ] **Step 1: Failing test** `TestSetLabels_NotifyLevelValidation`: `notify_level: "huge"` → 400. `notify_level: "minor"` → the modified labels include `docksmith.notify-level=minor`. Follow the existing set-labels test setup.
- [ ] **Step 2:** Run → FAIL
- [ ] **Step 3:** Implement as listed.
- [ ] **Step 4:** `go test ./internal/api/...` → PASS
- [ ] **Step 5:** Commit `feat(labels): add notify, notify-level, notify-digest labels`

### Task 3: Notification storage

**Files:**
- Create: `internal/storage/migrations/000024_create_notifications.up.sql` / `.down.sql`
- Create: `internal/storage/notify.go` (interface and row types), `internal/storage/sqlite_notify.go`
- Test: `internal/storage/sqlite_notify_test.go`

**Tables:**
- **`notify_services`:**
  - `id INTEGER PK`, `name TEXT`, `type TEXT`, `enabled INTEGER`, `delivery TEXT`
  - `events TEXT` (JSON array), `config TEXT` (JSON object)
  - `last_success_at`, `last_error TEXT`, `last_error_at`, `last_summary_at`, `created_at`, `updated_at`
- **`notify_container_state`:**
  - `container_name TEXT PK`, `last_notified_version TEXT`, `last_notified_digest TEXT`
  - `blocked INTEGER`, `check_fail_count INTEGER`, `check_fail_notified INTEGER`, `compose_mismatch INTEGER`, `updated_at`
- **`notify_pending`:**
  - `id INTEGER PK`, `service_id INTEGER` (FK → `notify_services` ON DELETE CASCADE)
  - `dedup_key TEXT`, `kind TEXT`, `container_name TEXT`, `payload TEXT` (JSON), `created_at`
  - `UNIQUE(service_id, dedup_key)`

**Interfaces:**
- Produces (package `storage`):
```go
type NotifyServiceRow struct { ID int64; Name, Type, Delivery string; Enabled bool; Events []string; Config map[string]string; LastSuccessAt, LastErrorAt, LastSummaryAt *time.Time; LastError string }
type NotifyContainerState struct { ContainerName, LastNotifiedVersion, LastNotifiedDigest string; Blocked, CheckFailNotified, ComposeMismatch bool; CheckFailCount int }
type NotifyPendingRow struct { ID, ServiceID int64; DedupKey, Kind, ContainerName, Payload string; CreatedAt time.Time }
type NotifyStore interface {
    ListNotifyServices(ctx context.Context) ([]NotifyServiceRow, error)
    GetNotifyService(ctx context.Context, id int64) (NotifyServiceRow, bool, error)
    SaveNotifyService(ctx context.Context, row NotifyServiceRow) (int64, error) // insert when ID==0, else update
    DeleteNotifyService(ctx context.Context, id int64) error
    RecordNotifySendResult(ctx context.Context, id int64, sendErr error, at time.Time) error
    SetNotifySummarySent(ctx context.Context, id int64, at time.Time) error
    LoadNotifyContainerStates(ctx context.Context) (map[string]NotifyContainerState, error)
    ReplaceNotifyContainerStates(ctx context.Context, states map[string]NotifyContainerState) error // one transaction; rows not in map are deleted
    UpsertNotifyPending(ctx context.Context, row NotifyPendingRow) error // on (service_id, dedup_key) conflict: replace payload, kind and created_at
    ListNotifyPending(ctx context.Context, serviceID int64) ([]NotifyPendingRow, error)
    DeleteNotifyPending(ctx context.Context, ids []int64) error
    DeleteNotifyPendingForContainer(ctx context.Context, containerName string) error
}
var _ NotifyStore = (*SQLiteStorage)(nil)
```
- Settings JSON lives in the existing `config` table under the key `notify_settings`. The seeded flag uses the key `notify_seeded`.

- [ ] **Step 1: Failing tests:**
  - `TestNotifyServices_CRUD`: insert, get, update, list, delete. Deleting cascades the pending rows.
  - `TestNotifyPending_UpsertReplacesByDedupKey`: two upserts with the same key → 1 row holding the second payload.
  - `TestNotifyContainerStates_ReplaceDropsMissing`: states persist across a reopen of the same DB file, and a name missing from the replace map is deleted.
- [ ] **Step 2:** `go test ./internal/storage/ -run Notify` → FAIL
- [ ] **Step 3:** Implement. Enable `PRAGMA foreign_keys=ON` if it isn't already set. Use `retryWithBackoff` for writes, matching `sqlite_config.go`.
- [ ] **Step 4:** Run → PASS
- [ ] **Step 5:** Commit `feat(storage): notification tables and NotifyStore`

### Task 4: Core types, settings, levels, cron

**Files:**
- Create: `internal/notify/types.go`, `internal/notify/settings.go`, `internal/notify/level.go`, `internal/notify/schedule.go`
- Modify: `cmd/docksmith/main.go` (add `import _ "time/tzdata"`), `go.mod` (`robfig/cron/v3`)
- Test: `internal/notify/level_test.go`, `internal/notify/schedule_test.go`, `internal/notify/settings_test.go`

**Interfaces:**
- Produces:
```go
type EventKind string // new_version, digest_changed, update_succeeded, update_failed, rollback, update_blocked, check_failing, compose_mismatch
var AllEventKinds []EventKind
type Level int // LevelBuild=1, LevelPatch=2, LevelMinor=3, LevelMajor=4
func ParseLevel(s string) (Level, error); func (l Level) String() string
type Event struct { Kind EventKind; Container, Stack, FromVersion, ToVersion, Message, OperationID string; Level Level; At time.Time }
type Timing string // "immediate" | "summary"
type Settings struct {
    ServerName   string               `json:"server_name"`
    BaseURL      string               `json:"base_url"`
    MinLevel     string               `json:"min_level"`
    NotifyDigest bool                 `json:"notify_digest"`
    SummaryCron  string               `json:"summary_cron"` // default "0 8 * * *"
    Timing       map[EventKind]Timing `json:"timing"`
}
func DefaultSettings(hostname string) Settings
func LoadSettings(ctx context.Context, cfg ConfigStore, hostname string) (Settings, error) // a missing key gives the defaults; missing Timing entries are filled from the defaults
func SaveSettings(ctx context.Context, cfg ConfigStore, s Settings) error
type ConfigStore interface { GetConfig(ctx context.Context, key string) (string, bool, error); SetConfig(ctx context.Context, key, value string) error }
// Classify returns ok=false for unparseable, equal, downgrade or UnknownChange.
func Classify(current, latest string) (Level, bool) // uses version.NewParser().ParseTag + version.NewComparator().GetChangeType
func ParseSchedule(expr string) (cron.Schedule, error)
func ValidateSchedule(expr string, minGap time.Duration, loc *time.Location, now time.Time) error
func NextFires(expr string, loc *time.Location, now time.Time, n int) ([]time.Time, error)
func TimezoneInfo() (name string, warning string) // warning is set when TZ is set but time.Local resolved to UTC
```
- Min-gap algorithm: walk `Next()` from `now` in `loc` for up to 366 days or 10,000 fires. Fail as soon as the gap between consecutive fires is `< minGap`.

- [ ] **Step 1: Failing tests:**
  - `TestClassify`:
    - `("4.0.1-ls200","4.0.1-ls201") == (LevelBuild,true)`
    - `("16.1.0","17.0.0") == (LevelMajor,true)`
    - `("1.2.3","1.2.2")` → ok=false
    - `("latest","1.2.3")` → ok=false
    - `("14-vectorchord0.4.3-pgvectors0.2.0","14-vectorchord0.4.4-pgvectors0.2.0")` → ok=false *unless* the parser yields a real comparison; assert against what `GetChangeType` returns and document the result in a comment.
  - `TestValidateSchedule`, with a 5m gap:
    - `"*/5 * * * *"` ok
    - `"*/2 * * * *"` error
    - `"0-4 8 * * *"` error (1-minute gap)
    - `"0 8 * * 1"` ok
    - `"bogus"` error
    - `"0 8 31 2 *"` error ("never fires")
  - `TestLoadSettings_Defaults`: an empty store gives `MinLevel=="patch"`, `NotifyDigest==false`, and the default timings per Global Constraints.
- [ ] **Step 2:** `go test ./internal/notify/` → FAIL
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run → PASS. `go build ./...` → ok.
- [ ] **Step 5:** Commit `feat(notify): core types, settings, level classification, cron validation`

### Task 5: Services → shoutrrr

**Files:**
- Create: `internal/notify/services.go`, `internal/notify/sender.go`
- Modify: `go.mod` (`github.com/nicholas-fedor/shoutrrr`)
- Test: `internal/notify/services_test.go`

**Interfaces:**
- Consumes: `storage.NotifyServiceRow`
- Produces:
```go
const MaskedSecret = "********"
var ServiceTypes = []string{"smtp","ntfy","discord","telegram","pushover","gotify","slack","webhook","advanced"}
func SecretFields(serviceType string) []string // smtp: password; ntfy: token,password; discord: webhook_url; telegram: token; pushover: token,user_key; gotify: token; slack: webhook_url; webhook: url; advanced: url
func ValidateServiceConfig(serviceType string, cfg map[string]string) error // required fields per type
func MaskConfig(serviceType string, cfg map[string]string) map[string]string
func MergeSecrets(serviceType string, incoming, stored map[string]string) map[string]string // incoming MaskedSecret → stored value
func BuildURL(serviceType string, cfg map[string]string) (string, error)
type Message struct { Title, Text, HTML string }
type Sender interface { Send(ctx context.Context, serviceType string, cfg map[string]string, msg Message) error }
type ShoutrrrSender struct{} // implements Sender
```
- SMTP config keys: `host`, `port`, `username`, `password`, `encryption` (`none|starttls|tls`), `from`, `to` (comma-separated). Enable HTML. Use `go doc github.com/nicholas-fedor/shoutrrr/pkg/services/smtp` to confirm the parameter names. If the SMTP service can't send a plain-text alternative alongside HTML, send HTML only and note it in the commit message.
- Chat services get `msg.Text` and a title param where the service supports one. SMTP gets `msg.HTML` (with `msg.Text` as the alternative if supported).

- [ ] **Step 1: Failing tests:**
  - `TestBuildURL_AllTypesParse`: for each type, give a representative config. `BuildURL` succeeds and `shoutrrr.CreateSender(url)` returns no error.
  - `TestMergeSecrets_KeepsStoredWhenMasked`: incoming `{"password":"********","host":"new"}` merged with stored `{"password":"hunter2","host":"old"}` → `{"password":"hunter2","host":"new"}`.
  - `TestMaskConfig_HidesSecrets`.
  - `TestValidateServiceConfig_SMTPRequiresHostFromTo`.
- [ ] **Step 2:** Run → FAIL
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run → PASS
- [ ] **Step 5:** Commit `feat(notify): service config to shoutrrr URL mapping and secret masking`

### Task 6: Detect check-result events

**Files:**
- Create: `internal/notify/detect.go`
- Test: `internal/notify/detect_test.go`

**Interfaces:**
- Consumes: `Settings`, `Classify`, `storage.NotifyContainerState`, label constants from Task 2
- Produces:
```go
type Snapshot struct { Name, Stack, Status, CurrentVersion, LatestVersion, LatestResolvedVersion, CurrentDigest, LatestDigest string; Labels map[string]string }
func Detect(prev map[string]storage.NotifyContainerState, snaps []Snapshot, s Settings, now time.Time) (events []Event, next map[string]storage.NotifyContainerState)
func Muted(labels map[string]string) bool // docksmith.notify == "false"
```
- Rules, applied per snapshot:
  - **Muted:** state is still tracked, but no events are emitted.
  - **New version:** applies when status ∈ {`UPDATE_AVAILABLE`, `UPDATE_AVAILABLE_BLOCKED`}.
    - Target is `LatestResolvedVersion` if set, else `LatestVersion`.
    - `Classify(CurrentVersion, target)`.
    - If ok, the level ≥ the effective min (label `notify-level` else `s.MinLevel`), and target ≠ `LastNotifiedVersion`: emit `new_version` and set `LastNotifiedVersion = target`.
  - **Digest:** applies when classification fails, digest is enabled (label `notify-digest` else `s.NotifyDigest`), `LatestDigest != ""`, it differs from `CurrentDigest`, and it differs from `LastNotifiedDigest`. Emit `digest_changed` and record the digest.
  - **Blocked:** emit `update_blocked` when the status is BLOCKED and `!prev.Blocked`. Then `Blocked = (status == BLOCKED)`.
  - **Check failing:** a status ∈ {`CHECK_FAILED`, `METADATA_UNAVAILABLE`} increments the count. At count 3 with `!CheckFailNotified`, emit and set notified. Any other status resets both.
  - **Compose mismatch:** emit on the transition into `COMPOSE_MISMATCH`.
  - Containers absent from `snaps` are not in `next`.

- [ ] **Step 1: Failing tests** (table-driven, `TestDetect`). Each case asserts the emitted kinds and the resulting state:
  - patch available, min=patch → 1 `new_version`. Run again with `next` as `prev` → 0 events.
  - newer version supersedes (4.0.10 then 4.0.11) → second run emits once for 4.0.11.
  - `-ls` bump with min=patch → 0. With label `notify-level=build` → 1.
  - `:latest` container (`LatestVersion=="latest"`, `LatestResolvedVersion=="1.3.0"`, `CurrentVersion=="1.2.3"`) → `new_version` with Level minor.
  - `:latest` with no resolved version, digest changed: digest off → 0. Label `notify-digest=true` → 1 `digest_changed`. Same digest again → 0.
  - label `notify=false` → 0 events, but state is still updated.
  - BLOCKED three runs in a row → 1 event.
  - CHECK_FAILED ×2 → 0. ×3 → 1. ×4 → 0. Then UP_TO_DATE, then ×3 → 1.
  - container missing from snaps → dropped from `next`.
- [ ] **Step 2:** Run → FAIL
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run → PASS
- [ ] **Step 5:** Commit `feat(notify): detect notification events from check results`

### Task 7: Operation events from the event bus

**Files:**
- Create: `internal/notify/operations.go`
- Test: `internal/notify/operations_test.go`

**Interfaces:**
- Consumes: `events.Bus`, `events.EventUpdateProgress`. The payload keys are `operation_id`, `container_name`, `stack_name`, `stage`, `message` (see `UpdateOrchestrator.publishProgress`). Also `storage.Storage.GetUpdateOperation`.
- Produces:
```go
type OperationLookup interface { GetUpdateOperation(ctx context.Context, operationID string) (storage.UpdateOperation, bool, error) }
type OperationWatcher struct { /* seen map[key]time.Time pruned after 1h */ }
func NewOperationWatcher(bus *events.Bus, ops OperationLookup, emit func(Event)) *OperationWatcher
func (w *OperationWatcher) Start(ctx context.Context) // subscribe; the loop only filters and hands off. emit must be non-blocking.
func (w *OperationWatcher) handle(ctx context.Context, e events.Event) // exported to tests via the same package
```
- Mapping: only stages `complete` and `failed` matter. Dedup key: `operation_id|container_name|stage`.
  - Empty `container_name`:
    - `failed` with a non-empty `op.BatchDetails`: ignore. `failOperation` already publishes per container via `updateBatchDetailStatus`.
    - Otherwise, use `op.ContainerName`.
  - `op.OperationType == "rollback"`: `complete` → `rollback`; `failed` → `update_failed` with Message prefixed `"Rollback failed: "`.
  - Any other operation type: `complete` → `update_succeeded`; `failed` → `update_failed`.
  - Versions come from the matching `BatchDetails` entry (`OldVersion` → `NewVersion`), else from `op.OldVersion` → `op.NewVersion`.

- [ ] **Step 1: Failing tests** with a fake lookup:
  - single-update `complete` published twice (once by `updateBatchDetailStatus`, once by `publishProgress`) → 1 `update_succeeded`.
  - batch where 3 complete and 2 fail → 3 succeeded + 2 failed.
  - `failOperation`-style failed event with empty container on an op with BatchDetails → 0 extra events.
  - rollback op `complete` → 1 `rollback`.
- [ ] **Step 2:** Run → FAIL
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run → PASS
- [ ] **Step 5:** Commit `feat(notify): derive update/rollback events from the event bus`

### Task 8: Rendering, dispatcher, summary scheduler

**Files:**
- Create: `internal/notify/render.go`, `internal/notify/dispatcher.go`, `internal/notify/scheduler.go`
- Test: `internal/notify/render_test.go`, `internal/notify/dispatcher_test.go`

**Interfaces:**
- Consumes: `Sender`, `storage.NotifyStore`, `Settings`, `Event`, `NextFires`
- Produces:
```go
func Render(s Settings, events []Event, summary bool) Message
type CurrentVersions func(container string) (version string, exists bool)
type Dispatcher struct { /* store, sender, settings getter, work chan */ }
func NewDispatcher(store storage.NotifyStore, sender Sender, settings func() Settings) *Dispatcher
func (d *Dispatcher) Start(ctx context.Context)            // worker goroutine(s) doing the sends
func (d *Dispatcher) Dispatch(ctx context.Context, evs []Event) // non-blocking: route, upsert pending synchronously (fast DB), enqueue immediate sends
func (d *Dispatcher) FlushSummaries(ctx context.Context, current CurrentVersions, now time.Time)
func (d *Dispatcher) SendTest(ctx context.Context, serviceID int64) error // synchronous, used by the API
type Scheduler struct{}
func NewScheduler(d *Dispatcher, settings func() Settings, current CurrentVersions) *Scheduler
func (s *Scheduler) Start(ctx context.Context) // 1-minute ticker; fires FlushSummaries when now >= next fire; recomputes next whenever SummaryCron changes
```
- **Routing, per enabled service whose `Events` contain the kind:**
  - Send now if `Delivery == "immediate"` or `Timing[kind] == "immediate"`. Each `Dispatch` call produces one message per service, grouping all its events for that service.
  - Otherwise, upsert pending:
    - dedup key `new_version|<container>` (newer versions replace older ones)
    - `digest_changed|<container>`
    - `<kind>|<container>|<operation_id or At unix>` for everything else
    - payload = JSON `Event`
- **Summary flush, per summary service:**
  - Drop `new_version` items whose container no longer exists, or whose current version is ≥ `ToVersion` (use `Classify(current, To)` not ok ⇒ drop).
  - If nothing remains, send nothing.
  - Otherwise send the summary. On success, delete those pending rows and call `SetNotifySummarySent`.
- **Every send:** 30s timeout and 2 retries (5s, 15s). Afterwards, `RecordNotifySendResult`.
- **Render:**
  - Title prefix `[<ServerName>] `.
  - All `new_version` events → `"<n> container update(s) available"`.
  - A single other event → e.g. `"Update failed: sonarr"`.
  - Summary → `"Docksmith summary: <n> item(s)"`.
  - HTML is a table (container, from → to, level) grouped by kind, with buttons per the Global Constraints when `BaseURL` is set. Text is the same content as lines with plain URLs.

- [ ] **Step 1: Failing tests** with a fake Sender (records calls; can be made to block or error):
  - `TestDispatch_ImmediateVsSummary`: an immediate service gets a send for `new_version`; a summary service gets a pending row. `update_failed` reaches the summary service immediately.
  - `TestDispatch_EventFilterAndDisabled`: a disabled service, or one missing the kind, gets nothing.
  - `TestFlush_PrunesAppliedAndSuperseded`: pending 4.0.10 replaced by 4.0.11 → summary has 1 sonarr line. A container whose current version is already 4.0.11 → nothing sent.
  - `TestFlush_EmptySendsNothing`.
  - `TestDispatch_DoesNotBlockOnHungSender`: the Sender blocks forever, and `Dispatch` returns in < 100ms.
  - `TestSend_FailureRecorded`: the Sender errors and `last_error` is set (shorten the retry delays via an unexported var in the test).
  - `TestRender_LinksOnlyWithBaseURL`: with BaseURL the HTML contains `/container/sonarr` and `/`; without it, no `href`.
- [ ] **Step 2:** Run → FAIL
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/notify/` → PASS
- [ ] **Step 5:** Commit `feat(notify): dispatcher, rendering, and summary scheduler`

### Task 9: Wire the notifier into the server

**Files:**
- Create: `internal/notify/notifier.go`
- Modify: `internal/update/background_checker.go`:
  - add `SetResultObserver(func(ctx context.Context, r *DiscoveryResult))`
  - call it after the cache update in the run function (~line 262), in its own goroutine, with recover
- Modify: `internal/api/server.go` (construct the notifier when `cfg.StorageService` implements `storage.NotifyStore`; start it; keep it on `Server` as `notifier *notify.Notifier`)
- Test: `internal/notify/notifier_test.go`

**Interfaces:**
- Produces:
```go
type Notifier struct{}
func New(store storage.NotifyStore, cfg ConfigStore, ops OperationLookup, bus *events.Bus, sender Sender, hostname string, checkInterval time.Duration) *Notifier
func (n *Notifier) Start(ctx context.Context)
func (n *Notifier) OnCheckResult(ctx context.Context, r *update.DiscoveryResult) // builds []Snapshot from r.Containers; runs Detect; ReplaceNotifyContainerStates; DeleteNotifyPendingForContainer for dropped containers; seeds silently if notify_seeded is unset; Dispatch
func (n *Notifier) Settings() Settings
func (n *Notifier) UpdateSettings(ctx context.Context, s Settings) error // validates (cron vs checkInterval, min level, base URL parses) then saves
func (n *Notifier) CheckInterval() time.Duration
func (n *Notifier) Dispatcher() *Dispatcher
```
- `Muted` events from operations are filtered using the container's labels from the last check result.
- Hostname: `dockerService.GetClient().Info(ctx)` `.Name`, falling back to `os.Hostname()`.

- [ ] **Step 1: Failing test** `TestNotifier_SeedsSilentlyThenNotifies`, using real SQLite in a temp dir and a fake Sender:
  - First `OnCheckResult` with a pending update → 0 sends, and `notify_seeded` is set.
  - Second result with a newer version → 1 send.
  - A new Notifier on the same DB with the same result → 0 sends.
- [ ] **Step 2:** Run → FAIL
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/... ` → PASS. `go build ./...` → ok.
- [ ] **Step 5:** Commit `feat(notify): wire notifier into background checker and server`

### Task 10: API endpoints

**Files:**
- Create: `internal/api/handlers_notify.go`
- Modify: `internal/api/server.go` (`registerRoutes`)
- Modify: `docs/api.md` (new Notifications section)
- Test: `internal/api/handlers_notify_test.go`

**Routes** (all respond with the existing `RespondSuccess` / `RespondBadRequest` helpers; 503 when the notifier is nil):
- `GET /api/notifications/settings` → `{settings, timezone, timezone_warning, check_interval, next_summaries: [3 RFC3339]}`
- `PUT /api/notifications/settings` → 400 with the validation message when `UpdateSettings` fails; nothing is saved.
- `POST /api/notifications/validate-cron` with `{cron}` → `{valid, error, next: [3]}`
- `GET /api/notifications/services` → rows with `MaskConfig` applied
- `POST /api/notifications/services`, `PUT /api/notifications/services/{id}`:
  - apply `ValidateServiceConfig`
  - PUT uses `MergeSecrets` with the stored row
  - `delivery` ∈ {immediate, summary}
  - events must be a subset of `AllEventKinds`
- `DELETE /api/notifications/services/{id}`
- `POST /api/notifications/services/{id}/test` → `Dispatcher().SendTest`; the error text is returned in a 400

- [ ] **Step 1: Failing tests:**
  - `TestNotifySettings_RejectsTooFrequentCron`: PUT `*/1 * * * *` with a 5m interval → 400, and a GET afterwards still shows the old cron.
  - `TestNotifyServices_MaskedRoundTrip`:
    1. POST smtp with password `hunter2`.
    2. GET → `********`.
    3. PUT with `********` and a changed host → the stored password is still `hunter2` (check via the store).
    4. No response body contains `hunter2`.
  - `TestNotifyServices_RejectsUnknownEvent`.
- [ ] **Step 2:** Run → FAIL
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/api/...` → PASS
- [ ] **Step 5:** Commit `feat(api): notification settings and service endpoints`

### Task 11: Settings UI

**Files:**
- Create: `ui/src/components/NotificationSettings.tsx` (+ `.css` if the existing Settings styles aren't enough)
- Modify: `ui/src/api/client.ts` (typed functions for each Task 10 route), `ui/src/types/api.ts` (types `NotifySettings`, `NotifyService`, `EventKind`), `ui/src/components/Settings.tsx` (render the new `<section className="settings-section">` for Notifications)
- Modify: `ui/src/components/ContainerConfigCard.tsx`: per-container label controls next to the existing pin/tag controls, using the same set-labels call. They are: a "Notifications" mute toggle (`notify`), a level select (`notify_level`, with an empty option meaning "use global"), and a digest toggle (`notify_digest`).

**Behaviour:**
- **Global settings:**
  - fields: server name, base URL, min level select (major/minor/patch/build), digest toggle
  - summary cron text field: shows the timezone, plus `timezone_warning` if present. Debounced `validate-cron` shows the next 3 fires or the error, and Save is disabled while invalid.
  - per-event timing (immediate/summary) for all 8 kinds, with human labels
- **Services list:**
  - each row shows name, type, enabled toggle, delivery, and last success/error
  - Add/Edit form: a type select drives the fields (SMTP: host, port, username, password, encryption, from, to; other types: their keys from `SecretFields`/`ValidateServiceConfig`; advanced: URL)
  - password-type inputs for secrets, prefilled with `********`
  - event checkboxes, defaulting to all except `digest_changed`
  - Send test button (shows the error inline), Delete with confirm
- **Style:** follow the existing Settings section markup and classes. No new dependencies.

- [ ] **Step 1:** Implement.
- [ ] **Step 2:** `cd ui && npm run build && npm run lint` → succeeds.
- [ ] **Step 3: Manual check:**
  1. `go run ./cmd/docksmith api --port 8080` (with Docker available) or the dev setup in the README.
  2. Add an SMTP service, enter an invalid cron and see Save disabled.
  3. Save a valid cron and see the next fires listed.
  4. Click Send test and see either success or a readable error.
- [ ] **Step 4:** Commit `feat(ui): notification settings`

### Task 12: Fork image publishing and docs

**Files:**
- Modify: `.github/workflows/build.yml`, `.github/workflows/release.yml`: replace `ghcr.io/chrisae9/docksmith` with an image name derived in a step: `echo "image=ghcr.io/${GITHUB_REPOSITORY_OWNER,,}/docksmith" >> $GITHUB_OUTPUT`. GHCR needs lowercase, and the owner is `ROODAY`.
- Modify: `README.md`: a Notifications bullet under "What It Does"; `TZ` in the configuration table; the image name `ghcr.io/rooday/docksmith`.
- Modify: `docs/integrations.md`, `docs/api.md`: fix stale port references 3000 → 8080 where they describe the container.

- [ ] **Step 1:** Edit the files.
- [ ] **Step 2:** `grep -rn "chrisae9/docksmith" .github README.md` → there are no hard-coded publish targets left (upstream links in prose are fine).
- [ ] **Step 3:** Commit `chore: publish fork image to ghcr.io/rooday and document notifications`
