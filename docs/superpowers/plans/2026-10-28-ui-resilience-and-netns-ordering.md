# UI Resilience and Netns Ordering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the Docksmith UI usable while it is briefly unreachable, warn about and defer ingress containers in batches, stop `network_mode` provider races from failing recreates, cache `/api/explorer`, serialize compose label writes, and make "migrate off `:latest`" legible.

**Architecture:** Server-side correctness first (`internal/compose` retry/wait, `internal/docker` ingress classification, `internal/api` cache + keyed mutex), then the client renders last-known data from a module-level snapshot instead of paint-gating on one fetch. Ingress is defined once on the server and carried to the UI on `ContainerExplorerItem`.

**Tech Stack:** Go 1.x (stdlib, `testify`, `golang.org/x/sync`), React 19 + TypeScript + Vite.

**Spec:** `docs/handoffs/2026-10-28-ui-resilience-and-netns-ordering.md`

## Global Constraints

- One branch: `feat/ui-resilience-netns-ordering`. One commit per task, message prefixes as written below.
- Do not redo request timeouts or refresh coalescing from commits `6beff9a`/`8d06a7d`. Build on them.
- Do not fold the operation pollers/executors (`ui/src/operation/*`) into `fetchAPI`.
- `ExplorerData` JSON keeps every existing field with the same name and type. New fields are additive only.
- Changed UI files add no new `eslint` errors. Repo-wide lint is already dirty (42 errors, 6 warnings) — the gate is per-changed-file.
- Known local baseline failures (not yours): `internal/update.TestCheckPermissions_FailsWithoutAccess`, and in `internal/api`: `TestHandleOperations_WithData`, `TestHandleOperations_StorageError`, `TestIntegration_Operations`, `TestIntegration_StorageErrors`. Never run `go test ./internal/api/` unfiltered on Windows.
- Any test that overrides the package-level vars in `internal/compose/recreate.go` must not call `t.Parallel()` and must restore them with `t.Cleanup`.

## Review Focus

1. A cold first load with no network reaches an explicit error state inside the 15s request budget — never an endless skeleton.
2. A cached snapshot never permanently masks a dead backend: a failed refresh with no fresh data surfaces the offline/error state, and reconnecting refreshes it.
3. A container merely routed by Traefik (`traefik.enable=true`) is not classified as ingress — otherwise every bulk action warns and reorders.
4. The `network_mode` provider wait adds no delay to containers whose provider is absent or already running.
5. The explorer cache does not cache errors and never serves stale data beyond its TTL.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/compose/recreate.go` (modify) | Provider readiness wait + bounded retry around `compose up` |
| `internal/compose/lock.go` (create) | `KeyedMutex` serializing compose read-modify-write |
| `internal/compose/recreate_test.go` (modify) | Retry classifier, retry loop, provider wait tests |
| `internal/compose/lock_test.go` (create) | Keyed mutex + concurrent label write tests |
| `internal/docker/ingress.go` (create) | Pure ingress classification used by API and orchestrator |
| `internal/docker/ingress_test.go` (create) | Classification and transitive-dependency tests |
| `internal/docker/service.go` (modify) | `IsIngress`/`IsSelf` on `ContainerExplorerItem`; sizes flag |
| `internal/update/update_orchestrator.go` (modify) | Ingress-last ordering in batch update |
| `internal/update/ingress_order_test.go` (create) | Ordering test |
| `internal/api/explorer_cache.go` (create) | TTL + single-flight cache |
| `internal/api/explorer_cache_test.go` (create) | Single-flight, TTL, error non-caching tests |
| `internal/api/handlers_explorer.go` (modify) | Use cache; `include_volume_sizes` query |
| `internal/api/handlers_labels.go` (modify) | Hold `KeyedMutex` across load→modify→save |
| `internal/api/server.go` (modify) | Fields for cache and keyed mutex |
| `ui/src/api/snapshot.ts` (create) | Last-good explorer/status/inspect snapshots |
| `ui/src/hooks/useContainersData.ts` (modify) | Seed from snapshot; independent loading; sizes option |
| `ui/src/components/Containers.tsx` (modify) | Stop blanking on refresh; ingress warning; pinnable UX |
| `ui/src/pages/ContainerPage.tsx` (modify) | Cached inspect, in-place retry, waiting state |
| `ui/src/App.tsx` (modify) | Global reconnecting banner |
| `ui/src/pages/OperationProgressPage.tsx` (modify) | Offline note naming ingress |
| `ui/src/api/client.ts` (modify) | `getExplorerData` sizes option |
| `ui/src/types/api.ts` (modify) | `is_ingress`/`is_self` on explorer items |
| `ui/src/styles/base.css` (modify) | Banner/warning classes beside `.active-operation-banner` |

---

### Task 1: Wait for and retry around restarting `network_mode` providers

**Files:**
- Modify: `internal/compose/recreate.go`
- Test: `internal/compose/recreate_test.go`

**Interfaces:**
- Consumes: `docker.Client` (`ListContainers(ctx)`), existing `mockDockerClient` in `recreate_test.go`.
- Produces: package vars `dockerCommand func(ctx context.Context, args ...string) ([]byte, error)`, `networkModeWaitTimeout time.Duration`, `networkModePollInterval time.Duration`, `composeRetryBackoff []time.Duration`; helpers `isNetworkNamespaceRace(output []byte) bool`, `(*Recreator).runComposeWithRetry(ctx context.Context, args []string) ([]byte, error)`, `(*Recreator).waitForNetworkModeProvider(ctx context.Context, c *docker.Container) error`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/compose/recreate_test.go`:

