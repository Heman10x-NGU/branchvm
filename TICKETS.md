# `branchvm`: 4-Wave Parallel Execution Tickets (7 Tickets = 7 PRs)

- **Repo Path:** `/Users/heman10x/Downloads/claude_dev/personal_projects/branchvm`
- **Executor:** Claude High (authorized to adopt a better data-backed systems design if justified with real Linux/Firecracker reasoning)
- **Rule:** 1 Ticket = 1 Git Worktree Branch = 1 PR. Skip unnecessary pre-checks: write code first, then run ONE targeted build/test gate and fix all issues together.

---

## Wave Schedule (Maximize Parallel Worktrees)

```text
WAVE 1 (Sequential, ~15 min):
  PR #1 [feat/t0-env-assets-contracts]  -> T0: Lima fc-lab + Firecracker >=1.17.0 + shared pkg/types

WAVE 2 (4 PARALLEL GIT WORKTREES, ZERO FILE OVERLAP, ~25 min wall time):
  PR #2 [feat/t1-execd-fcclient-rootfs] -> T1: cmd/execd + pkg/fcclient + scripts/build-rootfs.sh
  PR #3 [feat/t2-xfs-cow-sync]          -> T2: pkg/storage/{xfs,sync}.go
  PR #4 [feat/t3-netiso-cgroup-state]   -> T3: pkg/netiso + pkg/cgroup
  PR #5 [feat/t4-gateway-auth-demo]     -> T4: pkg/gateway + examples/auth-demo

WAVE 3 (Sequential after merging PRs #2-#5, ~25 min):
  PR #6 [feat/t5-orchestrator-verify]   -> T5: pkg/pool/{orchestrator,verifier}.go + cmd/branchvm (init/run/apply/gc/verify)

WAVE 4 (Sequential after merging PR #6, ~25 min):
  PR #7 [feat/t6-bench-readme-demo]     -> T6: pkg/storage/{prefault,uffd}.go + cmd_bench.go + BENCHMARKS.md + README.md + record-demo.sh
```

---

## End State (Plain Terms)

### What It SHOULD Be
1. One Go repo (~2,200 to 2,800 LOC) with two binaries (`branchvm` CLI + static Linux PID 1 `execd`) turning free local SSD space into $0/mo Firecracker microVM sandboxes.
2. `branchvm run --branches 3 -- <cmd>` clones a real Postgres-backed app (code + packages + live Postgres `:5432`) into 3 isolated Firecracker microVMs in under a second via XFS `reflink` CoW disks and shared `MAP_PRIVATE` RAM, running all 3 concurrently with zero port/DB clashes and leaving host `git status` clean.
3. `branchvm apply <id>` runs `git apply --check` + `git apply` for only the winning code diff and cleans up every VM, `netns`, `veth`, cgroup, and reflink disk (`0` leaks).
4. Zero-MITM host LLM gateway (`10.200.i.1:18080`) keeps real API keys out of VMs without fake SSL certs; `nftables` `policy drop` blocks external IPs, host `127.0.0.1`, LAN, `169.254.169.254`, and raw port `53`/`853` DNS exfiltration.
5. `branchvm verify` passes 6/6 live attacks inside microVMs (`env`, firewall, port-53 DNS, 5-clone CoW disk integrity, 5-clone `VMGenID` entropy uniqueness, `0`-leak teardown).
6. `branchvm bench` drops Linux page cache before cold runs and writes real `results/raw.jsonl` + `BENCHMARKS.md` (Cold boot vs. Snapshot vs. 3-branch run; Cold & Warm `mmap` vs. `mincore`/`fadvise(WILLNEED)` vs. `userfaultfd`; RAM density across `1, 3, 5, 10, 20` clones).

### What It SHOULD NOT Be
- No web UI, no React dashboard, no SaaS login, no host DB server.
- No TLS MITM root CA injection.
- No `virtio-vsock` transport (use private `veth`/`tap0` HTTP `10.200.i.2:8080`).
- No `firecracker-go-sdk` black-box wrapper.
- No fake benchmark numbers or fake `VMGenID` passes.
- No leaked PIDs, `netns`, `veth`, cgroups, or clone `.ext4` files.
- No em-dashes or AI filler prose in `README.md` or docs.

---

## Critical Edge Cases Already Caught (Obey In Code)

