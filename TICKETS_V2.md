# `branchvm` v0.2 (Tier 2): 3-Wave Parallel Execution Tickets (6 Tickets = 6 PRs)

- **Repo Path:** `/Users/heman10x/Downloads/claude_dev/personal_projects/branchvm`
- **Target Wedge:** The `git worktree` replacement for full-stack, frontend, and database-backed AI coding agents (Linux KVM + Apple M3/M4 via Lima `fc-lab`).
- **Execution Model:** Orchestrator dispatches 1 ticket per isolated git worktree branch = 1 PR.
- **Hard Invariants (Carried From v0.1, Never Violate):**
  1. Zero third-party dependencies in `go.mod` (Go 1.23 stdlib only).
  2. No TLS MITM fake root CAs, no `virtio-vsock`, no `firecracker-go-sdk`, no background daemon.
  3. Relative rootfs path (`"path_on_host": "rootfs.ext4"`) with `cmd.Dir` set to each clone's XFS reflink dir.
  4. `cmd/execd` cross-compiles as static Linux ELF (`CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`).
  5. Gateway binds strictly to per-branch host veth IP (`10.200.i.1:18080`), never `0.0.0.0`.
  6. Full backwards compatibility: `examples/auth-demo`, `branchvm verify` (`6/6 PASS`), `branchvm bench`, and `demo/server.py` must keep passing with zero regressions.
  7. Zero em-dashes, zero en-dashes as punctuation, and zero AI prose in `README.md`, comments, and docs.
  8. YAGNI: implement only what is specified. No speculative abstractions, no SaaS flags, no multi-tenant `jailer`, no Python/TS SDKs, no arbitrary internet egress proxy.

---

## Wave Schedule & Parallelism (Zero File Overlap Inside Wave 2)

```text
WAVE 1 (Sequential Foundation, 1 agent, ~15 to 20 min):
  PR #9  [feat/v2-t7-config-contracts]
         -> T7: pkg/types/types.go + pkg/types/types_test.go + pkg/config/ + examples/auth-demo/branchvm.toml
         (Merge to main first so all 4 Wave 2 worktrees compile against identical contracts.)

WAVE 2 (4 PARALLEL GIT WORKTREES, ZERO FILE OVERLAP, ~25 to 35 min wall time):
  PR #10 [feat/v2-t8-oci-rootfs-builder]
         -> T8: pkg/rootfs/ + examples/agent-image/Dockerfile + examples/node-demo/
  PR #11 [feat/v2-t9-execd-v2]
         -> T9: cmd/execd/ (pure-Go netlink, generic service supervisor, /pty, /tcp, delta delete sync)
  PR #12 [feat/v2-t10-delta-sync-pty-client]
         -> T10: pkg/storage/sync.go + pkg/storage/storage_test.go + pkg/ptyclient/{client,forward,term}
  PR #13 [feat/v2-t11-gateway-release]
         -> T11: pkg/gateway/proxy.go + pkg/gateway/proxy_test.go
                 + .github/workflows/release.yml + scripts/install.sh

WAVE 3 (Sequential Integration, Live fc-lab Verification & Docs, 1 agent, ~75 to 120 min):
  PR #14 [feat/v2-t12-orchestrator-cli-verify]
         -> T12: pkg/pool/{orchestrator,branch,verifier}.go
                 + cmd/branchvm/{main,cmd_init,cmd_run,cmd_apply,cmd_diff,cmd_shell,cmd_logs,cmd_forward,cmd_gc}.go
                 + Makefile + README.md
```

**Estimated Total Wall-Clock Time:** 2.5 to 4 hours total (about 1 hour of parallel agent coding + unit tests across Waves 1 to 3, plus 1.5 to 2.5 hours of live `fc-lab` Docker image builds, PTY/port-forward checks, multi-stack runs, real `claude -p` gateway verification, and edge-case fixes).

### Fast-to-Slow 3-Pass Execution Rule (Mandatory for Every Sub-Agent)

Do NOT run slow Docker builds, `fc-lab` shells, or full VM runs while still writing code. Every ticket follows this exact order:

