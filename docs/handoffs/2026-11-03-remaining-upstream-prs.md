# Handoff: remaining upstream PRs (2026-11-03)

State for splitting the fork into reviewable PRs against `chrisae9/docksmith`.
Read this first in a new session; it supersedes earlier planning notes.

## Status

Merged upstream:

| PR | What | Merge |
|---|---|---|
| #1 | `BuildChange` version classification (+ test mocks) | merge commit `8481e0b` |
| #3 | Notifications (labels, storage, engine, API, UI, docs) | merge commit `0370268` |

Fork sync:

- `main` = `upstream/main` + **60 fork-only commits**, 0 behind.
- Last sync `2026-11-03`: rebased `main` onto `upstream/main`, dropping the 34
  notification commits (already upstream as the squashed PR #3 commits) and
  keeping fork-only docs/image changes. Force-pushed to `origin/main`.
- Backup of the pre-sync fork: local branch `backup/main-pre-sync`.
- The old PR branches `pr/01-build-change` and `pr/02-notifications` are merged;
  delete them when convenient.

Remaining work is in three waves:

1. **Wave 1** — five independent fix PRs, can be opened in parallel.
2. **Wave 3** — three backend PRs in parallel, then one combined UI PR.
3. Nothing else after that; the fork's remaining unique work is upstreamed.

## Rules (learned the hard way)

- **Build every PR branch from `upstream/main`**, never from fork `main`:
  cross-fork PRs cannot be based on another branch in `ROODAY/docksmith`.
- **Do not reorder commits when squashing.** Reordering UI commits ahead of their
  fixes caused cascading conflicts. Squash contiguous ranges only.
- **Squash to final state.** Do not replay create-then-drop churn (migrations,
  fix-up commits). Reviewers should see the end result.
- **Exclude these from every PR:**
  - `docs/superpowers/` and `docs/handoffs/` (planning docs).
  - Fork image/CI changes: `ghcr.io/rooday/...`, `.github/workflows/` from
    commit `00eb67b`, `dfcf527`, and `d481cc4`.
  - The fork-only doc edits in `00eb67b` (notification docs are already upstream;
    keep only if re-based cleanly, but the rooday image refs must go).
- **Verification before opening a PR:**
  ```bash
  go build ./...
  npm --prefix ui run build
  # Full Go suite: run in WSL Docker (Windows has no Docker daemon)
  wsl.exe -d Ubuntu -u rooday -- bash -lc 'cd /mnt/c/Users/Rudhra/Code/docksmith && \
    docker run --rm -v "$PWD":/src -w /src -v /var/run/docker.sock:/var/run/docker.sock \
    -e GOTOOLCHAIN=auto golang:1.25 go test ./... -skip \
    "^(TestCheckPermissions_FailsWithoutAccess|TestIntegration_PermissionFailurePreventsPartialOperations|TestLabelOperationOnSelfContainerDefersRestartToHelper)$"'
  ```
  - The two chmod tests fail as root (container).
  - `TestLabelOperationOnSelfContainerDefersRestartToHelper` panics inside any
    Docker container because `/.dockerenv` exists and the test passes a nil
    Docker client to `NewPathTranslator`. Fails identically on the old fork.
  - `TestHandleOperations_WithData` fails on upstream `main` too — pre-existing.
  - `gofmt -l` flags every file on Windows due to CRLF; ignore it.
- PR body ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.
- Create PRs with:
  ```bash
  gh pr create --repo chrisae9/docksmith --base main --head ROODAY:pr/NN-<theme> \
    --title "..." --body "..."
  ```

## Wave 1 — five fix PRs (open in parallel)

All are independent of each other semantically. Commit hashes below are on the
current fork `main` (after the 2026-11-03 rebase).

### 1. `pr/04-update-ordering` — network_mode + ingress ordering (combine these)

Commits: `aef1882` `286d4bc` `98c9d39` `5908226` `4450acb` `0269c74` `4b8aeae` `68d2452`
(~980 lines)

Combines two themes that cannot be split: `286d4bc` edits
`internal/docker/ingress.go`, which only exists after `aef1882`; `4b8aeae` calls
`orderIngressLast` added by `5908226` and uses the identity from `286d4bc`.

Covers: ingress classification/warnings, Docker-API network identity, stale
`network_mode` provider tolerance, retry/wait around provider restarts,
recreate ingress last, ordering dependents after providers, UI copy.

### 2. `pr/05-self-restart` — restart docksmith via a helper container

Commit: `aa306f7` (~516 lines)

Self-targeted label operations park as `pending_restart` and hand off to a
helper container. Touches `cmd/docksmith/api.go`, `internal/api/*`,
`internal/selfupdate/*`, `internal/update/update_orchestrator.go`.

### 3. `pr/06-compose-lock` — serialize compose label writes per file

Commit: `335a3c3` (~170 lines)

Adds `compose.NewKeyedMutex` and locks read-modify-write cycles in
`handlers_labels.go`. Conflicts with #2 in `executeLabelOperationAsync` and the
`Server` struct — no semantic dependency, resolve manually.

### 4. `pr/07-api-response` — deadlines and response encoding

Commits: `733fd48` `ebb4de0` `37eb25d` `652a65e` `7f00c36` (~390 lines)

Server write deadlines, UI request timeouts, coalesced background refreshes,
compact JSON with `Content-Length`, gzip for JSON/static text.

**The status-backend PR depends on `652a65e`** (compact JSON). Merge this first
or fold `652a65e` into the status PR.

### 5. `pr/08-explorer-perf` — explorer cache and opt-in volume sizes

Commits: `13f9c5b` `9876d97` (~237 lines)

3s single-flight explorer cache; volume sizes become opt-in. Touches
`server.go`, `docker/service.go`, `client.ts`, `useContainersData.ts`.

### Wave-1 conflict map (from hunk analysis)

Hunk-disjoint (clean rebase): most pairs.
Small overlaps:

- #1 ↔ #2: `update_orchestrator.go` around `executeSelfRestart`.
- #2 ↔ #3: `handlers_labels.go` 323–414; `server.go` struct/constructor.
- #2 ↔ #5 and #3 ↔ #5: `server.go` struct/constructor.
- #4 ↔ #5: `client.ts` ~299–310 and `useContainersData.ts` 112–330.

If multiple merge in arbitrary order, expect a handful of small conflict
resolutions in `server.go` and `client.ts`.

## Wave 3 — three backend PRs, then one UI PR

Open the backends alongside or after wave 1. They are file-disjoint from each
other.

### 6. `pr/09-lscr-alias` — treat lscr.io as a GHCR alias

Commits: `31be020` `f9731a2` (~101 lines)
Files: `internal/registry/*`, docs. No dependencies.

### 7. `pr/10-floating-detection` — floating-tag detection (backend)

Commits: `cb374d3` `b0b4990` `7614666` plus the **backend hunks** of `8a34bfb`
(`internal/update/checker_guard_test.go` `/` `internal/update` only).
Files: `internal/update/checker.go`, `types.go`, tests. No dependencies.

### 8. `pr/11-status-backend` — slim status payload + trigger cap

Commits: `3329a43` `0d3838d` (~775 lines)
Files: `handlers_status.go`, `server.go`, `background_checker.go`,
`update/orchestrator.go`, `docs/api.md`.
Depends on `652a65e` from PR #4 (compact JSON) — open after #4 or include it.
API contract changes; mention the payload slimming explicitly in the PR body.

### 9. `pr/12-ui-refresh` — one combined UI PR

Opens after the notifications merge (done) **and** after the floating-detection
and status backends merge. Replay in this order (it is the authoring order; do
not reorder):

- Tooltips: `f684393` `9558c54` `173f2cd` `cb59512`
- Resilience: `21ace35` `ea2848b` `0d45a55` `4f0b7bb`
- Pin UX: `daa7012` `f517e30`
- Floating UI: `b722180` `5ab8fe5` `b7d1ffc` `48326ea` plus the **UI hunks** of
  `8a34bfb` (`Containers.tsx`, `ContainerPage.tsx`, `types/api.ts`, docs plan).
- Status UI: `9875704` `a6ccac5` `4b0aed9` `c98d663` `0877c21`

~1,400+ lines. Touches `Containers.tsx`, `ContainerPage.tsx`,
`useContainersData.ts`, `App.tsx`, `TabBar.tsx`, styles. Because the commits are
in authoring order, they apply without reordering conflicts.

Note: `8a34bfb` mixes backend/UI/doc hunks. Split by path:
backend → PR #7, UI → this PR, planning docs → drop.

## Fork-only commits — keep in the fork, never upstream

- `c5e3720`, `762f4d6`, `ad5a4c8` — notification design/plan docs.
- `88482bd`, `e7788c3`, `82f4c01`, `7e619d7`, `f5351c2`, `fe23291`,
  `ec00f77`, `1f8fef7`, `831b7a6`, `4e8c7c5` — plans/specs/handoffs.
- `00eb67b`, `d481cc4` — fork image refs (`ghcr.io/rooday`).
- `dfcf527` — manual image builds from `main`.

## After all PRs merge

- Re-sync `main`: `git fetch upstream && git switch main && git rebase upstream/main`
  (drop commits that merged; expect the same skip/drop approach).
- Delete merged `pr/*` branches and `backup/main-pre-sync` once confident.
- The fork keeps its own image builds and planning docs; nothing else is unique.

## Useful facts

- Upstream repo: `chrisae9/docksmith`; fork: `ROODAY/docksmith`.
- Remotes: `origin` = fork, `upstream` = chrisae9.
- `gh auth` account: `ROODAY` with `repo` scope; pushes use SSH.
- Upstream modules: Go 1.26.10, shoutrrr + robfig/cron already merged via PR #3.
- Migrations: notifications is `000024_create_notifications` (single, collapsed).