1. **Lima VirtioFS mount:** Keep `/var/lib/branchvm` strictly inside Lima's native Linux filesystem (NOT on macOS VirtioFS `/Users/...`), and build/run binaries natively inside `fc-lab` (use `--mount-writable` only for repo access).
2. **Relative rootfs path:** Pass `"path_on_host": "rootfs.ext4"` to Firecracker and set `cmd.Dir` to each clone's isolated directory containing its XFS `reflink` copy.
3. **Guest cross-compile:** Build `cmd/execd` with `CGO_ENABLED=0 GOOS=linux GOARCH=<guest-arch>` so it never produces a macOS Mach-O binary.
4. **Postgres refuses root:** `execd` (PID 1 root) MUST drop UID/GID to user `postgres` (with `/run/postgresql` owned by `postgres`) and wait for `pg_isready` before reporting `pg_ready: true`.
5. **ARM64 (`aarch64`) Linux 6.10+ kernel for `VMGenID`:** On Apple M4 Lima (`aarch64`), Firecracker boots uncompressed ARM64 `Image` (not ELF `vmlinux`), and DeviceTree `VMGenID` requires Linux **6.10+** (x86_64 uses ACPI in `>= 5.18`). `scripts/fetch-assets.sh` must fetch a `>= 6.10` kernel on `aarch64` (e.g., Ubuntu 24.10/25.04 or upstream `6.10+` Firecracker kernel); never fake a `VMGenID` pass if `/dev/urandom` matches across clones.
6. **`userfaultfd` sysctl on Ubuntu 24.04:** Run `branchvm` under `sudo` inside `fc-lab` (or set `sysctl -w vm.unprivileged_userfaultfd=1`) so `UFFD` and `drop_caches` work cleanly.

---

## Detailed Tickets

### Wave 1 -> PR #1 (`feat/t0-env-assets-contracts`): T0: Lima KVM Lab, Assets & Shared Contracts
- **Owned Files:** `scripts/setup-lima.sh`, `scripts/fetch-assets.sh`, `pkg/types/types.go`
- **Task:**
  1. Write & run `scripts/setup-lima.sh` (`limactl start --name fc-lab --vm-type vz --nested-virt --cpus 4 --memory 6 --disk 40 --mount-writable template:ubuntu`), install `xfsprogs`, `nftables`, `iproute2`, `curl`, `jq`, `git`, `golang`, `debootstrap` / `docker.io`, verify `/dev/kvm`.
  2. Write & run `scripts/fetch-assets.sh`: detect `uname -m`, install Firecracker `>= v1.17.0` + matching Linux `>= 6.10` kernel (`Image` on `aarch64`, `vmlinux` on `x86_64`) into `/var/lib/branchvm/assets/`.
  3. Define shared Go structs & interfaces in `pkg/types/types.go` (`ExecRequest`, `ExecResponse`, `HealthResponse`, `EntropyResponse`, `BranchSpec`, `BranchResult`, `RunState`, `BenchRecord`) so all 4 Wave 2 worktrees compile against identical contracts.
- **Gate:** `go vet ./...` passes; inside `fc-lab`, `/dev/kvm` exists, `firecracker --version` >= `1.17.0`, kernel file exists in `/var/lib/branchvm/assets/`. Merge PR #1 to `main` before starting Wave 2.

---

### Wave 2 (Run All 4 In Parallel Git Worktrees)

#### PR #2 (`feat/t1-execd-fcclient-rootfs`): T1: Guest PID 1 (`execd`), Pure Go `fcclient` & Rootfs Builder
- **Owned Files:** `cmd/execd/main.go`, `pkg/fcclient/types.go`, `pkg/fcclient/client.go`, `pkg/fcclient/client_test.go`, `scripts/build-rootfs.sh`
- **Task:**
  1. Build static Linux `cmd/execd/main.go` (PID 1 `/sbin/init`): mounts `/proc`, `/sys`, `/dev`, `/dev/pts`, `/tmp`; configures `lo` + `eth0` (`172.16.0.2/30`, gw `172.16.0.1`); reaps `SIGCHLD` zombies; drops UID/GID to `postgres` to start Postgres 16 on `127.0.0.1:5432` and waits for `pg_isready`; serves HTTP on `172.16.0.2:8080`: `GET /health`, `GET /entropy`, `POST /sync` (gzip tar -> `/workspace` + git commit `BASE_SHA`), `POST /exec`, `GET /diff` (`git add -N .` + `git diff --binary <BASE_SHA>`), `POST /checkpoint` (`CHECKPOINT;`, kill idle DB sessions, delete `/etc/machine-id` & random seeds, `sync`).
  2. Build pure Go Unix-socket `pkg/fcclient/` (`PutMachineConfig`, `PutBootSource`, `PutDrive`, `PutNetworkInterface`, `StartInstance`, `PauseVM`, `ResumeVM`, `CreateSnapshot`, `LoadSnapshot` supporting `File` and `Uffd`).
  3. Write `scripts/build-rootfs.sh` building ~1.5 GB Ubuntu 24.04 `base/rootfs.ext4` with Postgres 16, `git`, `curl`, `dnsutils`, Go runtime, and static `execd` as `/sbin/init`.