1. **Pass 1 (Write code only):** Write all owned files and unit tests for the ticket in one batch. Zero builds, zero tests, zero Docker, and zero `fc-lab` calls mid-way.
2. **Pass 2 (Fast local gate, <5s):** Run ONE targeted command (`go vet` + `go test -race` on ONLY the ticket's owned packages; for T9 `cmd/execd`, `GOOS=linux GOARCH=arm64 go vet ./cmd/execd/... && make build-guest`). Fix all errors together, then re-run once. Never touch Docker, `fc-lab`, `branchvm init`, `branchvm verify`, or `branchvm bench` in Wave 1 or Wave 2.
3. **Pass 3 (Single ordered live `fc-lab` gate at end of Wave 3 / T12 only):** Run checks strictly from cheapest to most expensive:
   1. Full unit suite (`go vet ./... && go test -race ./...`, ~5s)
   2. Build both binaries (`make build-all`, ~5s)
   3. `auth-demo` regression (`init` -> `run` -> `logs` -> `diff` -> `shell` -> `forward` -> `apply` -> `gc --all`, ~30s)
   4. Stateless Node Docker demo (`examples/node-demo`: auto-builds `Dockerfile` -> `run --branches 3` -> delta delete test -> `shell` -> `forward` -> `gc --all`, ~60s)
   5. Security verifier once (`branchvm verify --repo examples/auth-demo` -> `6/6 PASS`, ~45s)
   6. Real agent image last (`examples/agent-image/Dockerfile` -> mock SSE gateway test at $0 + optional real `claude -p` run if host API key is set). Do NOT re-run full `branchvm bench` because snapshot restore and pager internals are unchanged.

---

## Abstract Edge-Case Audit (17 Failure Modes Caught & Baked Into Tickets)

Every sub-agent must respect these design decisions. Each one prevents a concrete failure mode caught before coding:

1. **No hardcoded `authdemo` without breaking Tier 1:** Removing `const databaseName = "authdemo"` from `pkg/pool/orchestrator.go` would break `examples/auth-demo` and `branchvm verify` P4. Fix: ship `examples/auth-demo/branchvm.toml`, validate `cfg.Database` with `^[a-zA-Z_][a-zA-Z0-9_-]{0,62}$`, only run `createdb` when `cfg.Database != ""`, and make `verifier.go` P4 adapt to whether `postgres` is enabled (testing DB isolation when Postgres is configured, and disk CoW isolation alone when stateless).
2. **CLI vs `branchvm.toml` flag precedence:** Checking `*mem != 1024` cannot distinguish an omitted flag from `--mem 1024`. Fix: use `fs.Visit` to record explicitly passed flags. Precedence is strictly: explicit CLI flag > `branchvm.toml` > `golden/meta.json` (for `run` mem) > built-in default.
3. **Zero-dep TOML parser traps:** Naive line splitting breaks on `#` inside quoted strings (e.g. URLs/passwords), multi-line arrays, single-quoted strings, and silent typos. Fix: implement a strict quote-aware lexer in `pkg/config`, track bracket depth for multi-line arrays, and reject unknown keys with line numbers.
4. **Docker architecture mismatch & wrong-arch `execd`:** Exporting an `amd64` Docker image on Apple M4 (`arm64`) `fc-lab`, or injecting a macOS Mach-O `execd`, causes silent kernel panics. Fix: check `docker image inspect --format '{{.Architecture}}'` against `runtime.GOARCH`, and validate the `execd` ELF header (`\x7fELF` + `e_machine` 62 for `amd64` or 183 for `arm64`) before writing `/sbin/init`.
5. **Official Docker images lack `iproute2` (`ip`) and lose `ENV PATH` on `docker export`:** Images like `node:22-slim`, `python:3.12-slim`, and `golang:1.23` do not ship `iproute2`, and `docker export` strips Dockerfile `ENV` (breaking `go`, `cargo`, `bun`, `node` on custom `PATH`). Fix: configure `lo`, `eth0` (`172.16.0.2/30`), and default route (`172.16.0.1`) in pure Go via `AF_NETLINK` (`NETLINK_ROUTE`, checking `NLMSG_ERROR`) inside `cmd/execd/net.go`; save `docker image inspect` `Config.Env` into `/etc/branchvm/image-env.json` and merge it in `execd`; fail fast in `pkg/rootfs` if `git` or `/bin/sh` is missing from the exported image.
6. **Host symlink escape during rootfs staging:** On merged-`/usr` distros, `staging/sbin` or `staging/etc/resolv.conf` may be an absolute symlink (e.g. `/usr/sbin` or `/run/...`). Writing through it on the host would overwrite host system files. Fix: NEVER follow symlinks out of `stagingDir`; resolve every path inside `stagingDir` using `os.Lstat` per component and unlink existing target symlinks before writing injected files.
7. **`ext4` out-of-space and inode starvation on Node/Python images:** Fixed 1.5 GB images fail on larger stacks or `node_modules` inode counts. Fix: measure staging bytes and entry count; size image to `max(1536 MiB, 1.35*bytes + 512 MiB)` (rounded to 64 MiB, overridable via `rootfs_size_mb`); pass `-N max(65536, entries*2 + 32768)` to `mkfs.ext4 -d`; preserve ownership via `tar -xpf - --numeric-owner --same-permissions`; run `e2fsck -f -y`; cache built ext4 under `/var/lib/branchvm/images/<sha256>/rootfs.ext4`; always clean temp containers/dirs using `context.Background()` in `defer`.
8. **Postgres path differences across distros and stateless repos:** Official `postgres:16`/`17` images put data in `/var/lib/postgresql/data` without running `initdb` yet, and stateless repos have no Postgres at all. Fix: generic `/etc/branchvm/services.json` supervisor in `execd` with builtin `"postgres"` (auto-discovers `/usr/lib/postgresql/*/bin` or `PATH`, auto-discovers cluster or runs `initdb` as `postgres` with local trust auth if `PG_VERSION` is missing), builtin `"redis"`, custom `[[service]]` specs, and empty `services = []` (instant ready, skips `pg_isready` on restore).
9. **PTY hijack, PID 1 reaper race, `EIO`, and live gateway during `branchvm shell`:**
   - Read post-hijack bytes from `rw.Reader` (not raw `net.Conn`).
   - Spawn PTY shell via `a.procs.start`/`wait` so the PID 1 `SIGCHLD` orphan reaper never steals its exit status (`ECHILD`).
   - Close slave PTY fd in parent right after start; treat `syscall.EIO` on master read as clean EOF; kill `-pid` process group on client disconnect.
   - Use 5-byte binary framing (`0x01` data, `0x02` resize, `0x03` exit) and Linux `asm-generic` ioctls (`TIOCGPTN=0x80045430`, `TIOCSPTLCK=0x40045431`, `TIOCSWINSZ=0x5414`, `TCGETS=0x5401`, `TCSETS=0x5402`, identical on `amd64` and `arm64`).
   - `branchvm shell` gracefully degrades when stdin is not a TTY, restores termios via `defer`, and starts a temporary per-branch host gateway on `10.200.i.1:18080` for the shell's lifetime so interactive `claude`/`codex` works inside `shell` with zero leaked gateways on exit.
10. **Stale `.patch` after `branchvm shell` edits:** `loadPatch` previously read the cached `.patch` file first. Fix: when the branch Firecracker PID is alive, fetch live `GET /diff` first (refreshing the cached `.patch`) and fall back to the file only when the VM is stopped.
11. **Dirty `init`, rebases, and deletions in delta sync + `MemMB` OOM:** Naive `git diff <golden_sha>` misses reverted dirty files from `init`, untracked deletions, mode changes, and rebases. Fix: store a fast file manifest in `golden/meta.json` (`"git:<blobSHA>:<mode>"` from `git ls-files -s -z` for clean tracked files, `"sha:<sha256>:<mode>"` for dirty/untracked files, `"link:<target>"` for symlinks). Diff `current` vs `golden` maps to compute exact changed/added files and exact deleted paths. Send via `multipart/form-data` (`meta` JSON + `archive` gzip tar) to `POST /sync` while keeping plain `application/gzip` backwards-compatible. Also persist `RepoPath`, `MemMB`, `Services`, `Database`, `Env`, and `Providers` in `golden/meta.json` so `branchvm run` refuses mismatched repos and inherits `MemMB` instead of OOM-killing larger golden VMs.
12. **Real `claude -p` and `codex exec` inside `nftables policy drop`:** Claude Code refuses `--dangerously-skip-permissions` as root unless `IS_SANDBOX=1` is set, hangs on blocked telemetry without `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, and prompts for onboarding unless `/root/.claude.json` has `{"hasCompletedOnboarding":true}`. Fix: inject `IS_SANDBOX=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_TELEMETRY=1`, `DISABLE_ERROR_REPORTING=1`, `DISABLE_AUTOUPDATER=1`, `ANTHROPIC_AUTH_TOKEN=<dummy>`, `CODEX_API_KEY=<dummy>`; seed `/root/.claude.json` during rootfs staging if missing; set `FlushInterval: -1` on `httputil.ReverseProxy` for immediate SSE token streaming; support OpenRouter and custom `[providers.*]` routes in `pkg/gateway`.
13. **Preserve git-ignored build caches (`node_modules`, `.venv`, `target`) on `full` sync:** When `meta.Mode == "full"`, wiping `/workspace` wholesale destroys pre-installed dependencies baked into the Docker image or created during `--warm`. Fix: remove ONLY previously git-tracked files (`git ls-files -z`) before extracting, leaving git-ignored caches intact.
14. **One-command `dockerfile` auto-build & missing `image` auto-pull:** Users should not have to run `docker build` or `docker pull` separately before `branchvm init`. Fix: support optional `dockerfile = "Dockerfile"` in `branchvm.toml` (mutually exclusive with `image`); in `pkg/rootfs`, run `docker build --platform linux/<arch> -t branchvm-local-<hash> -f <dockerfile> <repoDir>` when `dockerfile` is set, and auto-run `docker pull --platform linux/<arch> <image>` if `docker image inspect` reports a missing image.
15. **Frontend & DB port preview with zero firewall changes (`POST /tcp` + `branchvm forward`):** Frontend and full-stack developers run Next.js/Vite dev servers or want to connect `psql`/GUI tools to one branch without colliding on `localhost`. Fix: add `POST /tcp?port=<N>` HTTP hijack tunnel in `cmd/execd/pty.go` (dials guest `127.0.0.1:<N>` and pipes raw bytes over the existing `10.200.i.2:8080` channel, zero `nftables` changes), `Forward` in `pkg/ptyclient/forward.go` listening on host `127.0.0.1:<localPort>`, and `cmd/branchvm/cmd_forward.go` (`branchvm forward <i> <guestPort>[:<hostPort>]`).
16. **Looping & cleanup ergonomics (`branchvm logs <i>`, `branchvm gc --all`, actionable gateway errors):** Reading `/var/lib/branchvm/runs/<id>-branch-2.out` by hand or copy-pasting run IDs slows down the `run` -> inspect -> `apply` loop. Fix: add `branchvm logs <i> [--run RUNID]` (~25 LOC printing saved branch stdout/stderr), add `--all` flag to `branchvm gc` (`pool.GCAll` discards all active runs cleanly), and return descriptive `403`/`502` JSON/text errors from `pkg/gateway/proxy.go` mentioning `[providers.<name>]` in `branchvm.toml`.
17. **Clean stateless default when `branchvm.toml` exists:** If `branchvm.toml` does not exist, `DefaultConfig()` keeps Postgres enabled (100% Tier 1 backwards compat). If `branchvm.toml` exists and omits `services` and `[[service]]`, default to stateless `services = []` so a minimal frontend or Node `branchvm.toml` (`dockerfile = "Dockerfile"`) never tries to boot Postgres.

---

## Detailed Tickets

### Wave 1 (Sequential Foundation)

#### PR #9 (`feat/v2-t7-config-contracts`): T7: Shared v2 Contracts & Zero-Dep `branchvm.toml` Parser
- **Owned Files:**
  - `pkg/types/types.go` (edit)
  - `pkg/types/types_test.go` (new)
  - `pkg/config/config.go` (new)
  - `pkg/config/config_test.go` (new)
  - `examples/auth-demo/branchvm.toml` (new)
- **Task:**
  1. Extend `pkg/types/types.go` (keep all existing fields/JSON tags intact):
     - Add `ServiceSpec`: `Name string`, `Preset string` (`"postgres"`, `"redis"`, or `""`), `Cmd []string`, `User string`, `Env []string`, `ReadyTCP string`, `ReadyCmd []string`, `CheckpointCmd []string`, `ReadyTimeoutSec int`.
     - Extend `HealthResponse`: add `ServicesReady bool` (`json:"services_ready"`) and `Services map[string]bool` (`json:"services,omitempty"`). Keep `PgReady bool` populated for backwards compatibility.
     - Add `SyncMeta`: `Mode string` (`"delta"` or `"full"`), `Delete []string` (`json:"delete,omitempty"`).
     - Add `ProviderSpec`: `Name string`, `Upstream string`, `APIKeyEnv string`, `AuthHeader string` (`"X-Api-Key"` or `"Authorization"`), `BaseURLEnv string`, `KeyEnv string`.
     - Add `GoldenMeta`: `RepoPath string`, `HostSHA string`, `Image string`, `Dockerfile string`, `ImageDigest string`, `MemMB int64`, `VCPUs int`, `Database string`, `Services []ServiceSpec`, `Env []string`, `Providers []ProviderSpec`, `GoldenFiles map[string]string` (repo-relative path -> content key).
     - Add PTY framing constants & helpers in `pkg/types/types.go`:
       - `PTYFrameData byte = 1`, `PTYFrameResize byte = 2`, `PTYFrameExit byte = 3`, `PTYMaxFrameBytes = 64 << 10`.
       - `WritePTYFrame(w io.Writer, kind byte, payload []byte) error` and `ReadPTYFrame(r io.Reader) (byte, []byte, error)` (5-byte header: 1-byte type + 4-byte big-endian uint32 length).
       - `PTYRequest` JSON struct (`Cmd []string`, `Env []string`, `Dir string`, `Rows uint16`, `Cols uint16`).
  2. Add `pkg/types/types_test.go`: test PTY frame codec roundtrips and bounds checks.
  3. Implement `pkg/config/config.go` (zero external deps):
     - `Config` struct: `Image string`, `Dockerfile string`, `RootfsSizeMB int64`, `MemMB int64`, `VCPUs int`, `Warm string`, `Database string`, `Env []string`, `Services []types.ServiceSpec`, `Providers []types.ProviderSpec`.
     - `Load(repoDir string) (Config, bool, error)`: reads `<repoDir>/branchvm.toml`. If missing (`os.ErrNotExist`), returns `(DefaultConfig(), false, nil)` where `DefaultConfig()` has `MemMB: 1024, VCPUs: 2, Services: []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}}` (preserving v0.1 default behavior when no config exists).
     - When `branchvm.toml` exists, `Parse` defaults `Services` to empty `[]types.ServiceSpec{}` (stateless unless `services` or `[[service]]` is configured).
     - Supported TOML syntax:
       - Top-level keys: `image`, `dockerfile`, `rootfs_size_mb`, `mem`, `cpus`, `warm`, `database`, `env = [...]`, `services = ["postgres", "redis"]`.
       - `[[service]]` tables for custom services (`name`, `cmd`, `user`, `env`, `ready_tcp`, `ready_cmd`, `checkpoint_cmd`, `ready_timeout_sec`).
       - `[providers.<name>]` tables (`upstream`, `api_key_env`, `auth_header`, `base_url_env`, `key_env`).
     - Validation:
       - Quote-aware `#` comment stripping and multi-line `[...]` array parsing.
       - Reject unknown keys, duplicate keys, and malformed syntax with file line numbers.
       - Reject setting both `image` and `dockerfile`.
       - Validate `database` against `^[a-zA-Z_][a-zA-Z0-9_-]{0,62}$` when non-empty.
       - Validate every `env` item contains `=` with non-empty key.
       - Validate custom `[[service]]` has non-empty unique `name`, non-empty `cmd`, valid `ready_tcp` host:port (if set), and `[providers.<name>]` has valid `http(s)` `upstream`, valid `api_key_env`, and `auth_header` in `{"Authorization", "X-Api-Key"}`.
       - Reject any key named `api_key` inside `[providers.*]` with an error telling the user to pass `api_key_env` (never hardcode secrets in `branchvm.toml`).
  4. Create `examples/auth-demo/branchvm.toml`:
     ```toml
     services = ["postgres"]
     database = "authdemo"
     warm = "go test -run ^$ ./..."
     env = [
       "DATABASE_URL=postgres://postgres@127.0.0.1:5432/authdemo?sslmode=disable",
     ]
     ```
  5. Write comprehensive unit tests in `pkg/config/config_test.go`.
