# Status Payload and Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut `/api/status` from 573 KB to ~38 KB, cap client-triggered checks at one per 60 s server-wide, add gzip and a visible refresh indicator, and stop the UI from waiting on checks it did not start.

**Architecture:** Server-side: slim the cached discovery result on `/api/status` only, emit compact JSON with `Content-Length`, compress JSON/static text in middleware, and move trigger policy into `BackgroundChecker` (atomic claim + stale floor + force). Client-side: share in-flight status requests, act on `{triggered}`, add a "Checked Nm ago" control, and use section skeletons on the container page.

**Tech Stack:** Go 1.25 (`net/http`, `compress/gzip`, `encoding/json`, testify), React 18 + TypeScript + Vite, Tailwind-free hand-written CSS.

**Spec:** `docs/superpowers/specs/2026-10-29-status-payload-and-refresh-design.md`

## Global Constraints

- Work on a branch: `git switch -c fix/status-payload-and-refresh` before Task 1. Do not commit to `main`.
- `/api/status` is the hot path: keep every field except `stacks` and `available_tags`; `/api/check` keeps the full shape.
- The 60 s floor applies only to non-forced **client** triggers. Internal triggers (`TriggerCheck()`) must never be floored.
- Never gzip `text/event-stream` or `text/plain`; never leave `Content-Length` on a compressed response.
- CLI output keeps the existing indented encoder; only HTTP API responses become compact.
- No new dependencies.
- Every commit message ends with `Co-Authored-By: Claude Code <noreply@anthropic.com>`.
- Known Windows baseline failures (pre-existing): `internal/update.TestCheckPermissions_FailsWithoutAccess`, and package-wide `go test ./internal/api/...` cannot pass on Windows. Run the targeted `-run` filters below; rely on Linux/CI for the full API package.

## Review Focus

- **SSE and log streams must survive the gzip middleware.** A wrapped/late-flushing `/api/events` would show a permanent "Reconnecting" banner; log streaming must stay unbuffered. Task 4 tests pin this.
- **`Content-Length` must never disagree with the bytes written.** Setting it and then compressing (or vice versa) truncates responses in browsers. Task 1 and Task 4 tests pin this.
- **The stale floor must not swallow internal refreshes.** After a container update, `TriggerCheck()` must queue a run even if a check finished seconds ago; otherwise badges stay stale for up to 5 minutes. Task 2 tests pin this.
- **`{triggered:false}` must skip the 15 s poll.** If the UI polls anyway, every deduped minute still looks like a hang. Task 6 implements this; it has no unit-test runner, so verify in the browser Network panel.
- **Slimming must not drop a consumed field.** `labels`, `standalone_containers`, counts, and timestamps are read by `Containers.tsx`/`App.tsx`; only `stacks` and `available_tags` may disappear. Task 3 tests pin this.

---

### Task 0: Branch and commit the design docs

- [ ] **Step 1: Create the branch**

```bash
git switch -c fix/status-payload-and-refresh
```

- [ ] **Step 2: Commit the spec and plan**

```bash
git add docs/superpowers/specs/2026-10-29-status-payload-and-refresh-design.md docs/superpowers/plans/2026-10-29-status-payload-and-refresh.md
git commit -m "docs: add status payload and refresh design" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 1: Compact API JSON with Content-Length

**Files:**
- Modify: `internal/output/json.go`
- Test: `internal/output/json_test.go`
- Modify: `internal/api/respond.go`
- Test: `internal/api/respond_test.go`

**Interfaces:**
- Produces: `func MarshalJSONData(data any) ([]byte, error)`, `func MarshalJSONError(err error) ([]byte, error)`, `func MarshalJSONErrorWithData(err error, data any) ([]byte, error)` in `internal/output`.
- Produces: `func writeJSONBody(w http.ResponseWriter, status int, body []byte)` in `internal/api`.
- `RespondSuccess`, `RespondError`, `RespondErrorWithData`, `RespondNoContent` keep their signatures.

- [ ] **Step 1: Write failing output tests**

Append to `internal/output/json_test.go`:

```go
func TestMarshalJSONData_IsCompactAndRoundTrips(t *testing.T) {
	data, err := MarshalJSONData(map[string]string{"key": "value"})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "\n")
	assert.NotContains(t, string(data), "  ")

	var resp Response
	require.NoError(t, json.Unmarshal(data, &resp))
	assert.True(t, resp.Success)
	assert.Equal(t, map[string]any{"key": "value"}, resp.Data)
}
```

Add `"github.com/stretchr/testify/assert"` and `".../require"` imports if absent. In `internal/api/respond_test.go`, add `encoding/json`, `strconv`, and the testify imports as needed.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/output/ -run TestMarshalJSONData_IsCompactAndRoundTrips`
Expected: compile error, `undefined: MarshalJSONData`.