- **Gate:** `go test -race ./pkg/fcclient/...` passes AND `make build-guest` builds a Linux static ELF binary.

#### PR #3 (`feat/t2-xfs-cow-sync`): T2: XFS `reflink` CoW Storage & Host Sync/Apply
- **Owned Files:** `pkg/storage/xfs.go`, `pkg/storage/sync.go`, `pkg/storage/storage_test.go`
- **Task:**
  1. `xfs.go`: creates/mounts loopback `/var/lib/branchvm/xfs.img` (`mkfs.xfs -m reflink=1,crc=1`) at `/var/lib/branchvm/pool/`; implements `CloneRootfs(base, dst)` via `cp --reflink=always` + `RemoveClone`.
  2. `sync.go`: packs tracked + untracked non-ignored dirty host files (`git status --porcelain`) into a gzip tar stream -> `POST /sync`; implements `ApplyPatch(repoDir, patch)` running `git apply --check` before `git apply`.
- **Gate:** `go test -race ./pkg/storage/...` passes (unit tests for tar/patch + Linux XFS reflink test cloning 5 independent copies with zero base mutation).

#### PR #4 (`feat/t3-netiso-cgroup-state`): T3: Per-Branch `netns`/`nftables`, `cgroups v2` & Crash-Safe State
- **Owned Files:** `pkg/netiso/netns.go`, `pkg/netiso/firewall.go`, `pkg/netiso/netiso_test.go`, `pkg/cgroup/cage.go`, `pkg/cgroup/state.go`, `pkg/cgroup/cgroup_test.go`
- **Task:**
  1. `pkg/netiso/`: creates `netns` `bvm-<run>-i`, `tap0` (`172.16.0.1/30` <-> `.2/30`), `veth` (`10.200.i.1/30` <-> `.2/30`), and `nftables` `policy drop` allowing ONLY DNAT `10.200.i.2:8080 -> 172.16.0.2:8080` and guest `-> 10.200.i.1:18080` (blocking all external IPs, host `127.0.0.1`, RFC1918 LAN, `169.254.169.254`, and raw UDP/TCP port `53`/`853`).
  2. `pkg/cgroup/`: manages `/sys/fs/cgroup/branchvm/<run>-i` (`memory.max`, `cpu.max`, `pids.max`, `memory.current`), atomic run-state JSON (`/var/lib/branchvm/runs/<run-id>.json`), `SIGINT`/`SIGTERM` cleanup trap, and stale-PID `GC()`.
- **Gate:** `go test -race ./pkg/netiso/... ./pkg/cgroup/...` passes inside `fc-lab` with `0` leaked namespaces, `veth`, or cgroups.

#### PR #5 (`feat/t4-gateway-auth-demo`): T4: Zero-MITM Host LLM Gateway & Postgres Demo App
- **Owned Files:** `pkg/gateway/proxy.go`, `pkg/gateway/proxy_test.go`, `examples/auth-demo/`
- **Task:**
  1. `pkg/gateway/proxy.go`: HTTP proxy bound strictly to per-branch `veth` host IP (`10.200.i.1:18080`). Verifies dummy key `bvm_dummy_<i>`, routes ONLY allowlisted paths (`/anthropic/...` -> `https://api.anthropic.com`, `/openai/...` -> `https://api.openai.com`), injects real host env keys over standard Go HTTPS (zero fake CAs), returns `403` for everything else.
  2. `examples/auth-demo/`: small Go API + Postgres 16 schema + 14 tests (1 intentional failing auth migration race test) + `demo-agents.sh` (deterministic offline 3-branch simulation where branch 1 fails, branch 2 fixes the race and passes 14/14, branch 3 passes with an alternative fix) so the demo runs both with real `claude` and offline at $0 API cost.
