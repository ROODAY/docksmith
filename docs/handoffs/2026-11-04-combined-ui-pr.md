# Handoff: the combined UI PR (2026-11-04)

State after the upstreaming waves completed. Read this first in a new session; it
supersedes `docs/handoffs/2026-11-03-remaining-upstream-prs.md`.

## Status

Every backend PR is merged upstream (`chrisae9/docksmith`):

| PR | What |
|---|---|
| #1 | `BuildChange` version classification |
| #3 | Notifications (engine, storage, API, UI, docs) |
| #5 | network_mode + ingress ordering (`orderUpdateBatch`) |
| #6 | Self-targeted restarts via a helper container, incl. bind-mount mirroring and a `docker compose config --quiet` preflight |
| #7 | Per-file compose label locking |
| #8 | Response deadlines, compact JSON, gzip |
| #9 | Explorer cache + opt-in volume sizes |
| #10 | `lscr.io` as a GHCR alias |
| #11 | Floating-tag detection backend |
| #12 | Slim status payload + trigger dedupe backend and UI consumption |

Fork `main` was re-synced on 2026-11-04:

- `main` = `upstream/main` (`4bd1318`, tag `v0.9.0`) + **37 fork-only commits**, 0 behind.
- The sync reset `main` to `upstream/main` and replayed only the fork-only commits
  (see "Fork-only commits" below). `git cherry`/patch-id is **not** reliable here:
  upstream merged our PRs from re-based branches, so patch-equivalence misses them.
- Backup of the pre-sync fork: `backup/main-pre-sync-2026-11-04`
  (older: `backup/main-pre-sync`).
- Merged `pr/04-…` through `pr/11-…` branches were deleted locally and on `origin`.

**The only remaining upstream work is the combined UI PR.**

## The combined UI PR

Branch it from `upstream/main`, never from fork `main`. Suggested name:
`pr/12-ui-refresh`.

Replay these commits in this exact order (authoring order; do not reorder):

- **Tooltips:** `f684393` `9558c54` `173f2cd` `cb59512`
- **Resilience:** `21ace35` `ea2848b` `0d45a55` `4f0b7bb`
- **Pin UX:** `daa7012` `f517e30`
- **Floating UI:** `b722180` `5ab8fe5` `b7d1ffc` `48326ea`
- **Status UI:** `9875704` `a6ccac5` `4b0aed9` `c98d663` `0877c21`

Plus the **UI hunks** of `8a34bfb` (`Containers.tsx`, `ContainerPage.tsx`,
`types/api.ts`). That commit is mixed: its backend guard test is already upstream
via PR #11, and its planning/doc hunks must be dropped.

~1,400+ lines across `ui/src/components/Containers.tsx`, `ContainerPage.tsx`,
`useContainersData.ts`, `App.tsx`, `TabBar.tsx`, `api/client.ts`, `types/api.ts`,
and styles.

### Conflicts you will hit (and how they were resolved in the fork sync)

Upstream now contains work that overlaps the status-UI commits, because PR #12
shipped the dedupe consumption (`ui/src/hooks/refreshTrigger.ts`,
`planRefresh`/`checkFinished`, `force=true` callers) before the fork's own
versions landed. During the 2026-11-04 sync these resolutions were applied on
fork `main`:

- `21ace35` (last-good snapshot) vs upstream `useContainersData.ts`
  (`includeVolumeSizes`, coalescing): kept both — snapshot state initializers and
  `setSnapshot` calls, with `getExplorerData({ includeVolumeSizes })`.
- `9875704` (dedupe status requests + trigger types): upstream already defines
  `TriggerCheckResponse` in `types/api.ts`; the local `TriggerCheckData` was
  dropped and `triggerCheck` returns `APIResponse<TriggerCheckResponse>`.
  The `statusInFlight` single-flight map in `client.ts` is new and worth keeping.
- `a6ccac5`, `4b0aed9`, `0877c21` (trigger dedupe / refreshing / baseline poll):
  upstream's `planRefresh` + `checkFinished` supersede their `runRefresh` hunks;
  those were resolved toward upstream, keeping only the unique additions
  (`refreshing` state, `lastBackgroundRun`, skeleton states, Settings refresh,
  65 s tick). The `baselineRun` local becomes unused and must be deleted.

The simplest safe starting point: derive the branch from fork `main`'s UI state
(`git diff upstream/main main -- ui/`) rather than replaying blindly, or replay
the commits above and reuse the fork-sync resolutions. Either way, review the
diff for duplicated polling logic before opening the PR.

