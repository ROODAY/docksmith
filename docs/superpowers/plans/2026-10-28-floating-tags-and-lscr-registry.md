# Floating Tags & lscr.io Registry Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make docksmith resolve `lscr.io` LinuxServer images like GHCR, never recommend a tag older than the running version, and surface floating tags with separate updates/pinnable counts and a safe bulk migration path.

**Architecture:** Route the `lscr.io` registry to the existing GHCR client (it proxies to ghcr.io) and read GHCR credentials from either Docker-config key. Add a checker-side guard so a tag-list scan can never set a recommendation older than the running version, and expose `using_floating_tag`. The UI adds floating/inert-pin indicators and splits real updates from pinnable counts and filters; migration reuses the existing update flow via `target_version`.

**Tech Stack:** Go 1.25, React 19 + TypeScript, Vite.

**Spec:** `docs/superpowers/specs/2026-10-28-floating-tags-and-lscr-registry-design.md`

## Global Constraints

- Go 1.25. Backend tests: `go test ./...` from the repo root. UI has no test framework; verify with `cd ui && npm run build` (tsc + vite) and `npm run lint`.
- GHCR credentials: a classic GitHub PAT with `read:packages`, supplied as `GITHUB_TOKEN` (wins) or an inline `auths."ghcr.io"` / `auths."lscr.io"` entry in `/root/.docker/config.json`.
- Never assign `RecommendedTag` or `LatestResolvedVersion` from a candidate older than `CurrentVersion`. If `CurrentVersion` does not parse, a tag-list scan must not produce a recommendation (digest-resolved candidates are exempt — they point at the running digest).
- `using_floating_tag` = the parser's own classification: `imgInfo.Tag.IsLatest || imgInfo.Tag.MetaTag != ""` (covers `latest`, `latest-*`, `stable`, `main`, `master`, `develop`, `dev`, `edge`, `nightly`, `beta`, `alpha`, `rc`, and is correct for unqualified image refs).
- Counts never mix: `UPDATE_AVAILABLE` and `UPDATE_AVAILABLE_BLOCKED` are **Updates**; `UP_TO_DATE_PINNABLE` is **Pinnable**. `isUpdatable` stays true for both because both are actionable; counts and filters use the new split helpers.
- Do not change pin-label semantics. Pin labels remain inert on floating tags, and the UI says so.
- Commit after every task. Start from a feature branch off `main`.

## Review Focus

1. A floating tag whose current version cannot be parsed, with an older tag visible in the list — must not produce a recommendation. Pinned by `TestNoDowngradeRecommendation` (Task 4).
2. The visible tag list is the oldest page (the radarr/lscr case: newest visible is `3.0.0.4039-ls29` while running `6.4.4.10685-ls324`) — status stays `UP_TO_DATE` with empty `LatestResolvedVersion`. Pinned by `TestNoDowngradeRecommendation` (Task 4).
3. Arch-prefixed and `version-` tags (`amd64-6.4.4…`, `version-3.0.0…`) appear in the list — they are never parsed as version targets. Pinned in the same test's tag fixture (Task 4).
4. A `stable`/`main` image — flagged floating, no crash, no bogus recommendation. Pinned by `TestCheckContainerSetsUsingFloatingTag` (Task 3).
5. The Docker config contains only an `lscr.io` auth entry — the token is still found. Pinned by `TestParseGHCRTokenFromDockerConfig` (Task 2).

---

### Task 1: Route lscr.io to the GHCR client (and document credentials)

**Files:**
- Modify: `internal/registry/manager.go:226-237` (`getClient`)
- Test: `internal/registry/manager_test.go`
- Modify: `README.md:89`, `docs/registries.md:150-167`, `docker-compose.example.yml:20-23`