- [ ] **Step 3: Implement the compact marshalers**

In `internal/output/json.go`, add:

```go
func MarshalJSONData(data any) ([]byte, error)   { return json.Marshal(SuccessResponse(data)) }
func MarshalJSONError(err error) ([]byte, error) { return json.Marshal(ErrorResponse(err)) }
func MarshalJSONErrorWithData(err error, data any) ([]byte, error) {
	resp := ErrorResponse(err)
	resp.Data = data
	return json.Marshal(resp)
}
```

Leave `WriteJSON`/`WriteJSONData` (indented, used by the CLI) untouched.

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/output/`
Expected: PASS.

- [ ] **Step 5: Write the failing respond test**

Append to `internal/api/respond_test.go`:

```go
func TestRespondSuccess_SetsContentLengthAndCompactBody(t *testing.T) {
	w := httptest.NewRecorder()
	RespondSuccess(w, map[string]string{"ok": "yes"})

	assert.Equal(t, strconv.Itoa(w.Body.Len()), w.Header().Get("Content-Length"))
	assert.NotContains(t, w.Body.String(), "\n")

	var resp struct {
		Success bool              `json:"success"`
		Data    map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	assert.Equal(t, "yes", resp.Data["ok"])
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/api/ -run TestRespondSuccess_SetsContentLengthAndCompactBody`
Expected: FAIL — `Content-Length` is empty and the body contains newlines.

- [ ] **Step 7: Implement `writeJSONBody` and use it**

In `internal/api/respond.go`:

```go
func writeJSONBody(w http.ResponseWriter, status int, body []byte) {
	extendWriteDeadline(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
```

`RespondSuccess` marshals with `output.MarshalJSONData`; on error, call `RespondInternalError(w, fmt.Errorf("failed to encode JSON: %w", err))` and return. `RespondError` marshals with `output.MarshalJSONError`; on marshal error, fall back to statically writing `{"success":false,"error":"failed to encode JSON"}` with that status. `RespondErrorWithData` uses `MarshalJSONErrorWithData`. `RespondNoContent` keeps `WriteHeader(204)` with no body and no `Content-Length`.

- [ ] **Step 8: Run the package tests**

Run: `go test ./internal/api/ -run 'TestRespond|TestResponseWriteGrace|TestHandleHealth'`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/output/json.go internal/output/json_test.go internal/api/respond.go internal/api/respond_test.go
git commit -m "feat(api): emit compact JSON with Content-Length" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: BackgroundChecker trigger policy

**Files:**
- Modify: `internal/update/background_checker.go`
- Test: `internal/update/background_checker_test.go`

**Interfaces:**
- Produces: `const clientTriggerMinInterval = 60 * time.Second`
- Produces: `func (bc *BackgroundChecker) TriggerCheckClient(force bool) bool` — returns true only when this call started a check.
- Produces (unexported): `type triggerPolicy int` with `triggerInternal`, `triggerClient`, `triggerClientForce`; `func (bc *BackgroundChecker) claimCheck(p triggerPolicy) bool`.
- Keeps: `func (bc *BackgroundChecker) TriggerCheck()` (internal; queues a follow-up when a check is running).
- Refactor: `runCheck()` becomes `claimCheck(triggerInternal)` + `performCheck()`; the existing `checking`/`pendingRecheck` reset stays in `performCheck`'s defer.

- [ ] **Step 1: Write failing claim tests**

Append to `internal/update/background_checker_test.go`:

```go
func TestClaimCheck_InternalQueuesWhenRunning(t *testing.T) {
	bc := NewBackgroundChecker(nil, nil, nil, nil, time.Hour)
	bc.cache.mu.Lock()
	bc.cache.checking = true
	bc.cache.pendingRecheck = false
	bc.cache.mu.Unlock()

	assert.False(t, bc.claimCheck(triggerInternal))
	assert.True(t, bc.cache.pendingRecheck)
}

func TestClaimCheck_ClientDoesNotQueueWhenRunning(t *testing.T) {
	for _, policy := range []triggerPolicy{triggerClient, triggerClientForce} {
		bc := NewBackgroundChecker(nil, nil, nil, nil, time.Hour)
		bc.cache.mu.Lock()
		bc.cache.checking = true
		bc.cache.pendingRecheck = false
		bc.cache.mu.Unlock()

		assert.False(t, bc.claimCheck(policy))
		assert.False(t, bc.cache.pendingRecheck)
	}
}

func TestClaimCheck_ClientRespectsFloorButForceBypasses(t *testing.T) {
	bc := NewBackgroundChecker(nil, nil, nil, nil, time.Hour)
	bc.cache.mu.Lock()
	bc.cache.lastBackgroundRun = time.Now()
	bc.cache.mu.Unlock()

	assert.False(t, bc.claimCheck(triggerClient))
	assert.True(t, bc.claimCheck(triggerClientForce))
	assert.True(t, bc.cache.checking)
}

func TestClaimCheck_ClientAllowedAfterFloorAndOnFirstRun(t *testing.T) {
	fresh := NewBackgroundChecker(nil, nil, nil, nil, time.Hour)
	assert.True(t, fresh.claimCheck(triggerClient))

	old := NewBackgroundChecker(nil, nil, nil, nil, time.Hour)
	old.cache.mu.Lock()
	old.cache.lastBackgroundRun = time.Now().Add(-61 * time.Second)
	old.cache.mu.Unlock()
	assert.True(t, old.claimCheck(triggerClient))
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/update/ -run TestClaimCheck`
Expected: compile error, `undefined: claimCheck` / `triggerClient`.

- [ ] **Step 3: Implement the claim and refactor `runCheck`**

In `internal/update/background_checker.go`, add the policy type and constant, then:

```go
func (bc *BackgroundChecker) runCheck() {
	if !bc.claimCheck(triggerInternal) {
		return
	}
	bc.performCheck()
}

func (bc *BackgroundChecker) TriggerCheck() { go bc.runCheck() }

func (bc *BackgroundChecker) TriggerCheckClient(force bool) bool {
	policy := triggerClient
	if force {
		policy = triggerClientForce
	}
	if !bc.claimCheck(policy) {
		return false
	}
	go bc.performCheck()
	return true
}

func (bc *BackgroundChecker) claimCheck(p triggerPolicy) bool {
	bc.cache.mu.Lock()
	defer bc.cache.mu.Unlock()

	if bc.cache.checking {
		if p == triggerInternal {
			bc.cache.pendingRecheck = true
		}
		return false
	}
	if p == triggerClient && !bc.cache.lastBackgroundRun.IsZero() &&
		time.Since(bc.cache.lastBackgroundRun) < clientTriggerMinInterval {
		return false
	}
	bc.cache.checking = true
	bc.cache.pendingRecheck = false
	return true
}
```

Move the existing body of `runCheck` after the initial lock block into `performCheck()`; do not re-check `checking` there.

- [ ] **Step 4: Run the claim tests**

Run: `go test ./internal/update/ -run TestClaimCheck`
Expected: PASS.

- [ ] **Step 5: Write the end-to-end trigger test**

Append:

```go
func TestTriggerCheckClient_RunsOnceThenRespectsFloor(t *testing.T) {
	dockerMock := &mockDockerClient{}
	orch := NewOrchestrator(dockerMock, &mockRegistryClient{})
	bc := NewBackgroundChecker(orch, dockerMock, nil, nil, time.Hour)

	require.True(t, bc.TriggerCheckClient(false), "first client trigger should start a check")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, lastRun, checking := bc.GetCachedResults()
		if !checking && !lastRun.IsZero() {
			assert.False(t, bc.TriggerCheckClient(false), "second trigger inside 60s must be a no-op")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("check did not complete")
}
```

- [ ] **Step 6: Run it**

Run: `go test ./internal/update/ -run TestTriggerCheckClient_RunsOnceThenRespectsFloor -v`
Expected: PASS (the mock orchestrator returns zero containers quickly).

- [ ] **Step 7: Commit**

```bash
git add internal/update/background_checker.go internal/update/background_checker_test.go
git commit -m "feat(update): cap client-triggered checks at one per minute" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: Slim `/api/status`, keep `/api/check` full, report `{triggered}`

**Files:**
- Modify: `internal/update/orchestrator.go` (tag only)
- Modify: `internal/api/server.go` (field type + `checkRunner`)
- Modify: `internal/api/handlers_status.go`
- Modify: `internal/api/handlers.go`
- Create: `internal/api/handlers_status_test.go`
- Modify: `docs/api.md`

**Interfaces:**
- Produces: `type checkRunner interface { GetCachedResults() (*update.DiscoveryResult, time.Time, time.Time, bool); TriggerCheck(); TriggerCheckClient(force bool) bool; MarkCacheCleared(); Start(); Stop() }`
- Changes: `Server.backgroundChecker` type becomes `checkRunner` (nil checks still work).
- Produces: `func buildStatusResult(cached *update.DiscoveryResult, lastCheck, lastBackgroundRun time.Time, checking bool, checkInterval, cacheTTL time.Duration, slim bool) *update.DiscoveryResult`
- Produces: `func (s *Server) writeStatus(w http.ResponseWriter, slim bool)`
- `handleGetStatus` calls `writeStatus(w, true)`; `handleCheck`'s background path calls `writeStatus(w, false)`.
- `/api/trigger-check` response data: `{"triggered": bool, "checking": bool, "last_background_run": string}`.

- [ ] **Step 1: Write failing status tests**

Create `internal/api/handlers_status_test.go` (package `api`):

```go
type fakeCheckRunner struct {
	result     *update.DiscoveryResult
	lastCheck  time.Time
	lastRun    time.Time
	checking   bool
	triggered  bool
	forceCalls []bool
	cleared    int
}

func (f *fakeCheckRunner) GetCachedResults() (*update.DiscoveryResult, time.Time, time.Time, bool) {
	return f.result, f.lastCheck, f.lastRun, f.checking
}
func (f *fakeCheckRunner) TriggerCheck()                               {}
func (f *fakeCheckRunner) TriggerCheckClient(force bool) bool          { f.forceCalls = append(f.forceCalls, force); return f.triggered }
func (f *fakeCheckRunner) MarkCacheCleared()                           { f.cleared++ }
func (f *fakeCheckRunner) Start()                                      {}
func (f *fakeCheckRunner) Stop()                                       {}

func TestBuildStatusResult_SlimDropsOnlyTagsAndStacks(t *testing.T) {
	cached := &update.DiscoveryResult{
		Containers: []update.ContainerInfo{{
			ContainerUpdate: update.ContainerUpdate{
				ContainerName: "nginx", Status: update.UpToDate,
				AvailableTags: []string{"1.0", "2.0"},
			},
			ID:     "abc",
			Labels: map[string]string{"docksmith.ignore": "true"},
		}},
		Stacks:      map[string]*update.Stack{"app": {Name: "app"}},
		UpdateOrder: []string{"nginx"},
		TotalChecked: 1, UpToDate: 1,
	}
	lastCheck := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	lastRun := lastCheck.Add(time.Minute)

	got := buildStatusResult(cached, lastCheck, lastRun, true, time.Minute, time.Hour, true)

	assert.Nil(t, got.Stacks)
	require.Len(t, got.Containers, 1)
	assert.Nil(t, got.Containers[0].AvailableTags)
	assert.Equal(t, map[string]string{"docksmith.ignore": "true"}, got.Containers[0].Labels)
	assert.Equal(t, []string{"nginx"}, got.UpdateOrder)
	assert.Equal(t, 1, got.TotalChecked)
	assert.Equal(t, lastCheck.Format(time.RFC3339), got.LastCacheRefresh)
	assert.Equal(t, lastRun.Format(time.RFC3339), got.LastBackgroundRun)
	assert.True(t, got.Checking)
	assert.Equal(t, "1m0s", got.CheckInterval)
	assert.Equal(t, "1h0m0s", got.CacheTTL)

	body, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "available_tags")
	assert.NotContains(t, string(body), `"stacks"`)
}

func TestBuildStatusResult_FullKeepsTagsAndStacks(t *testing.T) {
	cached := &update.DiscoveryResult{
		Containers: []update.ContainerInfo{{ContainerUpdate: update.ContainerUpdate{
			ContainerName: "nginx", AvailableTags: []string{"1.0"},
		}}},
		Stacks: map[string]*update.Stack{"app": {Name: "app"}},
	}
	got := buildStatusResult(cached, time.Time{}, time.Time{}, false, time.Minute, time.Hour, false)
	assert.NotNil(t, got.Stacks)
	assert.Len(t, got.Containers[0].AvailableTags, 1)
}

func TestHandleTriggerCheck_ReportsTriggeredAndForce(t *testing.T) {
	for _, tc := range []struct{ query string; wantForce, wantTriggered bool }{
		{"", false, false},
		{"?force=true", true, true},
	} {
		fake := &fakeCheckRunner{triggered: tc.wantTriggered, checking: true}
		s := &Server{backgroundChecker: fake}
		w := httptest.NewRecorder()
		s.handleTriggerCheck(w, httptest.NewRequest("POST", "/api/trigger-check"+tc.query, nil))

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Triggered         bool   `json:"triggered"`
				Checking          bool   `json:"checking"`
				LastBackgroundRun string `json:"last_background_run"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.True(t, resp.Success)
		assert.Equal(t, tc.wantTriggered, resp.Data.Triggered)
		assert.Equal(t, tc.wantForce, fake.forceCalls[0])
	}
}

func TestHandleGetStatus_IsSlimAndHandleCheckIsFull(t *testing.T) {
	cached := &update.DiscoveryResult{
		Containers: []update.ContainerInfo{{ContainerUpdate: update.ContainerUpdate{
			ContainerName: "nginx", AvailableTags: []string{"1.0"},
		}}},
		Stacks: map[string]*update.Stack{"app": {Name: "app"}},
	}
	fake := &fakeCheckRunner{result: cached}
	s := &Server{backgroundChecker: fake, discoveryOrchestrator: update.NewOrchestrator(nil, nil)}

	w := httptest.NewRecorder()
	s.handleGetStatus(w, httptest.NewRequest("GET", "/api/status", nil))
	assert.NotContains(t, w.Body.String(), "available_tags")
	assert.NotContains(t, w.Body.String(), `"stacks"`)

	w = httptest.NewRecorder()
	s.handleCheck(w, httptest.NewRequest("GET", "/api/check", nil))
	assert.Contains(t, w.Body.String(), "available_tags")
	assert.Contains(t, w.Body.String(), `"stacks"`)
	assert.Equal(t, 1, fake.cleared)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api/ -run 'TestBuildStatusResult|TestHandleTriggerCheck_Reports|TestHandleGetStatus_IsSlim'`
Expected: compile errors for `buildStatusResult`, the `checkRunner` field assignment, and `handleTriggerCheck`'s old behavior.

- [ ] **Step 3: Implement the slim builder and handler wiring**

- `internal/update/orchestrator.go`: change `Stacks` tag to `json:"stacks,omitempty"`.
- `internal/api/server.go`: declare `checkRunner` and change the field to `backgroundChecker checkRunner`. `NewServer` and `Start`/`Shutdown` continue to work unchanged; the concrete `*update.BackgroundChecker` satisfies the interface.
- `internal/api/handlers_status.go`: move the current copy logic into `buildStatusResult` with the signature above. When `slim` is true, do not copy stacks (`Stacks: nil`) and set `AvailableTags = nil` on each copied entry in `Containers` and `StandaloneContainers`; when false, deep-copy stacks and tags as today. It sets `LastCacheRefresh`, `LastBackgroundRun`, `Checking`, `NextCheck`, `CheckInterval`, `CacheTTL` exactly as the current handler does.
- Add `writeStatus(w http.ResponseWriter, slim bool)`; `handleGetStatus` delegates with `true`.
- `internal/api/handlers.go`, `handleCheck` background path: call `s.writeStatus(w, false)` instead of `s.handleGetStatus`.
- `internal/api/handlers.go`, `handleTriggerCheck`: remove the 100 ms sleep; read `force := parseBoolParam(r, "force")`; call `triggered := s.backgroundChecker.TriggerCheckClient(force)`; then `_, _, lastRun, checking := s.backgroundChecker.GetCachedResults()`; respond with `triggered`, `checking`, and `last_background_run` (empty string when zero, otherwise RFC3339).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/api/ -run 'TestBuildStatusResult|TestHandleTriggerCheck|TestHandleGetStatus_IsSlim|TestHandleCheck|TestHandleHealth'`
Expected: PASS.

- [ ] **Step 5: Document the shape change**

In `docs/api.md`, under `GET /api/status`, state that the response is slim: `containers` and `standalone_containers` omit `available_tags`, and `stacks` is omitted; counts and timestamps are unchanged. Under the trigger-check row, document `?force=true` and the `{triggered, checking, last_background_run}` fields.

- [ ] **Step 6: Commit**

```bash
git add internal/update/orchestrator.go internal/api/server.go internal/api/handlers_status.go internal/api/handlers.go internal/api/handlers_status_test.go docs/api.md
git commit -m "feat(api): slim status payload and report trigger dedupe" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: gzip middleware

**Files:**
- Create: `internal/api/gzip.go`
- Test: `internal/api/gzip_test.go`
- Modify: `internal/api/server.go` (middleware chain)

**Interfaces:**
- Produces: `func gzipMiddleware(next http.Handler) http.Handler`
- Produces (unexported): `func acceptsGzip(header string) bool` — comma-split, matches `gzip`/`*`, honours `q=0`.
- `gzipResponseWriter` implements `WriteHeader`, `Write`, `Flush`, `Unwrap() http.ResponseWriter`, and `Close() error`.
- Constants: `gzipMinSize = 1024`; compressible media types: `application/json`, `text/html`, `text/css`, `text/javascript`, `application/javascript`, `image/svg+xml`.

- [ ] **Step 1: Write failing gzip tests**

Create `internal/api/gzip_test.go`:

```go
func TestGzipMiddleware_CompressesJSON(t *testing.T) {
	body := []byte(`{"payload":"` + strings.Repeat("x", 2000) + `"}`)
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	assert.Contains(t, rec.Header().Get("Vary"), "Accept-Encoding")
	assert.Empty(t, rec.Header().Get("Content-Length"))
	zr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	got, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestGzipMiddleware_SkipsEventStream(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: ping\ndata: 1\n\n"))
		w.(http.Flusher).Flush()
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.Equal(t, "event: ping\ndata: 1\n\n", rec.Body.String())
}

func TestGzipMiddleware_SkipsWhenNotAcceptedOrTooSmall(t *testing.T) {
	cases := []struct{ encoding, contentType string; size int }{
		{"", "application/json", 2000},
		{"gzip;q=0", "application/json", 2000},
		{"gzip", "application/json", 100},
	}
	for _, tc := range cases {
		body := []byte(strings.Repeat("x", tc.size))
		h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", tc.contentType)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
		}))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		if tc.encoding != "" {
			req.Header.Set("Accept-Encoding", tc.encoding)
		}
		h.ServeHTTP(rec, req)
		assert.Empty(t, rec.Header().Get("Content-Encoding"), tc.encoding)
	}
}

func TestGzipResponseWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &gzipResponseWriter{ResponseWriter: rec}
	assert.Same(t, rec, rw.Unwrap())
}
```

Use a `text/plain` type for the size/no-encoding cases so only the size/encoding rules are under test.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api/ -run TestGzip`
Expected: compile error, `undefined: gzipMiddleware`.

- [ ] **Step 3: Implement the middleware**

`gzipMiddleware` returns `next` unchanged when `!acceptsGzip(...)`. Otherwise it wraps the writer and defers `Close()`. In `WriteHeader`, decide once: compress only when `code == 200`, media type is compressible, no existing `Content-Encoding`, and (`Content-Length` absent or ≥ `gzipMinSize`); on compress, `Del("Content-Length")`, set `Content-Encoding: gzip`, create the writer, then call the underlying `WriteHeader`. Always add `Vary: Accept-Encoding`. `Flush` flushes the gzip writer then the underlying flusher. `Unwrap` returns the underlying writer.

Add `gzipMiddleware` to the chain in `NewServer` after `CorrelationIDMiddleware`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/api/ -run 'TestGzip|TestEventsHandlerReturnsOnShutdown'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/gzip.go internal/api/gzip_test.go internal/api/server.go
git commit -m "feat(api): gzip JSON and static text responses" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: UI client — force flag, response type, in-flight dedupe

**Files:**
- Modify: `ui/src/types/api.ts`
- Modify: `ui/src/api/client.ts`

**Interfaces:**
- Produces: `export interface TriggerCheckResponse { triggered: boolean; checking: boolean; last_background_run?: string }`
- Changes: `stacks?: Record<string, Stack>` (optional).
- Changes: `triggerCheck(force?: boolean): Promise<APIResponse<TriggerCheckResponse>>`
- Changes: `getContainerStatus` returns a shared in-flight promise per timeout key.

- [ ] **Step 1: Update the types**

Make `stacks` optional and add `TriggerCheckResponse`; export it from `client.ts` where needed.

- [ ] **Step 2: Update `client.ts`**

- `triggerCheck(force = false)` posts to `/trigger-check${force ? '?force=true' : ''}` and returns `APIResponse<TriggerCheckResponse>`.
- Add a module-level `statusInFlight` keyed by `timeoutMs === undefined ? 'default' : String(timeoutMs)`; return the existing promise when the key matches; clear it in `.finally` only if it is still the same promise.

- [ ] **Step 3: Build and lint**

Run: `cd ui && npm run build && npx eslint src/api/client.ts src/types/api.ts`
Expected: build succeeds; no new lint errors.

- [ ] **Step 4: Commit**

```bash
git add ui/src/types/api.ts ui/src/api/client.ts
git commit -m "feat(ui): dedupe status requests and expose trigger dedupe" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: Auto-refresh honours `{triggered}`; header indicator

**Files:**
- Modify: `ui/src/hooks/useContainersData.ts`
- Modify: `ui/src/components/Containers.tsx`
- Modify: `ui/src/styles/containers.css`

**Interfaces:**
- `runRefresh(force: boolean)`, `startRefresh(force: boolean)`, `backgroundRefresh(force = false)`; `refresh()` remains `backgroundRefresh(true)`.
- `useContainersData` result gains `lastBackgroundRun?: string`.
- `Containers.tsx` adds a refresh-status button: spinner + "Checking…" when `checking`, else "Checked {relative}" with a refresh icon; clicking calls `refresh`.

- [ ] **Step 1: Implement the refresh logic**

In `useContainersData.ts`:

- Thread `force` through `runRefresh` → `triggerCheck(force)`.
- `const triggered = triggerRes.success && triggerRes.data?.triggered === true`.
- Run the `/api/status` poll loop **only when `triggered`**; change the 300 ms interval to 1000 ms. When not triggered, skip the loop and proceed to the explorer/status refetch.
- Keep the existing in-flight coalescing; a forced call waits for the in-flight run then starts exactly one trailing run.
- Return `lastBackgroundRun: statusData?.last_background_run`.

- [ ] **Step 2: Implement the header indicator**

In `Containers.tsx`, destructure `checking` and `lastBackgroundRun`. Move the existing `reconnecting` chip markup into a new right-aligned `.header-status` group alongside the new button, so `.header-top` contains `<h1>` and `.header-status`:

```tsx
<div className="header-status">
  {reconnecting && (
    <div className="connection-status reconnecting" title="Reconnecting...">
      <i className="fa-solid fa-wifi fa-fade"></i>
      <span>Reconnecting...</span>
    </div>
  )}
  <button
    className="refresh-status"
    onClick={refresh}
    disabled={checking}
    title="Check for updates now"
    aria-label="Check for updates now"
  >
    <i className={`fa-solid fa-rotate ${checking ? 'fa-spin' : ''}`} />
    <span>{checking ? 'Checking…' : `Checked ${lastBackgroundRun ? formatTimeAgo(lastBackgroundRun) : 'never'}`}</span>
  </button>
</div>
```

Add a 30 s interval in an effect that bumps a tick state so the relative label stays current. Import `formatTimeAgo` from `../utils/time`.

- [ ] **Step 3: Add styles**

In `ui/src/styles/containers.css`:

```css
.containers-header .header-status { display: flex; align-items: center; gap: var(--space-4); }
.containers-header .refresh-status {
  display: inline-flex; align-items: center; gap: var(--space-2);
  background: none; border: none; padding: var(--space-2) var(--space-3);
  color: var(--color-text-secondary); font-size: var(--text-sm);
  border-radius: var(--radius-md); cursor: pointer;
}
.containers-header .refresh-status:hover:not(:disabled) { color: var(--color-text-primary); background: var(--color-bg-tertiary); }
.containers-header .refresh-status:disabled { cursor: default; opacity: 0.7; }
```

Check mobile widths in the existing media queries so the label can shrink or hide.

- [ ] **Step 4: Build and lint**

Run: `cd ui && npm run build && npx eslint src/hooks/useContainersData.ts src/components/Containers.tsx`
Expected: build succeeds; no new lint errors.

- [ ] **Step 5: Manual verification**

With the app running: the header shows "Checked Nm ago"; clicking it forces a check (spinner + "Checking…"). Let the 60 s automatic refresh fire while a check ran recently, and confirm in the Network panel that the deduped trigger produces **no** repeated `/api/status` polls (only the explorer/status refetch).

- [ ] **Step 6: Commit**

```bash
git add ui/src/hooks/useContainersData.ts ui/src/components/Containers.tsx ui/src/styles/containers.css
git commit -m "feat(ui): honor trigger dedupe and show last-checked control" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 7: Container page skeletons while loading

**Files:**
- Modify: `ui/src/pages/ContainerPage.tsx`
- Modify: `ui/src/styles/container-page.css`

**Interfaces:**
- Imports `SkeletonDetailSection` from `../components/Skeleton/Skeleton`.
- Loading branch keeps the Back button, title, and reconnecting/retry copy; replaces the bare spinner with a compact hint row plus three `SkeletonDetailSection` blocks.

- [ ] **Step 1: Implement the loading layout**

Replace the `<main className="main-loading">` contents in the `loading` branch with:

```tsx
<main className="main-loading">
  <div className="loading-hint-row">
    <i className="fa-solid fa-circle-notch fa-spin"></i>
    <p className="loading-hint">
      {reconnecting
        ? 'Reconnecting to Docksmith — the page will fill in when it responds.'
        : 'Waiting for a response from Docksmith…'}
    </p>
    <button className="retry-btn" onClick={retry}>Retry now</button>
  </div>
  <SkeletonDetailSection />
  <SkeletonDetailSection />
  <SkeletonDetailSection />
</main>
```

Add `.loading-hint-row` styles in `container-page.css` (flex, wrap, centered, gap), keeping the existing `.loading-hint` and `.retry-btn` rules.

- [ ] **Step 2: Build and lint**

Run: `cd ui && npm run build && npx eslint src/pages/ContainerPage.tsx`
Expected: build succeeds; no new lint errors.

- [ ] **Step 3: Commit**

```bash
git add ui/src/pages/ContainerPage.tsx ui/src/styles/container-page.css
git commit -m "feat(ui): show section skeletons while a container loads" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 8: Full verification

**Files:** none.

- [ ] **Step 1: Run the Go tests**

```bash
go test ./internal/output/
go test ./internal/update/ -run 'TestClaimCheck|TestTriggerCheckClient|TestBackgroundChecker'
go test ./internal/api/ -run 'TestBuildStatusResult|TestHandleTriggerCheck|TestHandleGetStatus|TestHandleCheck|TestGzip|TestRespond|TestResponseWriteGrace|TestHandleHealth|TestEvents'
```

Expected: PASS. (Package-wide `go test ./internal/update/` still hits the pre-existing `TestCheckPermissions_FailsWithoutAccess` Windows failure, and package-wide `./internal/api/...` has the pre-existing Windows failures; use Linux/CI for those.)

- [ ] **Step 2: Build and lint the UI**

```bash
cd ui
npm run build
npx eslint src/api/client.ts src/types/api.ts src/hooks/useContainersData.ts src/components/Containers.tsx src/pages/ContainerPage.tsx
```

Expected: build succeeds; no new lint errors.

- [ ] **Step 3: Verify the server behaviour with curl (local or deployed)**

```bash
# size and compression
curl -sS -o /dev/null -w '%{size_download} bytes %{time_total}s\n' http://localhost:8080/api/status
curl -sS -H 'Accept-Encoding: gzip' -D - -o /dev/null http://localhost:8080/api/status | grep -i 'content-encoding\|content-length'
# trigger dedupe: first true, second false
curl -sS -X POST 'http://localhost:8080/api/trigger-check' ; echo
curl -sS -X POST 'http://localhost:8080/api/trigger-check' ; echo
curl -sS -X POST 'http://localhost:8080/api/trigger-check?force=true' ; echo
```

Expected: status ≤ ~40 KB (≤ ~6 KB gzipped), `content-encoding: gzip`; the first call reports `"triggered":true`, the second `false`, the forced call `true`.

- [ ] **Step 4: Confirm the branch is clean**

```bash
git status --short
```

Expected: no unstaged changes (the spec/plan were committed in Task 0; each task committed its own files).