```go
func TestIsNetworkNamespaceRace(t *testing.T) {
	incident := `Error response from daemon: cannot join network namespace of container: Container 42fb503e6b6f is restarting, wait until the container is running`
	assert.True(t, isNetworkNamespaceRace([]byte(incident)))
	assert.False(t, isNetworkNamespaceRace([]byte("Error response from daemon: pull access denied")))
}

func TestRecreateWithCompose_RetriesNetworkNamespaceRace(t *testing.T) {
	var upCalls int
	orig := dockerCommand
	t.Cleanup(func() { dockerCommand = orig })
	dockerCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "compose" {
			upCalls++
			if upCalls == 1 {
				return []byte("cannot join network namespace of container: Container x is restarting, wait until the container is running"), errors.New("exit status 1")
			}
			return []byte("ok"), nil
		}
		return []byte(""), nil
	}

	r := NewRecreator(&mockDockerClient{})
	c := &docker.Container{Name: "qbittorrent", Labels: map[string]string{
		"com.docker.compose.service": "qbittorrent",
	}}
	require.NoError(t, r.RecreateWithCompose(context.Background(), c, "/host/docker-compose.yaml", "/container/docker-compose.yaml"))
	assert.Equal(t, 2, upCalls)
}

func TestRecreateWithCompose_SkipsWaitWithoutNetworkMode(t *testing.T) {
	calls := 0
	origCmd := dockerCommand
	t.Cleanup(func() { dockerCommand = origCmd })
	dockerCommand = func(ctx context.Context, args ...string) ([]byte, error) { return nil, nil }

	mock := &countingDockerClient{listCalls: &calls}
	r := NewRecreator(mock)
	c := &docker.Container{Name: "app", Labels: map[string]string{"com.docker.compose.service": "app"}}
	require.NoError(t, r.RecreateWithCompose(context.Background(), c, "/host/docker-compose.yaml", "/container/docker-compose.yaml"))
	assert.Zero(t, calls, "must not list containers when the target has no network_mode label")
}

func TestRecreateWithCompose_WaitsForProvider(t *testing.T) {
	origCmd, origPoll := dockerCommand, networkModePollInterval
	t.Cleanup(func() { dockerCommand = origCmd; networkModePollInterval = origPoll })
	dockerCommand = func(ctx context.Context, args ...string) ([]byte, error) { return nil, nil }
	networkModePollInterval = time.Millisecond

	mock := &sequencedDockerClient{states: []string{"restarting", "restarting", "running"}}
	r := NewRecreator(mock)
	c := &docker.Container{Name: "qbittorrent", Labels: map[string]string{
		"com.docker.compose.service":  "qbittorrent",
		"com.docker.compose.project":  "engager",
		"com.docker.compose.network_mode": "service:gluetun",
	}}
	require.NoError(t, r.RecreateWithCompose(context.Background(), c, "/host/docker-compose.yaml", "/container/docker-compose.yaml"))
	assert.GreaterOrEqual(t, mock.calls, 3)
}

func TestRecreateWithCompose_FailsWhenProviderNeverRunning(t *testing.T) {
	origCmd, origPoll, origTimeout := dockerCommand, networkModePollInterval, networkModeWaitTimeout
	t.Cleanup(func() { dockerCommand = origCmd; networkModePollInterval = origPoll; networkModeWaitTimeout = origTimeout })
	dockerCommand = func(ctx context.Context, args ...string) ([]byte, error) { return nil, nil }
	networkModePollInterval = time.Millisecond
	networkModeWaitTimeout = 5 * time.Millisecond

	mock := &sequencedDockerClient{states: []string{"restarting", "restarting", "restarting"}}
	r := NewRecreator(mock)
	c := &docker.Container{Name: "qbittorrent", Labels: map[string]string{
		"com.docker.compose.service":      "qbittorrent",
		"com.docker.compose.project":      "engager",
		"com.docker.compose.network_mode": "service:gluetun",
	}}
	err := r.RecreateWithCompose(context.Background(), c, "/host/docker-compose.yaml", "/container/docker-compose.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gluetun")
}
```

Add these fakes at the bottom of the test file:

```go
type countingDockerClient struct{ listCalls *int }

func (m *countingDockerClient) ListContainers(ctx context.Context) ([]docker.Container, error) {
	*m.listCalls++
	return nil, nil
}
func (m *countingDockerClient) IsLocalImage(ctx context.Context, imageName string) (bool, error) { return false, nil }
func (m *countingDockerClient) GetImageVersion(ctx context.Context, imageName string) (string, error) { return "", nil }
func (m *countingDockerClient) GetImageDigest(ctx context.Context, imageName string) (string, error) { return "", nil }
func (m *countingDockerClient) Close() error { return nil }

// sequencedDockerClient returns a gluetun provider whose state walks through
// states on each ListContainers call (last value sticks).
type sequencedDockerClient struct {
	states []string
	calls  int
}

func (m *sequencedDockerClient) ListContainers(ctx context.Context) ([]docker.Container, error) {
	state := m.states[len(m.states)-1]
	if m.calls < len(m.states) {
		state = m.states[m.calls]
	}
	m.calls++
	return []docker.Container{{
		Name:   "gluetun",
		State:  state,
		Labels: map[string]string{"com.docker.compose.service": "gluetun", "com.docker.compose.project": "engager"},
	}}, nil
}
func (m *sequencedDockerClient) IsLocalImage(ctx context.Context, imageName string) (bool, error) { return false, nil }
func (m *sequencedDockerClient) GetImageVersion(ctx context.Context, imageName string) (string, error) { return "", nil }
func (m *sequencedDockerClient) GetImageDigest(ctx context.Context, imageName string) (string, error) { return "", nil }
func (m *sequencedDockerClient) Close() error { return nil }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/compose/ -run 'TestIsNetworkNamespaceRace|TestRecreateWithCompose_Retries|TestRecreateWithCompose_SkipsWait|TestRecreateWithCompose_Waits|TestRecreateWithCompose_Fails' -v`
Expected: FAIL — `undefined: isNetworkNamespaceRace`, `undefined: dockerCommand`, `undefined: networkModeWaitTimeout`, `undefined: networkModePollInterval`.

- [ ] **Step 3: Implement the wait and retry**

In `internal/compose/recreate.go`, add near the top (after imports):

```go
// Overridable in tests — they must not spawn docker.
var (
	dockerCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}
	networkModeWaitTimeout = 60 * time.Second
	networkModePollInterval = 1 * time.Second
	composeRetryBackoff    = []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}
)

// isNetworkNamespaceRace reports whether a failed `docker compose up` failed
// only because its network_mode provider was mid-restart.
func isNetworkNamespaceRace(output []byte) bool {
	s := string(output)
	return strings.Contains(s, "is restarting, wait until the container is running") ||
		strings.Contains(s, "cannot join network namespace")
}
```

`(*Recreator).runComposeWithRetry` retries `dockerCommand` over `composeRetryBackoff`, sleeping `delay` (respecting `ctx.Done()`), returning `(output, nil)` on success, `(output, err)` immediately when `!isNetworkNamespaceRace(output)`, and `(output, err)` after the final attempt.

`(*Recreator).waitForNetworkModeProvider` reads `container.Labels["com.docker.compose.network_mode"]`. Empty → `nil` without listing. Otherwise split on the first `:` into `prefix` (`service` or `container`) and `target`; poll at `networkModePollInterval` until a container in `ListContainers` matches (`container:` → `Name == target`; `service:` → `Labels["com.docker.compose.service"] == target && Labels["com.docker.compose.project"] == container's project`) and has `strings.EqualFold(c.State, "running")`. Not found keeps polling (the provider may be mid-recreate). On deadline return `fmt.Errorf("network mode provider %s is not running (waited %s)", target, networkModeWaitTimeout)`.