**Interfaces:**
- Consumes: nothing new.
- Produces: `Manager.getClient("lscr.io")` returns the same `*GHCRClient` instance as `"ghcr.io"`; `parseImageRef("lscr.io/linuxserver/radarr")` returns `("lscr.io", "linuxserver/radarr")`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/registry/manager_test.go`:

```go
func TestGetClientAliasesLscrToGHCR(t *testing.T) {
	manager := NewManager("")
	ghcr := manager.getClient("ghcr.io")
	lscr := manager.getClient("lscr.io")

	if ghcr != lscr {
		t.Fatalf("lscr.io and ghcr.io must use the same client instance")
	}
	if _, ok := lscr.(*GHCRClient); !ok {
		t.Fatalf("lscr.io client = %T, want *GHCRClient", lscr)
	}
}
```

Add a `parseImageRef` table case:

```go
{
	name:             "lscr.io image",
	imageRef:         "lscr.io/linuxserver/radarr",
	expectedRegistry: "lscr.io",
	expectedRepo:     "linuxserver/radarr",
},
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/registry -run 'TestGetClientAliasesLscrToGHCR|TestParseImageRef' -v`
Expected: `TestGetClientAliasesLscrToGHCR` FAILS because `lscr.io` currently returns a generic `*HTTPClient`; the new table case passes already.

- [ ] **Step 3: Implement the alias**

In `Manager.getClient`, add before `default`:

```go
case "lscr.io":
	// lscr.io is LinuxServer's registry host and proxies the entire V2 API to GHCR.
	// Send it through the GHCR client so tag ordering and digest lookups match ghcr.io.
	return m.ghcrClient
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/registry -run 'TestGetClientAliasesLscrToGHCR|TestParseImageRef' -v`
Expected: PASS.

- [ ] **Step 5: Update the credential docs**

- `README.md:89`: change the `GITHUB_TOKEN` description to `For private GHCR images and reliable LinuxServer/lscr.io tag resolution`.
- `docs/registries.md` LinuxServer section: state that `lscr.io` is supported as an alias of GHCR and that resolving a version tag from a floating tag requires a classic PAT with `read:packages`, mounted at `/root/.docker/config.json` (or `GITHUB_TOKEN`).
- `docker-compose.example.yml:22`: change the mount target from `/home/docksmith/.docker/config.json` to `/root/.docker/config.json`.

- [ ] **Step 6: Verify the docs**

Run: `grep -n "lscr.io" README.md docs/registries.md && grep -n "root/.docker/config.json" docker-compose.example.yml`
Expected: at least one match in each file.

- [ ] **Step 7: Commit**

```bash
git add internal/registry/manager.go internal/registry/manager_test.go README.md docs/registries.md docker-compose.example.yml
git commit -m "feat(registry): treat lscr.io as a GHCR alias"
```

---

### Task 2: Read GHCR credentials from either Docker-config key

**Files:**
- Modify: `internal/registry/ghcr.go:35-49,106-144`
- Test: `internal/registry/ghcr_creds_test.go` (new)

**Interfaces:**
- Produces: `parseGHCRTokenFromDockerConfig(data []byte) string` — returns the PAT from `auths["ghcr.io"]`, else `auths["lscr.io"]`, else `""`.

- [ ] **Step 1: Write the failing test**

Create `internal/registry/ghcr_creds_test.go`:

```go
package registry