## Fork-only commits to keep in the fork, never upstream

- Notification design/plan docs: `c5e3720`, `762f4d6`, `ad5a4c8`
- Plans/specs/handoffs: `88482bd`, `e7788c3`, `82f4c01`, `7e619d7`, `f5351c2`,
  `fe23291`, `ec00f77`, `1f8fef7`, `831b7a6`, `4e8c7c5`,
  the 2026-11-03 handoff (now `039747e`), and this document
- Fork image refs / CI: `00eb67b`, `d481cc4`, `dfcf527`
- All 20 UI commits listed above (until the combined UI PR merges)

Never upstream `docs/superpowers/`, `docs/handoffs/`, `ghcr.io/rooday/...` image
references, or `.github/workflows/` changes from the commits above.

## Rules (learned the hard way)

- **Build every PR branch from `upstream/main`**, never from fork `main`:
  cross-fork PRs cannot be based on another branch in `ROODAY/docksmith`.
- **Do not reorder commits when squashing.** Squash contiguous ranges only.
- **Squash to final state.** Reviewers should see the end result, not
  create-then-drop churn. Earlier PRs kept their commits because upstream merges
  preserve history; follow the existing PR bodies' level of detail.
- **Verification before opening a PR:**
  ```bash
  go build ./...
  npm --prefix ui run build
  npm --prefix ui test          # vitest (added by PR #12)
  # Full Go suite: run in WSL Docker (Windows has no Docker daemon)
  wsl.exe -d Ubuntu -u rooday -- bash -lc 'cd /mnt/c/Users/Rudhra/Code/docksmith && \
    docker run --rm -v "$PWD":/src -w /src -v /var/run/docker.sock:/var/run/docker.sock \
    -e GOTOOLCHAIN=auto golang:1.25 go test ./... -skip \
    "^(TestCheckPermissions_FailsWithoutAccess|TestIntegration_PermissionFailurePreventsPartialOperations|TestLabelOperationOnSelfContainerDefersRestartToHelper)$"'
  ```
  - `TestHandleOperations_WithData` fails on upstream `main` too — pre-existing.
  - The two chmod tests fail as root (container).
  - `TestLabelOperationOnSelfContainerDefersRestartToHelper` panics inside any
    Docker container; it passes when run from the WSL host.
  - `gofmt -l` flags every file on Windows due to CRLF; ignore it.
- **Selfupdate Docker fixtures:** `internal/selfupdate/recreate_docker_test.go`
  runs the real `docker compose config --quiet` preflight and a detached helper
  recreate. They skip without a Docker daemon and need the test process to share
  the daemon's filesystem. A local Go toolchain lives at `~/go-toolchain/go` in
  WSL, which can run them directly:
  ```bash
  wsl.exe -d Ubuntu -u rooday -- bash -lc 'cd /mnt/c/Users/Rudhra/Code/docksmith && \
    ~/go-toolchain/go/bin/go test -race ./internal/selfupdate'
  ```
- PR body ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.
- Create PRs with:
  ```bash
  gh pr create --repo chrisae9/docksmith --base main --head ROODAY:pr/NN-<theme> \
    --title "..." --body "..."
  ```
  `gh pr create` is blocked by the auto-mode classifier for Claude sessions; ask
  the user to approve or to run the prepared command.

## Resyncing the fork after the UI PR merges

`git cherry` and patch-id are unreliable here. Do it explicitly:

1. `git fetch upstream && git switch main && git branch -f backup/main-pre-sync-<date> main`
2. `git reset --hard upstream/main`
3. Cherry-pick, in chronological order, the fork-only commits that are **not**
   upstream: the docs/handoffs/specs commits, the image/CI commits, and any UI
   commits the merged PR did not carry. Confirm the UI diff against
   `upstream/main` is empty afterwards (`git diff upstream/main main -- ui/` should
   show only intentional leftovers — normally none).
4. `go build`, `npm --prefix ui run build`, `npm --prefix ui test`, WSL Go suite.
5. `git push --force-with-lease origin main`.
6. Delete merged `pr/*` branches locally and on `origin`, and prune.

## Useful facts

- Upstream: `chrisae9/docksmith`; fork: `ROODAY/docksmith`.
- Remotes: `origin` = fork, `upstream` = chrisae9.
- `gh auth` account: `ROODAY`, `repo` scope; pushes use SSH.
- Upstream is on `go 1.26.10`; notification migrations are collapsed into one.
- Fork `main` also carries `backup/main-pre-sync` and
  `backup/main-pre-sync-2026-11-04` locally; delete them once confident.