In `RecreateWithCompose`:
- Replace the `docker stop` / `docker rm` `exec.CommandContext` calls with `dockerCommand(ctx, "stop", container.Name)` and `dockerCommand(ctx, "rm", container.Name)` (still ignoring errors).
- After resolving `serviceName` and before stop/rm, call `r.waitForNetworkModeProvider(ctx, container)` and return its error.
- Replace the `cmd := exec.CommandContext(...)` + `cmd.CombinedOutput()` for the `up` command with `output, err := r.runComposeWithRetry(ctx, args)`.
- In `RecreateMultipleServices`, replace the `up` invocation with `r.runComposeWithRetry(ctx, args)` too (no provider wait — the batch may contain providers).

No change is needed for the label path: `restartContainerByService` (`internal/api/handlers_labels.go:625-661`) already calls `RecreateWithCompose`, so it inherits the wait and retry. The "provider X is not running" error reaches `batch_details` through the existing `failReason = fmt.Sprintf("Compose recreation failed: %v", err)` in `executeBatchUpdate` (line 1119) — verify its wording in the test above, do not add a second wrapper.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/compose/ -run 'TestIsNetworkNamespaceRace|TestRecreateWithCompose' -v`
Expected: PASS (existing validation tests too).

- [ ] **Step 5: Commit**

```bash
git add internal/compose/recreate.go internal/compose/recreate_test.go
git commit -m "fix(compose): wait for and retry around restarting network_mode providers"
```

---

### Task 2: Keep the containers list rendered from a last-good snapshot

**Files:**
- Create: `ui/src/api/snapshot.ts`
- Modify: `ui/src/hooks/useContainersData.ts`
- Modify: `ui/src/components/Containers.tsx:1125-1177` (loading and error gates)

**Interfaces:**
- Produces: `getSnapshot(): { explorer?: ExplorerData; status?: DiscoveryResult; updatedAt: number } | null`, `setSnapshot(partial: { explorer?: ExplorerData; status?: DiscoveryResult }): void`, `getCachedInspect(name: string): ContainerInspect | null`, `setCachedInspect(name: string, data: ContainerInspect): void`.
- Consumed by Task 3 (`getCachedInspect`, `setCachedInspect`).

- [ ] **Step 1: Create the snapshot module**

`ui/src/api/snapshot.ts` — a module-level cache that survives route changes and remounts; it must never throw:

```ts
import type { ContainerInspect, DiscoveryResult, ExplorerData } from '../types/api';

interface Snapshot {
  explorer?: ExplorerData;
  status?: DiscoveryResult;
  updatedAt: number;
}

let snapshot: Snapshot | null = null;
const inspects = new Map<string, ContainerInspect>();

export function getSnapshot(): Snapshot | null {
  return snapshot;
}

export function setSnapshot(partial: { explorer?: ExplorerData; status?: DiscoveryResult }): void {
  snapshot = { ...snapshot, ...partial, updatedAt: Date.now() };
}

export function getCachedInspect(name: string): ContainerInspect | null {
  return inspects.get(name) ?? null;
}