- **Gate:** `go vet ./... && go test -race ./pkg/types/... ./pkg/config/...` passes. Merge PR #9 to `main` before starting Wave 2.

---

### Wave 2 (Run All 4 Worktrees In Parallel, Zero File Overlap)

#### PR #10 (`feat/v2-t8-oci-rootfs-builder`): T8: OCI / Docker Image to Firecracker `rootfs.ext4` Builder & Multi-Stack Examples
- **Owned Files:**
  - `pkg/rootfs/builder.go` (new)
  - `pkg/rootfs/builder_test.go` (new)
  - `pkg/rootfs/builder_linux_test.go` (new)
  - `examples/agent-image/Dockerfile` (new)
  - `examples/node-demo/branchvm.toml` (new)
  - `examples/node-demo/Dockerfile` (new)
  - `examples/node-demo/server.js` (new)
  - `examples/node-demo/test.js` (new)
- **Task:**
  1. Implement `pkg/rootfs/builder.go`:
     - `BuildOptions`: `Image string`, `Dockerfile string`, `RepoDir string`, `ExecdPath string`, `Services []types.ServiceSpec`, `RootfsSizeMB int64`, `CacheDir string` (default `/var/lib/branchvm/images`), `Out io.Writer`.
     - `BuildResult`: `RootfsPath string`, `ImageDigest string`, `Cached bool`, `SizeMB int64`.
     - `FindExecd(explicit string) (string, error)`: checks (1) `explicit`, (2) sibling `execd` next to `os.Executable()`, (3) `/var/lib/branchvm/assets/execd`, (4) `./bin/execd`. Validates the binary is a Linux ELF matching `runtime.GOARCH` (`\x7fELF`, `e_ident[4]==2` 64-bit, `e_machine==62` for `amd64` or `183` for `arm64`, `PT_INTERP` absent so it is statically linked).
     - `BuildFromImage(ctx context.Context, opt BuildOptions) (BuildResult, error)`:
       1. Verify `docker` CLI works (`docker info`).
       2. Resolve source image:
          - If `opt.Dockerfile != ""`, resolve `dfPath := filepath.Join(opt.RepoDir, opt.Dockerfile)` cleanly inside `opt.RepoDir`, tag `branchvm-local-<sha256[:12]>` from Dockerfile path + repo path, and run `docker build --platform linux/<arch> -t <tag> -f <dfPath> <opt.RepoDir>`.
          - Else run `docker image inspect <opt.Image>`; if missing locally, auto-run `docker pull --platform linux/<arch> <opt.Image>` and re-inspect.
       3. Parse `Id`, `Architecture`, `Config.Env` from `docker image inspect`. Fail fast if `Architecture != runtime.GOARCH` with `--platform linux/<arch>` hint.
       4. Compute cache key `sha256(imageID + execdSHA256 + canonicalServicesJSON + rootfsSizeMB)`. If `<CacheDir>/<key>/rootfs.ext4` already exists, return `Cached: true` immediately.
       5. Create temp container via `docker create <image>` (with `defer docker rm -f <cid>` using `context.Background()` so `Ctrl-C` never leaks containers).
       6. Export container filesystem into `stagingDir` via `docker export <cid>` piped to `tar -xpf - --numeric-owner --same-permissions -C <stagingDir>` (defer `os.RemoveAll(stagingDir)`).
       7. Validate required guest tools inside `stagingDir` (resolving symlinks strictly inside `stagingDir`): check `/bin/sh` and `git` (search `PATH` from image env plus `/usr/bin`, `/bin`). Fail fast with actionable Dockerfile hint if missing.
       8. Safely inject guest files into `stagingDir` WITHOUT ever following a symlink outside `stagingDir` (helper `safeStagePath(staging, guestPath)` walking each component with `os.Lstat`, resolving relative/absolute symlink targets back inside `staging`, and unlinking any existing file/symlink at the leaf before writing):
          - Ensure mount dirs exist: `/proc`, `/sys`, `/dev`, `/dev/pts`, `/dev/shm`, `/run`, `/tmp` (`01777`), `/workspace` (`0755`), `/etc/branchvm` (`0755`), `/root` (`0700`).
          - Install `execd` at `/sbin/init` (`0755`, safely handling merged-`/usr` `/sbin -> usr/sbin`).
          - Write `/etc/branchvm/services.json` from `opt.Services`.
          - Write `/etc/branchvm/image-env.json` from Docker `Config.Env`.
          - Write clean `/etc/hostname` (`branchvm`), `/etc/hosts` (`127.0.0.1 localhost`, `127.0.1.1 branchvm`), empty `/etc/resolv.conf`, empty `/etc/machine-id` (`0444`), and remove `/var/lib/dbus/machine-id` and `/var/lib/systemd/random-seed`.
          - If `/root/.claude.json` does not exist in staging, write `{"hasCompletedOnboarding":true}\n` (`0600`) so headless `claude -p` never hangs on onboarding.
       9. Measure `stagingDir` total bytes and inode/entry count. Compute `sizeMB = max(1536, ceil((bytes*1.35 + 512MiB)/64MiB)*64)` (or `opt.RootfsSizeMB` if larger/explicit). Compute `inodes = max(65536, entries*2 + 32768)`.
       10. Create sparse temp file `rootfs.ext4.partial`, run `truncate -s <sizeMB>M`, `mkfs.ext4 -F -q -L branchvm-root -N <inodes> -d <stagingDir> <partial>`, and `e2fsck -f -y <partial>` (exit 0 or 1 is clean). Atomic `os.Rename` to cached `rootfs.ext4`.
  2. Write `examples/agent-image/Dockerfile`:
     - Based on `ubuntu:24.04` (multi-arch `amd64`/`arm64`). Installs `postgresql-16`, `git`, `curl`, `ca-certificates`, `dnsutils`, `golang-go`, Node.js 22 (via official binary tarball for `TARGETARCH` or NodeSource), and `@anthropic-ai/claude-code` globally (`npm install -g @anthropic-ai/claude-code`). Configures Postgres 16 local trust auth just like `scripts/build-rootfs.sh`. Clean apt caches.
  3. Write `examples/node-demo/`:
     - Minimal zero-npm-dep Node.js 22 app (`server.js` + `test.js` using built-in `node:test` and `node:assert`, plus an HTTP listener on `127.0.0.1:3000` when run directly for testing `branchvm forward`, and 1 intentional bug in `server.js` that fails 1 of 3 tests so multi-branch runs show pass/fail cleanly).
     - `Dockerfile`: `FROM node:22-slim`, `RUN apt-get update && apt-get install -y --no-install-recommends git curl ca-certificates && rm -rf /var/lib/apt/lists/*` (intentionally NO `iproute2` and NO Postgres, proving both pure-Go netlink in `execd` and stateless `services = []`!).
     - `branchvm.toml`: `dockerfile = "Dockerfile"`, `env = ["NODE_ENV=test"]` (omitting `services` to prove stateless default #17 and auto-build #14!).
  4. Write unit tests in `pkg/rootfs/builder_test.go` (ELF validation, `safeStagePath` symlink escape defense including `/sbin -> /usr/sbin` and `/etc/resolv.conf -> /run/systemd/stub`, size/inode math, cache key stability, dockerfile path containment) and Linux integration test in `pkg/rootfs/builder_linux_test.go` (skipped if `mkfs.ext4` or `docker` unavailable).
- **Gate:** `go vet ./pkg/rootfs/... && go test -race ./pkg/rootfs/...` passes.

#### PR #11 (`feat/v2-t9-execd-v2`): T9: Guest `execd` v2 (Pure-Go Netlink, Service Supervisor, `/pty`, `/tcp`, Delta Delete Sync)
- **Owned Files:**
  - `cmd/execd/main.go` (edit)
  - `cmd/execd/net.go` (new)
  - `cmd/execd/services.go` (new)
  - `cmd/execd/postgres.go` (edit)
  - `cmd/execd/handlers.go` (edit)
  - `cmd/execd/pty.go` (new)
  - `cmd/execd/execd_test.go` (new)
- **Task:**
  1. Pure-Go Netlink networking (`cmd/execd/net.go`):
     - Replace `exec.Command("ip", ...)` in `setupNetwork()` with pure-Go `AF_NETLINK` (`NETLINK_ROUTE`) calls using Go stdlib `syscall`:
       - Bring `lo` UP (`RTM_NEWLINK`, `IFF_UP`).
       - Bring `eth0` UP (`RTM_NEWLINK`, `IFF_UP`).
       - Assign `172.16.0.2/30` to `eth0` (`RTM_NEWADDR`, `NLM_F_CREATE|NLM_F_REPLACE`).
       - Add default route `0.0.0.0/0` via `172.16.0.1` on `eth0` (`RTM_NEWROUTE`, `NLM_F_CREATE|NLM_F_REPLACE`).
     - Read and validate `NLMSG_ERROR` ack for every message (treat `0` and `EEXIST` as success; return explicit error otherwise).
     - Works identically on `linux/amd64` and `linux/arm64` with zero `iproute2` binary required!
  2. Image environment merging (`cmd/execd/main.go`):
     - On startup, load `/etc/branchvm/image-env.json` (if present) as a `[]string` of `KEY=VAL`.
     - Add `mergeEnv(layers ...[]string) []string`: merges `baseEnv()`, image env, and per-request env (last key wins, ensuring standard system dirs remain in `PATH`). Use it in `runExec`, `pty`, and service launches.
  3. Generic service supervisor (`cmd/execd/services.go` + `cmd/execd/postgres.go`):
     - On boot, load `/etc/branchvm/services.json`.
     - If the file is missing (legacy Tier 1 rootfs), default to `[]types.ServiceSpec{{Name: "postgres", Preset: "postgres"}}`.
     - If the file exists and contains `[]` (empty array), start zero services and immediately mark `servicesReady = true`, `pgReady = true` (so legacy health checks succeed).
     - Builtin preset `"postgres"` (refactored from `postgres.go`):
       - Auto-discover Postgres binaries: glob `/usr/lib/postgresql/*/bin/postgres` (highest version) or `exec.LookPath("postgres")`.
       - Auto-discover data dir: existing `/var/lib/postgresql/<ver>/main` (+ `/etc/postgresql/<ver>/main/postgresql.conf`), else `$PGDATA` or `/var/lib/postgresql/data`.
       - If `<dataDir>/PG_VERSION` is missing, create dir owned by `postgres:postgres` (`0700`), run `initdb -D <dataDir> --auth=trust --encoding=UTF8` as user `postgres`, and configure `listen_addresses='127.0.0.1'`, `port=5432`, `ssl=off`.
       - Drop UID/GID to `postgres`, start `postgres`, track via `a.procs`, wait for `pg_isready`, and run `CHECKPOINT` + terminate idle backends during `POST /checkpoint`.
     - Builtin preset `"redis"`: starts `redis-server --bind 127.0.0.1 --port 6379 --save "" --appendonly no`, waits for TCP `127.0.0.1:6379`.
     - Custom `ServiceSpec`: drops UID/GID if `User != ""`, launches `Cmd` tracked via `a.procs`, waits for `ReadyTCP` (TCP dial loop) or `ReadyCmd` (exit 0 loop), runs `CheckpointCmd` (if set) during `POST /checkpoint`.
     - Update `GET /health` to return `OK`, `ServicesReady`, `Services` map, and `PgReady` (true when postgres is ready or not configured).
     - Update `POST /checkpoint`: run checkpoint hooks for all configured services (skipping Postgres if not configured), remove identity files (`/etc/machine-id`, `/var/lib/systemd/random-seed`), call `syscall.Sync()`.
  4. Delta sync + deletions in `POST /sync` (`cmd/execd/handlers.go`):
     - Keep `Content-Type: application/gzip` working identically (legacy full extract).
     - When `Content-Type` starts with `multipart/form-data`:
       - Parse `"meta"` part into `types.SyncMeta` and `"archive"` part as gzip tar stream.
       - If `meta.Mode == "full"`, remove ONLY previously git-tracked files listed by `git ls-files -z` (when `.git` exists) before extracting, NEVER wiping git-ignored caches (`node_modules`, `.venv`, `target`)!
       - For every `rel` in `meta.Delete`: validate via `safeJoin(workspaceDir, rel)`, reject `""`, `"."`, `".git"`, anything under `".git/"`, and `".bvm_base_sha"`. Run `os.RemoveAll(target)` and prune newly empty parent dirs up to (never including) `workspaceDir`.
       - Extract `"archive"` (if present/non-empty) using `extractTarGz`. Before extracting a regular file or symlink, if `target` is a directory, remove it cleanly; if a parent path is a file, remove it cleanly.
       - Run `commitBase()` and return `syncResponse{OK: true, BaseSHA: sha}`.
  5. Interactive PTY endpoint & TCP tunnel (`cmd/execd/pty.go`):
     - Route `POST /pty`:
       - Decode `types.PTYRequest` JSON body (cap 64 KiB; defaults: `Cmd: ["/bin/bash"]` falling back to `["/bin/sh"]` if `/bin/bash` absent, `Dir: "/workspace"`, `Rows: 24`, `Cols: 80`).
       - Allocate Linux PTY master via `os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)`.
       - Clear lock on slave (`TIOCSPTLCK = 0x40045431`, arg `int32(0)`) and query slave number (`TIOCGPTN = 0x80045430`, arg `uint32`), open `/dev/pts/<n>` (`O_RDWR|O_NOCTTY`).
       - Set initial window size (`TIOCSWINSZ = 0x5414`).
       - Start command attached to slave fd (`Stdin/Stdout/Stderr = slave`, `SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}`, `Env = mergeEnv(...)`) using `a.procs.start(cmd)` so PID 1 reaper does not race `cmd.Wait()`!
       - Immediately close `slave` in parent!
       - Hijack connection (`w.(http.Hijacker).Hijack()`), write `"HTTP/1.1 101 Switching Protocols\r\nUpgrade: bvm-pty\r\nConnection: Upgrade\r\n\r\n"`, flush `rw.Writer`.
       - Bridge bidirectionally using `types.ReadPTYFrame(rw.Reader)` / `types.WritePTYFrame(conn, ...)`:
         - Client -> master: `PTYFrameData` writes to master; `PTYFrameResize` calls `TIOCSWINSZ`.
         - Master -> client: reads chunks up to 32 KiB and writes `PTYFrameData`; treat `syscall.EIO` as normal EOF.
         - On client EOF/disconnect before child exits: send `SIGHUP` then `SIGKILL` to `-cmd.Process.Pid` so no orphan shell remains.
         - Wait via `a.procs.wait(cmd)`, extract exit code, send `PTYFrameExit` (`int32` big-endian), close master and `conn`.
     - Route `POST /tcp`:
       - Parse query `port` (must be integer `1..65535`).
       - Dial `net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)` inside the guest; if dial fails, return `502 Bad Gateway` before hijacking.
       - Hijack connection (`w.(http.Hijacker).Hijack()`), write `"HTTP/1.1 101 Switching Protocols\r\nUpgrade: bvm-tcp\r\nConnection: Upgrade\r\n\r\n"`, flush `rw.Writer`.
       - `io.Copy` bidirectionally between `rw.Reader`/`conn` and the guest TCP socket, propagating `CloseWrite()` when one side finishes.
  6. Unit tests in `cmd/execd/execd_test.go`: `mergeEnv`, `safeJoin` & delete path sanitization (blocks `.git`, `../`, `.bvm_base_sha`), `services.json` parsing/fallback, multipart delta sync extraction + deletion + empty-dir pruning + `full` mode preserving git-ignored `node_modules`.
- **Gate:** `make build-guest` produces a static Linux ELF AND `GOOS=linux GOARCH=arm64 go vet ./cmd/execd/...` passes.

#### PR #12 (`feat/v2-t10-delta-sync-pty-client`): T10: Host Manifest Delta Sync, Pure-Go PTY Client & TCP Port Forwarder
- **Owned Files:**
  - `pkg/storage/sync.go` (edit)
  - `pkg/storage/storage_test.go` (edit)
  - `pkg/ptyclient/client.go` (new)
  - `pkg/ptyclient/term_linux.go` (new)
  - `pkg/ptyclient/term_other.go` (new)
  - `pkg/ptyclient/forward.go` (new)
  - `pkg/ptyclient/client_test.go` (new)
- **Task:**
  1. Manifest-based delta sync in `pkg/storage/sync.go` (keep `DirtyFiles`, `PackWorkspace`, `PackFiles`, `ApplyPatch` intact and backwards-compatible):
     - Add `WorkspaceManifest(repoDir string) (hostSHA string, files map[string]string, err error)`:
       - `hostSHA`: `git rev-parse HEAD` (empty string if unborn HEAD).
       - Read tracked index entries via `git ls-files -s -z` (`<mode> <blobSHA> <stage>\t<path>\0`) -> seed `files[path] = "git:" + blobSHA + ":" + mode`.
       - Read working-tree deltas via `git status --porcelain=v1 -z --untracked-files=all`:
         - For deleted paths (`x == 'D' || y == 'D'`): `delete(files, path)`.
         - For renames (`x == 'R'`): `delete(files, origPath)` and hash `newPath`.
         - For modified/added/untracked paths: `os.Lstat` -> if symlink, `"link:" + readlink`; if regular file, `"sha:" + sha256Hex + ":" + octalPerm`; if missing on disk, `delete(files, path)`.
     - Add `DiffManifests(golden, current map[string]string) (changed []string, deleted []string)`:
       - `changed`: sorted paths in `current` where `golden[p] != current[p]`.
       - `deleted`: sorted paths in `golden` not present in `current`.
     - Add `EncodeSyncMultipart(meta types.SyncMeta, archive []byte) (body []byte, contentType string, err error)` using stdlib `mime/multipart`: writes JSON `"meta"` part and optional gzip tar `"archive"` part.
  2. Unit tests in `pkg/storage/storage_test.go`:
     - Clean repo -> modify 1 file + delete 1 file + add 1 untracked file + rename 1 file + chmod 1 file -> `DiffManifests` returns exact `changed` and `deleted`.
     - Dirty file at `init` later reverted to committed content -> `DiffManifests` detects the revert and includes it in `changed`.
     - Untracked file at `init` later deleted -> `DiffManifests` puts it in `deleted`.
     - Rebase / amend commit -> `DiffManifests` handles cleanly without git history errors.
     - Symlink target change -> detected in `changed`.
     - `EncodeSyncMultipart` roundtrip verification.
  3. Pure-Go host PTY client (`pkg/ptyclient/client.go`, `term_linux.go`, `term_other.go`, zero external deps):
     - `Attach(ctx context.Context, addr string, req types.PTYRequest, stdin *os.File, stdout io.Writer) (int, error)`:
       - Queries host terminal size (`TIOCGWINSZ = 0x5413`) if `stdin` is a TTY (`TCGETS = 0x5401` succeeds); sets `req.Rows`, `req.Cols`.
       - Dials `net.DialTimeout("tcp", addr, 5*time.Second)`, sends raw HTTP/1.1 `POST /pty` with JSON body and `Connection: Upgrade`, `Upgrade: bvm-pty`, reads response with `http.ReadResponse(br, httpReq)`, verifies `101 Switching Protocols`.
       - If `stdin` is a TTY: saves `origTermios` via `TCGETS`, switches to raw mode via `TCSETS`, and registers `defer restoreTerm(stdin.Fd(), orig)` so terminal is ALWAYS restored even on error or panic.
       - Registers `SIGWINCH` listener (when TTY) to send `PTYFrameResize` frames on resize.
       - Bridges `stdin` -> `PTYFrameData` and server `PTYFrameData` -> `stdout`, returning the exit code from `PTYFrameExit` when the remote shell finishes.
       - Serializes writes to `conn` with a mutex so `PTYFrameData` and `PTYFrameResize` never interleave bytes!
       - Put Linux termios/winsize ioctls in `term_linux.go` and stub in `term_other.go` so `go test ./...` on macOS compiles cleanly!
  4. Pure-Go TCP forwarder (`pkg/ptyclient/forward.go`):
     - `Forward(ctx context.Context, guestAPIAddr string, guestPort, hostPort int, ready func(actualHostPort int)) error`:
       - Listens on `fmt.Sprintf("127.0.0.1:%d", hostPort)` (`hostPort == 0` picks an ephemeral port). Calls `ready(actualPort)` if non-nil.
       - Stops accepting when `ctx` cancels.
       - For each incoming client connection, dials `guestAPIAddr`, sends `POST /tcp?port=<guestPort>` with `Connection: Upgrade`, `Upgrade: bvm-tcp`, verifies `101 Switching Protocols`, and copies bytes bidirectionally until EOF or context cancel.
  5. Unit tests in `pkg/ptyclient/client_test.go`: mock `POST /pty` hijack (bidirectional data, resize frame delivery, non-zero exit code, non-TTY stdin pipe) AND mock `POST /tcp?port=3000` hijack (roundtrip HTTP request/response through `Forward` listener).
- **Gate:** `go vet ./pkg/storage/... ./pkg/ptyclient/... && go test -race ./pkg/storage/... ./pkg/ptyclient/...` passes.

#### PR #13 (`feat/v2-t11-gateway-release`): T11: Multi-Provider Streaming Gateway & Single-Tarball Release Packaging
- **Owned Files:**
  - `pkg/gateway/proxy.go` (edit)
  - `pkg/gateway/proxy_test.go` (edit)
  - `.github/workflows/release.yml` (new)
  - `scripts/install.sh` (new)
- **Task:**
  1. Extend `pkg/gateway/proxy.go` (100% backwards-compatible with existing `AnthropicKey`/`OpenAIKey`/`Upstreams` fields):
     - Add built-in `ProviderOpenRouter = "openrouter"` (`https://openrouter.ai/api`, `Authorization: Bearer <OpenRouterKey>`).
     - Add `OpenRouterKey string` and `ExtraProviders []ProviderRoute` (`Name`, `Upstream`, `Key`, `AuthHeader` in `{"Authorization", "X-Api-Key"}`) to `Config`.
     - Validate extra provider names (`^[a-z0-9_-]{1,32}$`), `http`/`https` upstream URLs, and reject path traversal across all registered provider routes in `parseRoute`.
     - Set `FlushInterval: -1` on `httputil.ReverseProxy` so SSE token streams (`text/event-stream`) from Anthropic, OpenAI, and OpenRouter flush immediately with zero buffering stalls.
     - Keep `authorized()` accepting both `x-api-key` and `Authorization: Bearer` in constant time, stripping inbound auth headers and injecting only the real provider key on outbound requests.
     - Return actionable error messages on `403` and `502` responses telling the user which provider route failed and how to register custom endpoints under `[providers.<name>]` in `branchvm.toml`.
  2. Extend `pkg/gateway/proxy_test.go`:
     - Test `/openrouter/v1/chat/completions` Bearer injection and dummy-key stripping.
     - Test custom `ExtraProviders` with both `X-Api-Key` and `Authorization` header modes, plus invalid provider name / traversal rejection.
     - Test streaming SSE response chunk arrives immediately before upstream closes body (`FlushInterval: -1`).
     - Test descriptive `403` body on unknown provider path.
  3. Add `.github/workflows/release.yml`:
     - Triggers on tags `v*`.
     - Matrix build for `linux/amd64` and `linux/arm64`: compiles `branchvm` (`CGO_ENABLED=0`) and static `execd` (`CGO_ENABLED=0`), packages `branchvm_<version>_linux_<arch>.tar.gz` containing `branchvm`, `execd`, `scripts/fetch-assets.sh`, and `SHA256SUMS`, uploads to GitHub Release via `softprops/action-gh-release`.
  4. Add `scripts/install.sh`:
     - Clean 1-line Linux installer (`curl -fsSL https://raw.githubusercontent.com/Heman10x-NGU/branchvm/main/scripts/install.sh | sudo sh`): detects `amd64`/`arm64`, downloads matching release tarball + `SHA256SUMS`, verifies checksum, installs `/usr/local/bin/branchvm` and `/var/lib/branchvm/assets/execd`, prints next step (`fetch-assets.sh`).
- **Gate:** `go vet ./pkg/gateway/... && go test -race ./pkg/gateway/...` passes; `bash -n scripts/install.sh` passes.

---

### Wave 3 (Sequential Integration, Live `fc-lab` Verification & Docs)

#### PR #14 (`feat/v2-t12-orchestrator-cli-verify`): T12: Orchestrator Wiring, `diff`/`shell`/`logs`/`forward`/`gc --all` Commands, Live `fc-lab` Gate & `README.md`
- **Owned Files:**
  - `pkg/pool/orchestrator.go` (edit)
  - `pkg/pool/branch.go` (edit)
  - `pkg/pool/verifier.go` (edit)
  - `cmd/branchvm/main.go` (edit)
  - `cmd/branchvm/cmd_init.go` (edit)
  - `cmd/branchvm/cmd_run.go` (edit)
  - `cmd/branchvm/cmd_apply.go` (edit)
  - `cmd/branchvm/cmd_gc.go` (edit)
  - `cmd/branchvm/cmd_diff.go` (new)
  - `cmd/branchvm/cmd_shell.go` (new)
  - `cmd/branchvm/cmd_logs.go` (new)
  - `cmd/branchvm/cmd_forward.go` (new)
  - `Makefile` (edit)
  - `README.md` (edit)
- **Task:**
  1. Remove hardcoded `databaseName = "authdemo"` and `databaseURL` from `pkg/pool/orchestrator.go` and `pkg/pool/branch.go`.
  2. Wire `branchvm.toml` + `--image` / `dockerfile` + `golden/meta.json` in `pkg/pool/orchestrator.go` and `cmd/branchvm/cmd_init.go`:
     - `cmdInit` adds `--image`, `--execd`, `--rootfs-size` flags and uses `fs.Visit` for strict precedence (`explicit CLI flag > branchvm.toml > defaults`).
     - If `opt.Image != ""` or `cfg.Dockerfile != ""`, call `rootfs.BuildFromImage` (with `RepoDir: opt.Repo`) to build/reuse cached `rootfs.ext4` and copy/reflink it into `golden/rootfs.ext4`. Otherwise use `/var/lib/branchvm/base/rootfs.ext4` (legacy path).
     - `waitReady` waits for `h.OK && h.ServicesReady` (backward compatible with `h.PgReady`).
     - `ensureDatabase` runs ONLY when `cfg.Database != ""` and `postgres` is in `cfg.Services`.
     - Record `golden/meta.json` atomically (`GoldenMeta` with `RepoPath`, `HostSHA`, `Image`, `Dockerfile`, `ImageDigest`, `MemMB`, `VCPUs`, `Database`, `Services`, `Env`, `Providers`, and `GoldenFiles` from `storage.WorkspaceManifest`).
  3. Wire delta sync + inherited config + agent env in `pkg/pool/orchestrator.go`, `pkg/pool/branch.go`, and `cmd/branchvm/cmd_run.go`:
     - Load `golden/meta.json` (if missing, use legacy full-sync + Postgres defaults).
     - If `meta.RepoPath != ""` and `filepath.Clean(meta.RepoPath) != filepath.Clean(opt.Repo)`, fail with clear message telling user to run `branchvm init` in `opt.Repo`.
     - Inherit `MemMB` from `meta.MemMB` when `--mem` was not explicitly passed on CLI.
     - Compute `currentSHA, currentFiles := storage.WorkspaceManifest(opt.Repo)` and `changed, deleted := storage.DiffManifests(meta.GoldenFiles, currentFiles)`:
       - If `len(changed) == 0 && len(deleted) == 0` (clean delta): skip `POST /sync` completely (0 ms sync!).
       - Otherwise pack only `changed` files with `storage.PackFiles`, wrap with `storage.EncodeSyncMultipart(SyncMeta{Mode: "delta", Delete: deleted}, archive)`, and log the delta size (`delta: %d changed, %d deleted, %d KiB`).
       - Fall back to `Mode: "full"` when `meta.GoldenFiles == nil`.
     - Update `Branch.restore()`: run `waitPostgres` ONLY if `"postgres"` is in `meta.Services` (stateless snapshots skip `pg_isready` completely!).
     - Update `Branch.Env()`:
       - Start with `meta.Env` (from `branchvm.toml`).
       - Inject branch gateway and headless coding-agent env:
         - `HOME=/root`, `BRANCH_INDEX=<i>`
         - `ANTHROPIC_BASE_URL=http://<gw>/anthropic`, `ANTHROPIC_API_KEY=<dummy>`, `ANTHROPIC_AUTH_TOKEN=<dummy>`
         - `OPENAI_BASE_URL=http://<gw>/openai/v1`, `OPENAI_API_KEY=<dummy>`, `CODEX_API_KEY=<dummy>`
         - `OPENROUTER_BASE_URL=http://<gw>/openrouter/v1`, `OPENROUTER_API_KEY=<dummy>`
         - Custom provider `BaseURLEnv` and `KeyEnv` from `meta.Providers`
         - `IS_SANDBOX=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_TELEMETRY=1`, `DISABLE_ERROR_REPORTING=1`, `DISABLE_AUTOUPDATER=1`
     - Update `Branch.startGateway()` to pass `OpenRouterKey: os.Getenv("OPENROUTER_API_KEY")` and `ExtraProviders` built from `meta.Providers` (reading host env `p.APIKeyEnv`).
     - Update live table header/column in `orchestrator.go`: show configured service port (`5432` when Postgres enabled, `-` when stateless).
  4. Wire `diff`, `shell`, `logs`, `forward`, `gc --all`, and fix `loadPatch`:
     - Update `loadPatch` in `pkg/pool/orchestrator.go`: when `isFirecracker(bs.FirecrackerPID)` is true, fetch live `GET /diff` FIRST (and update the saved `.patch` file), falling back to saved `.patch` only if the VM is stopped.
     - Add `pool.Diff(ctx, DiffOptions{Index, RunID, Stat, Out})`.
     - Add `pool.Shell(ctx, ShellOptions{Index, RunID, Cmd, Stdin, Stdout})`: starts a temporary per-branch host gateway on `10.200.i.1:18080` (`defer stopGateway()`), attaches via `ptyclient.Attach` with `Branch.Env()` + host `TERM`.
     - Add `pool.Logs(ctx, LogsOptions{Index, RunID, Out})`: reads saved `<run>-branch-<i>.out` and writes it to `Out`.
     - Add `pool.Forward(ctx, ForwardOptions{Index, RunID, GuestPort, HostPort, Out})`: verifies live branch PID and forwards `127.0.0.1:<hostPort>` -> guest `127.0.0.1:<guestPort>` via `ptyclient.Forward` until Ctrl-C.
     - Add `pool.GCAll(out io.Writer) error` + `--all` flag in `cmd/branchvm/cmd_gc.go` to discard every active run and scan for leaks.
     - Add `cmd/branchvm/cmd_diff.go`, `cmd/branchvm/cmd_shell.go`, `cmd/branchvm/cmd_logs.go`, `cmd/branchvm/cmd_forward.go`; update `cmd/branchvm/main.go` usage and `printRunNext`.
  5. Update `pkg/pool/verifier.go`:
     - In `checkCoW` (`writeDistinct`), check `meta.Services`: if Postgres is enabled, run the DB isolation probe against `meta.Database` (defaulting to `"postgres"` if empty); if stateless, run the 1 MiB CoW disk write + SHA-256 probe without requiring `psql`.
  6. Update `Makefile` (add `build-all` building both `bin/branchvm` and `bin/execd`, keep existing targets) and `README.md` (document `branchvm.toml`, `--image`/`dockerfile`, `diff`, `shell`, `logs`, `forward`, `gc --all`, delta sync, real `claude -p` / `codex exec` usage, release install, updated Limits section, zero em-dashes and zero AI prose).
- **Live Verification Gate (Run Inside `fc-lab` Before Merging PR #14, Pass 3 Ordered Cheapest-to-Expensive):**
  1. `go vet ./... && go test -race ./...` passes; `go.mod` still has zero dependencies.
  2. `make build-all` succeeds.
  3. **Tier 1 Regression Gate (`examples/auth-demo`):**
     - `sudo -E ../../branchvm init` (reads `examples/auth-demo/branchvm.toml`)
     - `sudo -E ../../branchvm run --branches 3 -- ./demo-agents.sh`
     - `sudo ../../branchvm logs 2`, `sudo ../../branchvm diff 2 --stat`, and `sudo ../../branchvm diff 2`
     - `sudo ../../branchvm shell 2 -- go test ./...` returns exit 0
     - `sudo -E ../../branchvm apply 2` promotes patch + 0 leaks
  4. **Multi-Stack Stateless Docker Gate (`examples/node-demo`, no `iproute2`, no Postgres):**
     - `cd examples/node-demo && sudo ../../branchvm init` (proves `dockerfile` auto-build + stateless default)
     - `sudo ../../branchvm run --branches 3 -- node --test`
     - Delta sync + file deletion check: create `tmp_a.txt` before `init`, delete `tmp_a.txt` + edit 1 file after `init`, run `branchvm run --branches 2 -- sh -c 'test ! -e tmp_a.txt'`, confirm exit 0 and tiny delta log
     - `sudo ../../branchvm shell 1 -- node -e 'console.log(process.env.NODE_ENV)'`
     - Start listener inside branch and verify `branchvm forward` reaches it cleanly, then `sudo ../../branchvm gc --all` (0 leaks)
  5. **Security Verifier (`examples/auth-demo`):**
     - `sudo ../../branchvm verify --repo examples/auth-demo` prints **`6/6 PASS`**
  6. **Real Coding Agent Gate:**
     - Build `examples/agent-image/Dockerfile` (`branchvm-agent:latest`), run with `examples/auth-demo`:
       - Verify end-to-end mock-upstream streaming test passes at $0.
       - If host `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY` is set, run 1 live 3-branch `claude --dangerously-skip-permissions -p` pass through the zero-MITM gateway and verify `branchvm diff` + `branchvm apply` + zero leaked real keys in guest! (If no API key is set, record that clearly in the verification summary and stop for user key.)