- **Gate:** `go test -race ./pkg/gateway/...` passes and `examples/auth-demo` compiles cleanly.

---

### Wave 3 -> PR #6 (`feat/t5-orchestrator-verify`): T5: Orchestrator CLI (`init`, `run`, `apply`, `gc`) & 6-Attack `branchvm verify`
- **Owned Files:** `pkg/pool/orchestrator.go`, `pkg/pool/verifier.go`, `cmd/branchvm/main.go`, `cmd/branchvm/cmd_init.go`, `cmd/branchvm/cmd_run.go`, `cmd/branchvm/cmd_apply.go`, `cmd/branchvm/cmd_gc.go`, `cmd/branchvm/cmd_verify.go`
- **Task:**
  1. Wire `branchvm init` (golden boot with relative `"rootfs.ext4"` -> sync -> `POST /checkpoint` -> pause -> `CreateSnapshot`).
  2. Wire `branchvm run --branches N -- <cmd>` (`GC()`, start per-branch gateways, fan out N clones concurrently in isolated `netns`/cgroups/`cmd.Dir`, sync dirty files, inject `ANTHROPIC_BASE_URL` + dummy key, execute `<cmd>`, print live table with `branch`, `restore_ms`, `pg_port`, `host_ram_mb`, `status`).
  3. Wire `branchvm apply <id>` (`GET /diff` -> `git apply --check` -> `git apply` -> full teardown) and `branchvm gc`.
  4. Wire `branchvm verify`: fires 6 live probes inside microVM clones (`env` key check, `curl evil.example.com` + `169.254.169.254` firewall drop, `nslookup secret.attacker.com 8.8.8.8` port-53 drop, 5-clone concurrent CoW disk SHA-256 test, 5-clone `VMGenID` `/dev/urandom` uniqueness, `0`-leak teardown) and prints crisp PASS/FAIL table.
- **Gate:** Inside `fc-lab` on `examples/auth-demo`: `branchvm init` succeeds; `branchvm run --branches 3 -- ./demo-agents.sh` runs 3 Postgres instances on `:5432` concurrently, leaves host `git status` clean; `branchvm apply 2` promotes cleanly; `branchvm verify` prints `6/6 PASS`.

---

### Wave 4 -> PR #7 (`feat/t6-bench-readme-demo`): T6: Honest Local-SSD Benchmarker (`branchvm bench`), Deadpan `README.md` & 60s Demo Script
- **Owned Files:** `pkg/storage/prefault.go`, `pkg/storage/uffd.go`, `cmd/branchvm/cmd_bench.go`, `BENCHMARKS.md`, `results/raw.jsonl`, `README.md`, `scripts/record-demo.sh`
- **Task:**
  1. Implement `pkg/storage/prefault.go` (`mincore(2)` working-set recorder -> `ws.bin` + `posix_fadvise(WILLNEED)`) and `pkg/storage/uffd.go` (minimal Go `userfaultfd` `SCM_RIGHTS` + `UFFDIO_COPY` pager).
  2. Implement `branchvm bench`: `>= 10` iterations per mode, dropping Linux page cache (`sync && echo 3 > /proc/sys/vm/drop_caches`) before every Cold run, outputting `results/raw.jsonl` and `BENCHMARKS.md` (Table 1: Cold boot vs. Snapshot vs. 3-branch run + tar sync; Table 2: Cold & Warm `mmap` vs. `mincore`/`fadvise` vs. `UFFD`; Table 3: Host RAM density across `1, 3, 5, 10, 20` clones).
  3. Write deadpan `README.md` (zero em-dashes, zero AI prose) with architecture diagram, Alternatives comparison table (`branchvm` vs. `boat.dev`/Modal/E2B, Docker `sbx`, Microsandbox, E2B Embed, Anthropic `srt`, `git worktree`, `vmsan`), Quickstart, live `branchvm verify` output, and link to `BENCHMARKS.md`.
  4. Write `scripts/record-demo.sh` for clean 60-second recording.
- **Gate:** `go vet ./...`, `go test -race ./...`, `branchvm verify` (`6/6 PASS`), real measured `results/raw.jsonl` + `BENCHMARKS.md` committed.