export function setCachedInspect(name: string, data: ContainerInspect): void {
  inspects.set(name, data);
}
```

- [ ] **Step 2: Seed and decouple `useContainersData`**

In `ui/src/hooks/useContainersData.ts`:

- Import `getSnapshot, setSnapshot` from `../api/snapshot`.
- Initialise:
  ```ts
  const [loading, setLoading] = useState(() => !getSnapshot()?.explorer);
  const [explorerData, setExplorerData] = useState<ExplorerData | null>(() => getSnapshot()?.explorer ?? null);
  const [statusData, setStatusData] = useState<DiscoveryResult | null>(() => getSnapshot()?.status ?? null);
  const hasInitialData = useRef(!!getSnapshot()?.explorer);
  ```
- Replace `fetchData`'s body with two independently-settling requests, so neither owns the loading gate:

  ```ts
  const fetchData = useCallback(async () => {
    const explorerPromise = getExplorerData()
      .then(res => {
        if (res.success && res.data) {
          setExplorerData(res.data);
          setSnapshot({ explorer: res.data });
          hasInitialData.current = true;
          setError(null);
        } else if (!hasInitialData.current) {
          setError(res.error || 'Failed to fetch container data');
        }
      })
      .catch(err => {
        if (!hasInitialData.current) setError(err instanceof Error ? err.message : 'Unknown error');
      })
      .finally(() => setLoading(false));

    const statusPromise = getContainerStatus()
      .then(res => {
        if (res.success && res.data) {
          setStatusData(res.data);
          setSnapshot({ status: res.data });
        }
      })
      .catch(() => { /* status is optional; explorer already carries the list */ });

    await Promise.allSettled([explorerPromise, statusPromise]);
  }, []);
  ```

  `loading` is cleared by the explorer request alone. A failure with an existing snapshot leaves `error` null and keeps the stale list on screen.
- In `runRefresh`, after a successful `getExplorerData()` / status fetch, call `setSnapshot({ explorer: ..., status: ... })` alongside the `setX` calls.
- In `cacheRefresh`, call `setSnapshot` on success and only `setLoading(true)` when there is no current explorer data (`if (!explorerData) setLoading(true)`); keep `setError(null)` at entry.

- [ ] **Step 3: Stop the component blanking and keep the stale chip**

In `ui/src/components/Containers.tsx`:

- Change `if (loading) {` (line 1125) to `if (loading && containers.length === 0) {`.
- Change the header chip condition (line 1211) from `reconnecting` to `(reconnecting || !!error)`.
- Add above the main list (inside the non-skeleton return, before the header) a stale notice when a refresh failed but data is on screen:

  ```tsx
  {error && containers.length > 0 && (
    <div className="stale-data-banner" role="status">
      <i className="fa-solid fa-triangle-exclamation"></i>
      <span>Showing last known data — {error}</span>
    </div>
  )}
  ```

  The error branch at line 1164 already requires `!containers.length`, so it keeps handling the cold no-data case.

- [ ] **Step 4: Add the banner style**

In `ui/src/styles/base.css`, next to `.active-operation-banner` (line 221), add `.stale-data-banner` and `.connection-banner` sharing the banner's background/padding with an amber left border; keep them `position: sticky; top: 0; z-index: 50`.

- [ ] **Step 5: Verify build and lint**

Run: `cd ui && npm run build`
Expected: clean `tsc -b` + Vite build.
Run: `npx eslint src/api/snapshot.ts src/hooks/useContainersData.ts src/components/Containers.tsx`
Expected: no new errors.

Manual check (no UI test runner exists): load `/`, wait for data, then DevTools → Network → Offline, press the refresh button. The list stays visible with the stale banner; going back online and pressing refresh restores the chip-free header. A fresh tab with Offline shows the `explorer-error` state, not a skeleton.

- [ ] **Step 6: Commit**

```bash
git add ui/src/api/snapshot.ts ui/src/hooks/useContainersData.ts ui/src/components/Containers.tsx ui/src/styles/base.css
git commit -m "fix(ui): keep the containers list rendered from a last-good snapshot"
```

---

### Task 3: Container page renders its shell and cached details while `inspect` is pending

**Files:**
- Modify: `ui/src/pages/ContainerPage.tsx:55-58, 140-154, 286-289, 550-592`

**Interfaces:**
- Consumes: `getCachedInspect`, `setCachedInspect` from Task 2; `useEventStream().reconnecting`.

- [ ] **Step 1: Seed from the per-container cache**

- Import `getCachedInspect, setCachedInspect` from `../api/snapshot` and `useEventStream` from `../context/EventStreamContext`.
- Change the state declarations (lines 55-57):
  ```ts
  const [loading, setLoading] = useState(() => !getCachedInspect(containerName));
  const [inspectData, setInspectData] = useState<ContainerInspect | null>(() => getCachedInspect(containerName));
  ```
- In `fetchInspect`, on success add `setCachedInspect(containerName, response.data)` before `setInspectData`.
- Add a reset effect so route-param navigation between containers reseeds:
  ```ts
  useEffect(() => {
    const cached = getCachedInspect(containerName);
    setInspectData(cached);
    setLoading(!cached);
    setError(null);
  }, [containerName]);
  ```
- Add `const { reconnecting } = useEventStream();`.

- [ ] **Step 2: In-place retry instead of a reload**

Replace the error-state Retry handler (line 587) `onClick={() => window.location.reload()}` with a `retry` callback defined above:

```ts
const retry = useCallback(() => {
  setError(null);
  setLoading(!getCachedInspect(containerName));
  fetchInspect();
  fetchDocksmithData();
}, [containerName, fetchInspect, fetchDocksmithData]);
```

and use `onClick={retry}`.

- [ ] **Step 3: Make the loading state say what it is waiting on**

In the loading branch (lines 551-569), keep the header and Back button, and replace `<main className="main-loading">` content with:

```tsx
<main className="main-loading">
  <div className="loading-spinner">
    <i className="fa-solid fa-circle-notch fa-spin"></i>
  </div>
  <p className="loading-hint">
    {reconnecting
      ? 'Reconnecting to Docksmith — the page will fill in when it responds.'
      : 'Waiting for a response from Docksmith…'}
  </p>
  <button className="retry-btn" onClick={retry}>Retry now</button>
</main>
```

Add `.loading-hint` and `.retry-btn` to `ui/src/styles/container-page.css`.

- [ ] **Step 4: Show the reconnecting chip in the page header**

In the main (loaded) header, next to `.header-badges`, render when `reconnecting`:

```tsx
{reconnecting && (
  <div className="connection-status reconnecting" title="Reconnecting...">
    <i className="fa-solid fa-wifi fa-fade"></i>
    <span>Reconnecting...</span>
  </div>
)}
```

- [ ] **Step 5: Verify build and lint**

Run: `cd ui && npm run build && npx eslint src/pages/ContainerPage.tsx`
Expected: no errors.

Manual check: open a container, go Offline, navigate to another container — the header and Back button render immediately with the waiting hint and a working Retry; going online and pressing Retry fills the page without a reload. Reopen a previously visited container while Offline — cached details render immediately.

- [ ] **Step 6: Commit**

```bash
git add ui/src/pages/ContainerPage.tsx ui/src/styles/container-page.css
git commit -m "fix(ui): render the container page from cache while inspect is pending"
```

---

### Task 4: Global reconnecting banner

**Files:**
- Modify: `ui/src/App.tsx` (`AppContent`, near the active-operation banner render)

**Interfaces:**
- Consumes: `useEventStream().reconnecting` (already imported in `App.tsx`).

- [ ] **Step 1: Render the banner**

In `AppContent`, destructure `reconnecting` from the existing `useEventStream()` call. Render immediately above the active-operation banner (or at the top of the content area when no operation is active):

```tsx
{reconnecting && (
  <div className="connection-banner" role="status">
    <i className="fa-solid fa-wifi fa-fade"></i>
    <span>Reconnecting to Docksmith…</span>
  </div>
)}
```

CSS for `.connection-banner` was added in Task 2 Step 4.

- [ ] **Step 2: Verify build and lint**

Run: `cd ui && npm run build && npx eslint src/App.tsx`
Expected: no errors.

Manual check: DevTools → Offline. The banner appears within one reconnect cycle (≤15s) on `/`, `/operation`, and `/container/...`, and disappears on reconnect without a reload.

- [ ] **Step 3: Commit**

```bash
git add ui/src/App.tsx
git commit -m "feat(ui): show a global reconnecting banner"
```

---

### Task 5: Define ingress server-side and warn before bulk actions

**Files:**
- Create: `internal/docker/ingress.go`
- Test: `internal/docker/ingress_test.go`
- Modify: `internal/docker/service.go:465-529`
- Modify: `ui/src/types/api.ts:315-321, 430-466`
- Modify: `ui/src/hooks/useContainersData.ts` (`mergeOne`)
- Modify: `ui/src/components/Containers.tsx:1095-1122, 1390-1393`

**Interfaces:**
- Produces: `docker.IngressCandidate{Name, Image string; Labels map[string]string}`, `docker.IsIngressContainer(name, image string) bool`, `docker.ComputeIngress(candidates []IngressCandidate) map[string]bool`; JSON fields `is_ingress`, `is_self` on every `ContainerExplorerItem`; TS fields `is_ingress?: boolean`, `is_self?: boolean` on `ContainerExplorerItem` and `UnifiedContainerItem`.

- [ ] **Step 1: Write the failing test**

`internal/docker/ingress_test.go`:

```go
package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsIngressContainer(t *testing.T) {
	assert.True(t, IsIngressContainer("traefik", "traefik:v3.1"))
	assert.True(t, IsIngressContainer("traefik-ts", "ghcr.io/traefik/traefik:v3.1"))
	assert.True(t, IsIngressContainer("tailscale", "tailscale/tailscale:latest"))
	assert.False(t, IsIngressContainer("qbittorrent", "lscr.io/linuxserver/qbittorrent:latest"))
	assert.False(t, IsIngressContainer("my-traefik-admin", "nginx:latest"))
}

func TestComputeIngress_TraefikEnabledAppIsNotIngress(t *testing.T) {
	got := ComputeIngress([]IngressCandidate{
		{Name: "traefik", Image: "traefik:v3.1", Labels: map[string]string{"com.docker.compose.service": "traefik"}},
		{Name: "kometa", Image: "kometar/kometa:latest", Labels: map[string]string{
			"com.docker.compose.service": "kometa",
			"traefik.enable":             "true",
		}},
	})
	assert.True(t, got["traefik"])
	assert.False(t, got["kometa"], "a container merely routed by Traefik is not ingress")
}

func TestComputeIngress_FollowsNetworkMode(t *testing.T) {
	got := ComputeIngress([]IngressCandidate{
		{Name: "tailscale", Image: "tailscale/tailscale:latest", Labels: map[string]string{"com.docker.compose.service": "tailscale"}},
		{Name: "traefik-ts", Image: "nginx:latest", Labels: map[string]string{
			"com.docker.compose.service":      "traefik-ts",
			"com.docker.compose.network_mode": "service:tailscale",
		}},
		{Name: "qbittorrent", Image: "lscr.io/linuxserver/qbittorrent:latest", Labels: map[string]string{
			"com.docker.compose.service":      "qbittorrent",
			"com.docker.compose.network_mode": "service:gluetun",
		}},
	})
	assert.True(t, got["tailscale"])
	assert.True(t, got["traefik-ts"], "a container sharing an ingress container's netns is ingress")
	assert.False(t, got["qbittorrent"])
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/docker/ -run 'TestIsIngressContainer|TestComputeIngress' -v`
Expected: FAIL — `undefined: IsIngressContainer`.

- [ ] **Step 3: Implement ingress classification**

`internal/docker/ingress.go`:

```go
package docker

import "strings"

// IngressCandidate is the subset of container identity needed to classify ingress.
type IngressCandidate struct {
	Name   string
	Image  string
	Labels map[string]string
}

// IsIngressContainer reports whether recreating this container takes down the
// Docksmith UI's own route: a Traefik or Tailscale instance.
func IsIngressContainer(name, image string) bool { ... }

// ComputeIngress returns a set of container names that serve as (or depend on)
// the UI's ingress path.
func ComputeIngress(candidates []IngressCandidate) map[string]bool { ... }
```

`IsIngressContainer` compares `imageRepository(image)` (strip digest after `@`, tag after `:`, take the last `/` segment) against `"traefik"`/`"tailscale"`, and falls back to the lower-cased name matching `traefik`, `traefik-*`/`traefik_*`, `tailscale`, `tailscale-*`/`tailscale_*`.

`ComputeIngress` first marks candidates where `IsIngressContainer`, then repeats until stable: for each unmarked candidate whose `com.docker.compose.network_mode` label is `service:X` or `container:X`, mark it when `X` names an already-marked candidate's container name or its `com.docker.compose.service` label.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/docker/ -run 'TestIsIngressContainer|TestComputeIngress' -v`
Expected: PASS.

- [ ] **Step 5: Expose the flags on the explorer response**

In `internal/docker/service.go`:
- Add to `ContainerExplorerItem`:
  ```go
  IsIngress bool `json:"is_ingress"`
  IsSelf    bool `json:"is_self"`
  ```
- In `ListContainersForExplorer`, build `[]IngressCandidate` from `rawContainers` (`Name` trimmed of `/`, `Image: c.Image`, `Labels: c.Labels`) and call `ComputeIngress` once. Set `IsIngress: ingress[name]` and `IsSelf: selfupdate.IsSelfContainer(c.ID, c.Image, name)` on each item. `selfupdate` imports only stdlib, so there is no cycle.

- [ ] **Step 6: Carry the flags through the UI types and warning**

- `ui/src/types/api.ts`: add `is_ingress?: boolean; is_self?: boolean;` to `ContainerExplorerItem` and `UnifiedContainerItem`.
- `ui/src/hooks/useContainersData.ts` `mergeOne`: copy `is_ingress: explorer.is_ingress, is_self: explorer.is_self`.
- `ui/src/components/Containers.tsx` `selectedAnalysis` (line 1095): add
  ```ts
  hasIngress: selected.some(c => c.is_ingress),
  ```
  and render next to the self-update warning (line 1391):
  ```tsx
  {selectedAnalysis.hasIngress && (
    <div className="ingress-warning"><i className="fa-solid fa-triangle-exclamation"></i><span>Selected containers serve the Docksmith UI. You will lose access until they restart.</span></div>
  )}
  ```
  In `ui/src/styles/base.css`, add `.ingress-warning` to the same rule block as `.self-update-warning`.

- [ ] **Step 7: Verify**

Run: `go test ./internal/docker/ -run 'TestIsIngressContainer|TestComputeIngress' -v`
Run: `cd ui && npm run build && npx eslint src/types/api.ts src/hooks/useContainersData.ts src/components/Containers.tsx`
Expected: both clean.

- [ ] **Step 8: Commit**

```bash
git add internal/docker/ingress.go internal/docker/ingress_test.go internal/docker/service.go ui/src/types/api.ts ui/src/hooks/useContainersData.ts ui/src/components/Containers.tsx ui/src/styles/base.css
git commit -m "feat: classify ingress containers and warn before acting on them"
```

---

### Task 6: Order ingress containers last in batch updates

**Files:**
- Create: `internal/update/ingress_order_test.go`
- Modify: `internal/update/update_orchestrator.go:1055-1091, 1099`
- Modify: `ui/src/components/Containers.tsx:520-522` (pass ingress names into the operation)
- Modify: `ui/src/pages/OperationProgressPage.tsx:40, 100-140`

**Interfaces:**
- Consumes: `docker.IsIngressContainer` (Task 5).
- Produces: `func orderIngressLast(containers []*docker.Container) []*docker.Container`.

- [ ] **Step 1: Write the failing test**

`internal/update/ingress_order_test.go`:

```go
package update

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/chis/docksmith/internal/docker"
)

func TestOrderIngressLast(t *testing.T) {
	in := []*docker.Container{
		{Name: "tailscale", Image: "tailscale/tailscale:latest"},
		{Name: "kometa", Image: "kometar/kometa:latest"},
		{Name: "traefik-ts", Image: "ghcr.io/traefik/traefik:v3"},
		{Name: "flaresolverr", Image: "flaresolverr/flaresolverr:latest"},
		{Name: "docksmith", Image: "ghcr.io/rooday/docksmith:1.2.3"},
	}
	got := orderIngressLast(in)
	names := make([]string, len(got))
	for i, c := range got {
		names[i] = c.Name
	}
	assert.Equal(t, []string{"kometa", "flaresolverr", "docksmith", "tailscale", "traefik-ts"}, names)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run 'TestOrderIngressLast' -v`
Expected: FAIL — `undefined: orderIngressLast`.

- [ ] **Step 3: Implement stable ingress-last ordering**

In `internal/update/update_orchestrator.go`, add:

```go
// orderIngressLast moves ingress containers after all others, preserving the
// relative order within each group. Dependency ordering still decides within
// the non-ingress group; this only makes the UI outage as short as possible.
func orderIngressLast(containers []*docker.Container) []*docker.Container {
	last := make([]*docker.Container, 0, len(containers))
	rest := make([]*docker.Container, 0, len(containers))
	for _, c := range containers {
		if docker.IsIngressContainer(c.Name, c.Image) {
			last = append(last, c)
			continue
		}
		rest = append(rest, c)
	}
	return append(rest, last...)
}
```

Then in `executeBatchUpdate`, immediately after building `orderedContainers` (after the "If not in dependency graph, add remaining" block, line ~1091):

```go
orderedContainers = orderIngressLast(orderedContainers)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run 'TestOrderIngressLast' -v`
Expected: PASS.

- [ ] **Step 5: Give the operation page a coherent offline message**

- In `ui/src/components/Containers.tsx` `handleBulkUpdate`: add
  `const ingressNames = allSelected.filter(c => c.is_ingress).map(c => c.name);`
  and include it in the update navigation payload (line 521):
  `navigate('/operation', { state: { update: { containers: containersToUpdate, ingress_names: ingressNames } } });`
  (Do the same for the `mixed` payload at line 513 only if `ingressNames.length > 0` — otherwise omit the key so existing paths are unchanged.)
- In `ui/src/pages/OperationProgressPage.tsx`, read it in the page (the recovery path after a refresh has no location state and falls back to the generic text):
  ```ts
  const ingressNames = (location.state as { update?: { ingress_names?: string[] } } | null)?.update?.ingress_names ?? [];
  ```
  Destructure `reconnecting` from the existing `useEventStream()` call (line 40), and render near the operation header:
  ```tsx
  {reconnecting && (
    <div className="operation-offline-note" role="status">
      <i className="fa-solid fa-wifi fa-fade"></i>
      <span>
        Connection to Docksmith lost. The operation continues on the server.{' '}
        {ingressNames.length > 0
          ? `Waiting for ${ingressNames.join(', ')} to come back — the UI stays offline until ingress restarts.`
          : 'Progress resumes here automatically.'}
      </span>
    </div>
  )}
  ```
  Stage log lines emitted during the outage are not replayed (the server keeps going); the post-reconnect status catch-up restores terminal per-container states. This note is the user-facing explanation — do not try to replay stage events.

Add `.operation-offline-note` to `ui/src/pages/OperationProgressPage.css` using the same amber banner treatment as `.stale-data-banner`.

- [ ] **Step 6: Verify build and lint**

Run: `cd ui && npm run build && npx eslint src/pages/OperationProgressPage.tsx`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/update/update_orchestrator.go internal/update/ingress_order_test.go ui/src/components/Containers.tsx ui/src/pages/OperationProgressPage.tsx ui/src/pages/OperationProgressPage.css
git commit -m "feat(update): recreate ingress containers last and explain the outage"
```

---

### Task 7: Cache `/api/explorer` with a short TTL and single flight

**Files:**
- Create: `internal/api/explorer_cache.go`
- Test: `internal/api/explorer_cache_test.go`
- Modify: `internal/api/handlers_explorer.go:23-111`
- Modify: `internal/api/server.go:27-50, 61+`

**Interfaces:**
- Produces: `newExplorerCache(ttl time.Duration) *explorerCache`, `(*explorerCache).get(ctx context.Context, key string, compute func(context.Context) (*ExplorerData, error)) (*ExplorerData, error)`; `Server.explorerCache *explorerCache`; `(*Server).computeExplorer(ctx context.Context, includeVolumeSizes bool) (*ExplorerData, error)`.

- [ ] **Step 1: Write the failing tests**

`internal/api/explorer_cache_test.go`:

```go
package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExplorerCache_SingleFlight(t *testing.T) {
	c := newExplorerCache(time.Minute)
	var calls int32
	compute := func(ctx context.Context) (*ExplorerData, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		return &ExplorerData{}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.get(context.Background(), "default", compute)
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestExplorerCache_RespectsTTL(t *testing.T) {
	c := newExplorerCache(10 * time.Millisecond)
	var calls int32
	compute := func(ctx context.Context) (*ExplorerData, error) {
		atomic.AddInt32(&calls, 1)
		return &ExplorerData{}, nil
	}

	_, err := c.get(context.Background(), "default", compute)
	require.NoError(t, err)
	_, err = c.get(context.Background(), "default", compute)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "second call inside the TTL must be served from cache")

	time.Sleep(20 * time.Millisecond)
	_, err = c.get(context.Background(), "default", compute)
	require.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "after the TTL the cache recomputes")
}

func TestExplorerCache_DoesNotCacheErrors(t *testing.T) {
	c := newExplorerCache(time.Minute)
	compute := func(ctx context.Context) (*ExplorerData, error) {
		return nil, errors.New("docker down")
	}
	_, err := c.get(context.Background(), "default", compute)
	require.Error(t, err)

	compute = func(ctx context.Context) (*ExplorerData, error) { return &ExplorerData{}, nil }
	got, err := c.get(context.Background(), "default", compute)
	require.NoError(t, err)
	assert.NotNil(t, got)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run 'TestExplorerCache' -v`
Expected: FAIL — `undefined: newExplorerCache`.

- [ ] **Step 3: Implement the cache**

`internal/api/explorer_cache.go`:

```go
package api

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const explorerCacheTTL = 3 * time.Second

type explorerEntry struct {
	data *ExplorerData
	at   time.Time
}

type explorerCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]explorerEntry
	group   singleflight.Group
}

func newExplorerCache(ttl time.Duration) *explorerCache { ... }
```

`get` returns a fresh entry directly; otherwise calls `group.Do(key, ...)`, re-checks the entry inside the flight, calls `compute(ctx)`, and stores only on success (`data != nil && err == nil`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run 'TestExplorerCache' -v`
Expected: PASS.

- [ ] **Step 5: Wire it into the handler and server**

- `internal/api/server.go`: add `explorerCache *explorerCache` to `Server`; initialise `explorerCache: newExplorerCache(explorerCacheTTL)` in `NewServer`.
- `internal/api/handlers_explorer.go`: rename the current handler body into `func (s *Server) computeExplorer(ctx context.Context, includeVolumeSizes bool) (*ExplorerData, error)` returning `*ExplorerData` instead of writing a response (replace both `RespondInternalError` calls with `return nil, err`). `handleExplorer` becomes:

  ```go
  key := "default"
  data, err := s.explorerCache.get(r.Context(), key, func(ctx context.Context) (*ExplorerData, error) {
      return s.computeExplorer(ctx, false)
  })
  if err != nil {
      RespondInternalError(w, err)
      return
  }
  RespondSuccess(w, data)
  ```

  Keep `includeVolumeSizes` as a parameter for Task 8 (unused `false` here is replaced there).

- [ ] **Step 6: Commit**

```bash
git add internal/api/explorer_cache.go internal/api/explorer_cache_test.go internal/api/handlers_explorer.go internal/api/server.go
git commit -m "perf(api): cache explorer responses for 3s with single-flight"
```

---

### Task 8: Make volume sizes opt-in for `/api/explorer`

**Files:**
- Modify: `internal/docker/service.go:388-463`
- Modify: `internal/api/handlers_explorer.go`
- Modify: `ui/src/api/client.ts:306-308`
- Modify: `ui/src/hooks/useContainersData.ts`
- Modify: `ui/src/components/Containers.tsx:111, 114`

**Interfaces:**
- Consumes: `computeExplorer(ctx, includeVolumeSizes)` from Task 7.
- Produces: `ListVolumesWithContainers(ctx context.Context, containers []container.Summary, includeSizes bool) ([]VolumeInfo, error)`; `getExplorerData(options?: { includeVolumeSizes?: boolean })`; `useContainersData(options?: { includeVolumeSizes?: boolean })`.

- [ ] **Step 1: Add the sizes flag to the Docker service**

Change `ListVolumesWithContainers` to take `includeSizes bool`. When false, skip the `s.cli.DiskUsage` call entirely so every volume keeps `Size: -1`. Update `ListVolumes` (line 395) to pass `true`. Update the `handleExplorer` caller.

- [ ] **Step 2: Read the flag in the handler**

In `handleExplorer`, compute `includeSizes := r.URL.Query().Get("include_volume_sizes") == "true"`, use it as the cache key (`"sizes"` / `"default"`), and pass it through `computeExplorer`.

- [ ] **Step 3: Let the UI request sizes only for the Volumes tab**

- `ui/src/api/client.ts`:
  ```ts
  export async function getExplorerData(options?: { includeVolumeSizes?: boolean }): Promise<ExplorerResponse> {
    return fetchAPI(`/explorer${options?.includeVolumeSizes ? '?include_volume_sizes=true' : ''}`);
  }
  ```
- `ui/src/hooks/useContainersData.ts`: change the signature to `useContainersData(options: { includeVolumeSizes?: boolean } = {})`, read `const { includeVolumeSizes = false } = options`, pass `{ includeVolumeSizes }` to every `getExplorerData()` call, and add:
  ```ts
  const mountedOnce = useRef(false);
  useEffect(() => {
    if (!mountedOnce.current) { mountedOnce.current = true; return; }
    fetchData();
  }, [includeVolumeSizes, fetchData]);
  ```
- `ui/src/components/Containers.tsx`: move the existing `useContainersData()` call (line 111) below the `activeSubTab` state (line 114) and call `useContainersData({ includeVolumeSizes: activeSubTab === 'volumes' })`.

- [ ] **Step 4: Verify**

Run: `go test ./internal/docker/ -run 'TestIsIngressContainer|TestComputeIngress' -v` (unchanged package still builds).
Run: `cd ui && npm run build && npx eslint src/api/client.ts src/hooks/useContainersData.ts src/components/Containers.tsx`
Expected: clean.

Manual check: open `/`, then the Volumes tab — sizes populate after one request; returning to Containers does not blank the page.

- [ ] **Step 5: Commit**

```bash
git add internal/docker/service.go internal/api/handlers_explorer.go ui/src/api/client.ts ui/src/hooks/useContainersData.ts ui/src/components/Containers.tsx
git commit -m "perf(api): make explorer volume sizes opt-in"
```

---

### Task 9: Serialize label read-modify-write per compose file

**Files:**
- Create: `internal/compose/lock.go`
- Test: `internal/compose/lock_test.go`
- Modify: `internal/api/handlers_labels.go:318-344`
- Modify: `internal/api/server.go:27-50, 61+`

**Interfaces:**
- Produces: `compose.NewKeyedMutex() *compose.KeyedMutex`, `(*compose.KeyedMutex).Lock(key string) (unlock func())`; `Server.composeMu *compose.KeyedMutex`.

- [ ] **Step 1: Write the failing tests**

`internal/compose/lock_test.go`:

```go
package compose

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyedMutex_SerializesSameKey(t *testing.T) {
	m := NewKeyedMutex()
	unlockA := m.Lock("a")
	acquired := make(chan struct{})
	go func() {
		unlock := m.Lock("a")
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
		t.Fatal("second Lock must block while the first holds the key")
	case <-time.After(20 * time.Millisecond):
	}
	unlockA()
	<-acquired
}

func TestKeyedMutex_ConcurrentLabelWritesSurvive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`services:
  gluetun:
    image: qmcgaw/gluetun:latest
    labels:
      docksmith.ignore: "false"
  flaresolverr:
    image: flaresolverr/flaresolverr:latest
    labels:
      docksmith.ignore: "false"
`), 0644))

	m := NewKeyedMutex()
	var wg sync.WaitGroup
	for _, svc := range []struct{ name, label string }{
		{"gluetun", "docksmith.version-pin-minor"},
		{"flaresolverr", "docksmith.version-pin-patch"},
	} {
		wg.Add(1)
		go func(name, label string) {
			defer wg.Done()
			unlock := m.Lock(path)
			defer unlock()

			cf, err := LoadComposeFile(path)
			require.NoError(t, err)
			service, err := cf.FindServiceByContainerName(name)
			require.NoError(t, err)
			require.NoError(t, service.SetLabel(label, "true"))
			require.NoError(t, cf.Save())
		}(svc.name, svc.label)
	}
	wg.Wait()

	cf, err := LoadComposeFile(path)
	require.NoError(t, err)
	gluetun, err := cf.FindServiceByContainerName("gluetun")
	require.NoError(t, err)
	flaresolverr, err := cf.FindServiceByContainerName("flaresolverr")
	require.NoError(t, err)

	gluetunLabels, err := gluetun.GetAllLabels()
	require.NoError(t, err)
	flaresolverrLabels, err := flaresolverr.GetAllLabels()
	require.NoError(t, err)
	assert.Equal(t, "true", gluetunLabels["docksmith.version-pin-minor"])
	assert.Equal(t, "true", flaresolverrLabels["docksmith.version-pin-patch"])
}
```

If `GetLabels` is unexported, use the exported accessor already present in `editor.go` (`GetAllLabels`, line 321); do not add a new one.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/compose/ -run 'TestKeyedMutex' -v`
Expected: FAIL — `undefined: NewKeyedMutex`.

- [ ] **Step 3: Implement `KeyedMutex`**

`internal/compose/lock.go`:

```go
package compose

import "sync"

// KeyedMutex serializes critical sections that share a key — used to make
// compose read-modify-write cycles on the same file atomic.
type KeyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func NewKeyedMutex() *KeyedMutex { ... }

// Lock blocks until the key is free and returns the matching unlock.
func (k *KeyedMutex) Lock(key string) func() { ... }
```

`Lock` takes the map mutex, creates/increments the entry, releases the map mutex, locks the entry, and returns a func that unlocks the entry then decrements `refs` and deletes the entry at zero under the map mutex.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/compose/ -run 'TestKeyedMutex' -v`
Expected: PASS.

- [ ] **Step 5: Hold the lock across the label read-modify-write**

- `internal/api/server.go`: add `composeMu *compose.KeyedMutex` to `Server` and initialise it in `NewServer`.
- `internal/api/handlers_labels.go` `executeLabelOperationAsync`, Stage 2 (lines 318-344): wrap load → find service → modify → save in a closure that defers the unlock, keyed by `composeFilePath`:

  ```go
  unlock := s.composeMu.Lock(composeFilePath)
  modified, removed, err := func() (map[string]string, []string, error) {
      defer unlock()
      composeFile, err := compose.LoadComposeFileOrIncluded(composeFilePath, cfg.containerName)
      if err != nil { return nil, nil, err }
      service, err := composeFile.FindServiceByContainerName(serviceName)
      if err != nil { return nil, nil, err }
      modified, removed, err := modify(service)
      if err != nil { return nil, nil, err }
      if err := composeFile.Save(); err != nil { return nil, nil, err }
      return modified, removed, nil
  }()
  if err != nil {
      s.failLabelOperationWithEvent(ctx, operationID, cfg.containerName, stackName, fmt.Sprintf("failed to update compose file: %v", err))
      return
  }
  ```

  Keep the per-error messages the existing code uses by mapping the returned error's text (do not change user-facing wording beyond dropping the duplicated prefixes).

- [ ] **Step 6: Verify**

Run: `go test ./internal/compose/ -run 'TestKeyedMutex' -v`
Run: `go build ./...`
Expected: PASS / clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/compose/lock.go internal/compose/lock_test.go internal/api/handlers_labels.go internal/api/server.go
git commit -m "fix(api): serialize compose label writes per file"
```

---

### Task 10: Make "migrate off `:latest`" legible as Pin

**Files:**
- Modify: `ui/src/components/Containers.tsx:842-845, 1095-1122, 1391-1458`
- Modify: `ui/src/pages/ContainerPage.tsx:763-792` (Pin button and version-pin section label)
- Modify: `ui/src/styles/base.css` (badge/confirm styles)

**Interfaces:**
- Consumes: `update_status === 'UP_TO_DATE_PINNABLE'`, `using_latest_tag`, `current_tag`, `recommended_tag` from `UnifiedContainerItem`.
- No backend changes.

- [ ] **Step 1: Add the pinnable bucket**

In `selectedAnalysis` (line 1095):
```ts
const pinnable = updatable.filter(c => c.update_status === 'UP_TO_DATE_PINNABLE');
const rebuildCount = updatable.filter(c =>
  (c.change_type === ChangeType.NoChange || c.change_type === ChangeType.UnknownChange) &&
  c.update_status !== 'UPDATE_AVAILABLE_BLOCKED' &&
  c.update_status !== 'UP_TO_DATE_PINNABLE'
).length;
...
pinnableCount: pinnable.length,
allActionablePinnable: updatable.length > 0 && pinnable.length === updatable.length && mismatches.length === 0,
```

- [ ] **Step 2: Show a distinct badge**

Next to the Rebuild badge (line 1402) add:
```tsx
{selectedAnalysis.pinnableCount > 0 && <span className="update-type-badge pinnable">{selectedAnalysis.pinnableCount} Pinnable</span>}
```
Add `.update-type-badge.pinnable` styling (same shape as `.rebuild`, distinct colour) in `base.css`.

- [ ] **Step 3: Label the primary button and confirm the targets**

- Button label (line 1456): `selectedAnalysis.allActionablePinnable ? 'Pin to recommended tags' : selectedAnalysis.hasBlocked ? 'Force Update' : 'Update'`.
- Keep the bolt icon only for the force case.
- Add state `const [showPinConfirm, setShowPinConfirm] = useState(false);`.
- Primary button click: `selectedAnalysis.allActionablePinnable ? setShowPinConfirm(true) : handleBulkUpdate()`.
- Render a confirm dialog just above the selection action bar:
  ```tsx
  {showPinConfirm && (
    <div className="pin-confirm" role="dialog" aria-label="Pin to recommended tags">
      <p>Replace the floating tag with the recommended version for {selectedAnalysis.pinnableCount} container(s):</p>
      <ul>
        {containers.filter(c => selectedContainers.has(c.name) && c.update_status === 'UP_TO_DATE_PINNABLE').map(c => (
          <li key={c.name}><strong>{c.name}</strong>: {c.current_tag || '?'} → {c.recommended_tag || '?'}</li>
        ))}
      </ul>
      <div className="pin-confirm-actions">
        <button onClick={() => setShowPinConfirm(false)}>Cancel</button>
        <button onClick={() => { setShowPinConfirm(false); handleBulkUpdate(); }}>Pin</button>
      </div>
    </div>
  )}
  ```
- Add `.pin-confirm` styles reusing the existing settings-menu surface.

- [ ] **Step 4: Rename the row action**

At line 842, when `c.update_status === 'UP_TO_DATE_PINNABLE'`, render `label="Pin to tag"` instead of `"Update"` (the handler already sends `recommended_tag`).

- [ ] **Step 5: Separate the two pin concepts**

- Bulk Actions dropdown: change the section label at line 1438 from `Pinning` to `Version pinning`, and the buttons to `Clear version pin`, `Version pin · Major`, `Version pin · Minor`, `Version pin · Patch`.
- The primary update button's pinnable mode stays `Pin to recommended tags` (compose image tag).
- In `ui/src/pages/ContainerPage.tsx`, the blue Pin button (line 763-792) becomes `Pin to tag` with tooltip "Replace the floating tag with {recommended_tag}", and the label-based `versionPin` section heading becomes "Version pin".

- [ ] **Step 6: Verify build and lint**

Run: `cd ui && npm run build && npx eslint src/components/Containers.tsx src/pages/ContainerPage.tsx`
Expected: clean.

Manual check: select only `:latest` containers with `UP_TO_DATE_PINNABLE` status → badge reads "N Pinnable", primary button reads "Pin to recommended tags", and the confirm lists `current → recommended`; the resulting operation is a normal update and history records an update to `recommended_tag`.

- [ ] **Step 7: Commit**

```bash
git add ui/src/components/Containers.tsx ui/src/pages/ContainerPage.tsx ui/src/styles/base.css
git commit -m "feat(ui): distinguish pinning to a tag from version pins"
```

---

## Final Verification

- [ ] `go test ./...` — only the documented Windows baseline failures appear.
- [ ] `cd ui && npm run build` — clean.
- [ ] `npx eslint` on every changed UI file — no new errors.
- [ ] Walk the handoff's four item-1 acceptance checks and the item-2/3/4 acceptance lines against the running stack.
- [ ] Whole-branch review before merging.