import (
	"encoding/base64"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParseGHCRTokenFromDockerConfig(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{"ghcr entry", `{"auths":{"ghcr.io":{"auth":"` + b64("user:ghcr-token") + `"}}}`, "ghcr-token"},
		{"lscr entry", `{"auths":{"lscr.io":{"auth":"` + b64("user:lscr-token") + `"}}}`, "lscr-token"},
		{"ghcr wins when both", `{"auths":{"ghcr.io":{"auth":"` + b64("u:ghcr") + `"},"lscr.io":{"auth":"` + b64("u:lscr") + `"}}}`, "ghcr"},
		{"no ghcr keys", `{"auths":{"https://index.docker.io/v1/":{"auth":"` + b64("u:hub") + `"}}}`, ""},
		{"malformed base64", `{"auths":{"ghcr.io":{"auth":"!!!"}}}`, ""},
		{"missing colon", `{"auths":{"ghcr.io":{"auth":"` + b64("tokenonly") + `"}}}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGHCRTokenFromDockerConfig([]byte(tt.config)); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/registry -run TestParseGHCRTokenFromDockerConfig -v`
Expected: FAIL — `undefined: parseGHCRTokenFromDockerConfig`.

- [ ] **Step 3: Implement the parser and refactor the file read**

In `ghcr.go`, replace the body of `readGHCRCredsFromDockerConfig` so it reads `$HOME/.docker/config.json` and delegates to:

```go
// parseGHCRTokenFromDockerConfig returns the GHCR personal access token from a
// Docker config. lscr.io is accepted because it proxies to GHCR.
func parseGHCRTokenFromDockerConfig(data []byte) string {
	var config dockerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}
	for _, key := range []string{"ghcr.io", "lscr.io"} {
		auth, found := config.Auths[key]
		if !found {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
		if err != nil {
			continue
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			continue
		}
		return parts[1]
	}
	return ""
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/registry -run TestParseGHCRTokenFromDockerConfig -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/ghcr.go internal/registry/ghcr_creds_test.go
git commit -m "feat(registry): accept lscr.io credentials for GHCR requests"
```

---

### Task 3: Expose `using_floating_tag`

**Files:**
- Modify: `internal/update/types.go:38` (add field after `UsingLatestTag`)
- Modify: `internal/update/checker.go:485-494` (set after `checkTag` is computed)
- Test: `internal/update/checker_floating_test.go` (new)

**Interfaces:**
- Produces: `ContainerUpdate.UsingFloatingTag bool` with JSON key `using_floating_tag`.

- [ ] **Step 1: Write the failing test**

Create `internal/update/checker_floating_test.go`; reuse the mocks in `checker_integration_test.go` (same package):

```go
package update

import (
	"context"
	"testing"

	"github.com/chis/docksmith/internal/docker"
)

func TestCheckContainerSetsUsingFloatingTag(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  bool
	}{
		{"latest", "reg.example/app:latest", true},
		{"latest-alpine", "reg.example/app:latest-alpine", true},
		{"stable", "reg.example/app:stable", true},
		{"main", "reg.example/app:main", true},
		{"pinned semver", "reg.example/app:1.25.3", false},
		{"major only", "reg.example/app:16", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDocker := &mockDockerClient{
				containers:    []docker.Container{{ID: "c", Name: "app", Image: tt.image}},
				imageDigests:  map[string]string{tt.image: "sha256:abc"},
				imageVersions: map[string]string{tt.image: "1.25.3"},
				localImages:   map[string]bool{},
			}
			mockRegistry := &mockRegistryClient{
				tags: map[string][]string{
					"reg.example/app": {"latest", "stable", "main", "1.25.3", "16"},
				},
				tagDigests:     map[string]string{tt.image: "sha256:abc"},
				digestMappings: map[string]map[string][]string{"reg.example/app": {}},
			}
			checker := NewChecker(mockDocker, mockRegistry, newMockStorage())

			result, err := checker.CheckForUpdates(context.Background())
			if err != nil {
				t.Fatalf("CheckForUpdates failed: %v", err)
			}
			if len(result.Updates) != 1 {
				t.Fatalf("got %d updates, want 1", len(result.Updates))
			}
			if got := result.Updates[0].UsingFloatingTag; got != tt.want {
				t.Fatalf("UsingFloatingTag = %v, want %v", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/update -run TestCheckContainerSetsUsingFloatingTag -v`
Expected: FAIL — `result.Updates[0].UsingFloatingTag undefined`.

- [ ] **Step 3: Add the field and set it**

Add to `ContainerUpdate` in `types.go`:

```go
UsingFloatingTag  bool                `json:"using_floating_tag"` // True for latest/latest-*/meta tags with no fixed version
```

In `checkContainer`, immediately after the `checkTag` block and before `update.CurrentSuffix = currentSuffix`:

```go
update.UsingFloatingTag = imgInfo.Tag != nil && (imgInfo.Tag.IsLatest || imgInfo.Tag.MetaTag != "")
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/update -run TestCheckContainerSetsUsingFloatingTag -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/update/types.go internal/update/checker.go internal/update/checker_floating_test.go
git commit -m "feat(update): expose using_floating_tag"
```

---

### Task 4: Never recommend an older tag

**Files:**
- Modify: `internal/update/checker.go` — add helper near `findLatestVersion`; guard `:772`, `:784`, `:922`
- Test: `internal/update/checker_guard_test.go` (new)

**Interfaces:**
- Produces: `(*Checker).isValidPinTarget(currentVersion, candidateTag string) bool` — false when either side does not parse; true when `candidate >= current`.

- [ ] **Step 1: Write the failing unit test**

Create `internal/update/checker_guard_test.go`:

```go
package update

import "testing"

func TestIsValidPinTarget(t *testing.T) {
	checker := NewChecker(nil, nil, nil)
	tests := []struct {
		name    string
		current string
		target  string
		want    bool
	}{
		{"equal", "6.4.4.10685-ls324", "6.4.4.10685-ls324", true},
		{"newer build", "6.4.4.10685-ls324", "6.4.4.10686-ls324", true},
		{"newer minor", "6.4.4.10685-ls324", "6.5.0.1-ls1", true},
		{"older", "6.4.4.10685-ls324", "3.0.0.4039-ls29", false},
		{"unknown current", "", "3.0.0.4039-ls29", false},
		{"unparsable target", "6.4.4.10685-ls324", "nonsense", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checker.isValidPinTarget(tt.current, tt.target); got != tt.want {
				t.Fatalf("isValidPinTarget(%q, %q) = %v, want %v", tt.current, tt.target, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/update -run TestIsValidPinTarget -v`
Expected: FAIL — `checker.isValidPinTarget undefined`.

- [ ] **Step 3: Implement the helper**

```go
// isValidPinTarget reports whether candidateTag is safe to recommend for
// migration: it must parse and be equal to or newer than the running version.
// An unknown current version is never safe.
func (c *Checker) isValidPinTarget(currentVersion, candidateTag string) bool {
	current := c.versionParser.ParseTag(currentVersion)
	candidate := c.versionParser.ParseTag(candidateTag)
	if current == nil || candidate == nil {
		return false
	}
	return c.versionComp.Compare(candidate, current) >= 0
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/update -run TestIsValidPinTarget -v`
Expected: PASS.

- [ ] **Step 5: Write the failing checker test**

Append to `checker_guard_test.go`:

```go
func TestNoDowngradeRecommendation(t *testing.T) {
	mockDocker := &mockDockerClient{
		containers: []docker.Container{
			{ID: "c", Name: "radarr", Image: "lscr.io/linuxserver/radarr:latest"},
		},
		imageDigests:  map[string]string{"lscr.io/linuxserver/radarr:latest": "sha256:current"},
		imageVersions: map[string]string{"lscr.io/linuxserver/radarr:latest": "6.4.4.10685-ls324"},
		localImages:   map[string]bool{},
	}
	mockRegistry := &mockRegistryClient{
		tags: map[string][]string{
			"lscr.io/linuxserver/radarr": {
				"amd64-v0.2.0.1504-ls81", "v0.2.0.1504-ls81",
				"amd64-3.0.0.3978-ls27", "version-3.0.0.3978", "3.0.0.4039-ls29",
			},
		},
		tagDigests: map[string]string{
			"lscr.io/linuxserver/radarr:latest": "sha256:current",
		},
		digestMappings: map[string]map[string][]string{
			"lscr.io/linuxserver/radarr": {},
		},
	}
	checker := NewChecker(mockDocker, mockRegistry, newMockStorage())

	result, err := checker.CheckForUpdates(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdates failed: %v", err)
	}
	update := result.Updates[0]

	if update.Status != UpToDate {
		t.Fatalf("status = %s, want UP_TO_DATE", update.Status)
	}
	if update.LatestResolvedVersion != "" {
		t.Fatalf("LatestResolvedVersion = %q, want empty (no older tag may be recommended)", update.LatestResolvedVersion)
	}
	if update.RecommendedTag != "" {
		t.Fatalf("RecommendedTag = %q, want empty", update.RecommendedTag)
	}
}
```

Add imports for `context` and the docker package to the file.

Also add the positive case, proving the guard accepts the running version:

```go
func TestPinTargetAtRunningVersion(t *testing.T) {
	mockDocker := &mockDockerClient{
		containers: []docker.Container{
			{ID: "c", Name: "radarr", Image: "lscr.io/linuxserver/radarr:latest"},
		},
		imageDigests:  map[string]string{"lscr.io/linuxserver/radarr:latest": "sha256:current"},
		imageVersions: map[string]string{"lscr.io/linuxserver/radarr:latest": "6.4.4.10685-ls324"},
		localImages:   map[string]bool{},
	}
	mockRegistry := &mockRegistryClient{
		tags: map[string][]string{
			"lscr.io/linuxserver/radarr": {"6.4.4.10685-ls324"},
		},
		tagDigests: map[string]string{
			"lscr.io/linuxserver/radarr:latest": "sha256:current",
		},
		digestMappings: map[string]map[string][]string{
			"lscr.io/linuxserver/radarr": {},
		},
	}
	checker := NewChecker(mockDocker, mockRegistry, newMockStorage())

	result, err := checker.CheckForUpdates(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdates failed: %v", err)
	}
	update := result.Updates[0]

	if update.Status != UpToDatePinnable {
		t.Fatalf("status = %s, want UP_TO_DATE_PINNABLE", update.Status)
	}
	if update.RecommendedTag != "6.4.4.10685-ls324" {
		t.Fatalf("RecommendedTag = %q, want the running version tag", update.RecommendedTag)
	}
}
```

- [ ] **Step 6: Run them to verify the negative case fails**

Run: `go test ./internal/update -run 'TestNoDowngradeRecommendation|TestPinTargetAtRunningVersion' -v`
Expected: `TestNoDowngradeRecommendation` FAILS — `LatestResolvedVersion = "3.0.0.4039-ls29"` (the current downgrade bug). `TestPinTargetAtRunningVersion` passes because the current guardless code already accepts the equal version.

- [ ] **Step 7: Apply the guard at all three sites**

- The re-check block (`if update.Status == UpToDate && checkTag == "latest" ...`): add `&& c.isValidPinTarget(update.CurrentVersion, latestVersion)`.
- The meta-tag fallback block (`if isMetaTag(checkTag) && update.LatestResolvedVersion == "" ...`): add `&& c.isValidPinTarget(update.CurrentVersion, latestVersion)`.
- The final pinnable block (`if update.UsingLatestTag && update.CurrentVersion != "" ...`): change the inner condition to `if update.LatestResolvedVersion != "" && c.isValidPinTarget(update.CurrentVersion, update.LatestResolvedVersion)`.

Do not guard the digest-resolution assignments (`resolveVersionFromDigest`); they point at the running digest.

- [ ] **Step 8: Run the test to verify it passes**

Run: `go test ./internal/update -run 'TestNoDowngradeRecommendation|TestPinTargetAtRunningVersion|TestIsValidPinTarget' -v`
Expected: PASS.

- [ ] **Step 9: Run the full backend suite**

Run: `go test ./...`
Expected: PASS (fix any checker expectations that assumed an older recommendation, per the design's risk note).

- [ ] **Step 10: Commit**

```bash
git add internal/update/checker.go internal/update/checker_guard_test.go
git commit -m "fix(update): never recommend a tag older than the running version"
```

---

### Task 5: UI — floating badge and inert-pin warning

**Files:**
- Modify: `ui/src/types/api.ts` — add `using_floating_tag?: boolean` to both `ContainerInfo` (line ~30) and `UnifiedContainerItem` (line ~454)
- Modify: `ui/src/hooks/useContainersData.ts:66-94` — copy the field in `mergeOne`
- Modify: `ui/src/components/Containers.tsx:760-786` (`getStatusBadge`) and `:843` (pin icon)
- Modify: `ui/src/pages/ContainerPage.tsx:505-520` (`getStatusBadge`) and the "Pin to Version" control block
- Modify: `ui/src/styles/containers.css`, `ui/src/styles/container-page.css`

**Interfaces:**
- Consumes: `using_floating_tag` from Task 3; `isUpdatable` from `ui/src/utils/status.ts`.
- Produces: `FLOATING` badge for floating tags with no migration target; a warning next to version pins on floating tags.

- [ ] **Step 1: Add the payload field to both UI types and the merge**

In `ContainerInfo` and `UnifiedContainerItem` in `ui/src/types/api.ts`, after `using_latest_tag`:

```ts
using_floating_tag?: boolean;
```

In `mergeOne` in `ui/src/hooks/useContainersData.ts`, next to `using_latest_tag`:

```ts
using_floating_tag: status?.using_floating_tag,
```

- [ ] **Step 2: Add the list badge**

In `Containers.tsx`, `getStatusBadge` (after the `!c.has_update_data` early return, before the `switch`):

```tsx
const allowLatest = c.labels?.['docksmith.allow-latest'] === 'true';
if (
  c.using_floating_tag &&
  !allowLatest &&
  c.update_status !== 'IGNORED' &&
  c.update_status !== 'LOCAL_IMAGE' &&
  c.update_status !== 'COMPOSE_MISMATCH' &&
  !isUpdatable(c.update_status || '')
) {
  return (
    <span className="status-badge floating" title={`Floating tag ${c.current_tag || ''} — no exact version tag could be resolved`}>
      FLOATING
    </span>
  );
}
```

The `LOCAL_IMAGE` and `COMPOSE_MISMATCH` exclusions matter because this early return sits before the `switch`: without them it would shadow those existing badges.

`UP_TO_DATE_PINNABLE` keeps the existing PIN badge because it is updatable.

- [ ] **Step 3: Add the inert-pin warning to the list row**

Next to the existing pin icon (the `versionPin && ...` span):

```tsx
{versionPin && c.using_floating_tag && (
  <span className="label-icon pin-warning" title="Version pin has no effect while running a floating tag — migrate to a version tag first">
    <i className="fa-solid fa-triangle-exclamation"></i>
  </span>
)}
```

- [ ] **Step 4: Add the detail-page badge**

In `ContainerPage.tsx`, `getStatusBadge`, before the `switch`:

```tsx
const allowLatest = docksmithData.labels?.['docksmith.allow-latest'] === 'true';
if (
  docksmithData.using_floating_tag &&
  !allowLatest &&
  docksmithData.status !== 'IGNORED' &&
  !isUpdatable(docksmithData.status) &&
  docksmithData.status !== 'COMPOSE_MISMATCH' &&
  docksmithData.status !== 'LOCAL_IMAGE'
) {
  return <span className="docksmith-badge floating" title="Floating tag — no exact version tag could be resolved">Floating Tag</span>;
}
```

Import `isUpdatable` from `../utils/status` if not already imported.

- [ ] **Step 5: Add the inert-pin notice on the detail page**

Directly after the "Pin to Version" segmented control block (the `setting-item segmented-row` containing the `none|patch|minor|major` buttons):

```tsx
{versionPin !== 'none' && docksmithData?.using_floating_tag && (
  <div className="pin-inert-warning">
    <i className="fa-solid fa-triangle-exclamation"></i>
    <span>This version pin has no effect while the image uses a floating tag. Migrate to a version tag first.</span>
  </div>
)}
```

- [ ] **Step 6: Add the styles**

`ui/src/styles/containers.css`:

```css
.containers-page .status-badge.floating {
  background: rgba(245, 158, 11, 0.15);
  color: #f59e0b;
}
.label-icon.pin-warning { color: var(--color-warning); }
```

`ui/src/styles/container-page.css`:

```css
.container-page .docksmith-badge.floating {
  background: rgba(245, 158, 11, 0.15);
  color: #f59e0b;
}
.container-page .pin-inert-warning {
  display: flex;
  gap: 8px;
  align-items: center;
  padding: 8px 12px;
  border-radius: 6px;
  background: rgba(245, 158, 11, 0.12);
  color: #f59e0b;
  font-size: 0.85rem;
}
```

Use the project's existing warning token if one exists; otherwise these values.

- [ ] **Step 7: Verify the UI builds and lints**

Run: `cd ui && npm run build && npm run lint`
Expected: both succeed.

- [ ] **Step 8: Commit**

```bash
git add ui/src/types/api.ts ui/src/hooks/useContainersData.ts ui/src/components/Containers.tsx ui/src/pages/ContainerPage.tsx ui/src/styles/containers.css ui/src/styles/container-page.css
git commit -m "feat(ui): flag floating tags and inert version pins"
```

---

### Task 6: Separate updates and pinnable counts in the shell

**Files:**
- Modify: `ui/src/utils/status.ts`
- Modify: `ui/src/App.tsx:103-129,253`
- Modify: `ui/src/components/TabBar.tsx`
- Modify: `ui/src/styles/*.css` (tab badge styling)

**Interfaces:**
- Produces: `isVersionUpdate(status)` and `isPinnable(status)`; `TabBar` gains a `pinnableCount?: number` prop and renders two badges.

- [ ] **Step 1: Add the split helpers**

In `ui/src/utils/status.ts`:

```ts
export function isVersionUpdate(status: string): boolean {
  return status === 'UPDATE_AVAILABLE' || status === 'UPDATE_AVAILABLE_BLOCKED';
}

export function isPinnable(status: string): boolean {
  return status === 'UP_TO_DATE_PINNABLE';
}

export function isUpdatable(status: string): boolean {
  return isVersionUpdate(status) || isPinnable(status);
}
```

- [ ] **Step 2: Compute both counts in App**

Replace the `pinnableCount`/`count` block in `fetchUpdateCount` with:

```ts
const containers = result.data.containers;
let updates = containers.filter(c => isVersionUpdate(c.status)).length;
let pinnable = containers.filter(c => isPinnable(c.status)).length;

try {
  const saved = localStorage.getItem('containers_view_settings');
  const viewSettings = saved ? JSON.parse(saved) : null;
  if (viewSettings && !viewSettings.showStandalone) {
    const standalone = result.data.standalone_containers || [];
    updates -= standalone.filter(c => isVersionUpdate(c.status)).length;
    pinnable -= standalone.filter(c => isPinnable(c.status)).length;
  }
} catch { /* ignore parse errors */ }

setUpdateCount(Math.max(0, updates));
setPinnableCount(Math.max(0, pinnable));
```

Add `const [pinnableCount, setPinnableCount] = useState(0);` next to `updateCount`, and pass `pinnableCount={pinnableCount}` to `TabBar`.

- [ ] **Step 3: Render two badges in TabBar**

Add `pinnableCount?: number` to `TabBarProps` (default 0), and render both in the icon container:

```tsx
{badgeUpdate !== undefined && (
  <span className="tab-badge update" title={`${badgeUpdate} update${badgeUpdate === 1 ? '' : 's'} available`}>
    {badgeUpdate > 99 ? '99+' : badgeUpdate}
  </span>
)}
{badgePinnable !== undefined && (
  <span className="tab-badge pinnable" title={`${badgePinnable} container${badgePinnable === 1 ? '' : 's'} on a floating tag`}>
    {badgePinnable > 99 ? '99+' : badgePinnable}
  </span>
)}
```

Set `badgeUpdate = updateCount > 0 ? updateCount : undefined` and `badgePinnable = pinnableCount > 0 ? pinnableCount : undefined`. Include both counts in the button's `aria-label`.

- [ ] **Step 4: Style the two badges**

Update the tab badge CSS so the badges sit side by side in the icon container (e.g. a flex row, small gap), with `.tab-badge.update` keeping the existing color and `.tab-badge.pinnable` using the pinnable purple (`--color-badge-pinnable`).

- [ ] **Step 5: Verify the UI builds and lints**

Run: `cd ui && npm run build && npm run lint`
Expected: both succeed.

- [ ] **Step 6: Commit**

```bash
git add ui/src/utils/status.ts ui/src/App.tsx ui/src/components/TabBar.tsx ui/src/styles
git commit -m "feat(ui): show update and pinnable counts separately"
```

---

### Task 7: Separate filters and selection summary

**Files:**
- Modify: `ui/src/components/Containers.tsx` — `ContainerFilter`, `DEFAULT_VIEW_SETTINGS`, `filterContainer`, toolbar, empty state, `selectedAnalysis`
- Modify: `ui/src/styles/containers.css` (empty state / floating badge in selection)

**Interfaces:**
- Consumes: `isVersionUpdate`, `isPinnable` (Task 6); `using_floating_tag` (Task 3).
- Produces: three-way filter (`all`/`updates`/`pinnable`), separate counts in the toolbar, and a `floatingNoTargetCount` in `selectedAnalysis`.

- [ ] **Step 1: Add the filter value**

```ts
type ContainerFilter = 'all' | 'updates' | 'pinnable';
```

Keep the default `filter: 'updates'`; persisted settings from older versions remain valid.

- [ ] **Step 2: Split the filter predicate**

In `filterContainer`:

```ts
if (viewSettings.filter === 'updates') {
  if (!c.has_update_data) return false;
  if (isVersionUpdate(c.update_status || '')) return true;
  if (viewSettings.showMismatch && isMismatch(c.update_status || '')) return true;
  return false;
}
if (viewSettings.filter === 'pinnable') {
  return c.has_update_data && isPinnable(c.update_status || '');
}
```

- [ ] **Step 3: Add the filter button and counts**

In the filter toolbar, compute `updatesCount` as the number of version updates only
(`isVersionUpdate`), matching the sidebar; the filter may also show mismatches when
`showMismatch` is on, and the button tooltip says so. Compute `pinnableCount` as `isPinnable`.
Render:

```tsx
<button className={viewSettings.filter === 'all' ? 'active' : ''} onClick={() => updateViewSettings({ filter: 'all' })}>All</button>
<button className={viewSettings.filter === 'updates' ? 'active' : ''} onClick={() => updateViewSettings({ filter: 'updates' })}>Updates ({updatesCount})</button>
<button className={viewSettings.filter === 'pinnable' ? 'active' : ''} onClick={() => updateViewSettings({ filter: 'pinnable' })}>Pinnable ({pinnableCount})</button>
```

- [ ] **Step 4: Add the pinnable empty state and clarify the updates one**

Where the `updates` empty state returns "All containers are up to date", change the heading to
"No version updates available"; when `pinnableCount > 0`, add a line "N container(s) are on
floating tags — see Pinnable." Add a `pinnable` branch: heading "No floating tags", subtext
"Every container is pinned to a version tag."

- [ ] **Step 5: Split the selection analysis**

In `selectedAnalysis`, compute `versionUpdates` with `isVersionUpdate` and `pinnable` with `isPinnable`; derive patch/minor/major/blocked/rebuild counts from `versionUpdates` only. Keep `hasUpdates` true when either group is non-empty. Keep `allActionablePinnable` correct by using the union: `versionUpdates.length + pinnable.length > 0 && pinnable.length === versionUpdates.length + pinnable.length && mismatches.length === 0`. Add:

```ts
const floatingNoTargetCount = selected.filter(c =>
  c.using_floating_tag &&
  !isUpdatable(c.update_status || '') &&
  c.update_status !== 'IGNORED' &&
  c.labels?.['docksmith.allow-latest'] !== 'true'
).length;
```

- [ ] **Step 6: Show the no-target count**

In the selection summary badges, next to the Pinnable badge:

```tsx
{selectedAnalysis.floatingNoTargetCount > 0 && (
  <span className="update-type-badge floating">
    {selectedAnalysis.floatingNoTargetCount} Floating · no target
  </span>
)}
```

- [ ] **Step 7: Verify the UI builds and lints**

Run: `cd ui && npm run build && npm run lint`
Expected: both succeed.

- [ ] **Step 8: Commit**

```bash
git add ui/src/components/Containers.tsx ui/src/styles/containers.css
git commit -m "feat(ui): filter pinnable containers separately"
```
