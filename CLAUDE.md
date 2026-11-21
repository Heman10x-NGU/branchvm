# CLAUDE.md: Operating Directives for `branchvm`

You are executing on **Claude High**. Move fast, ship clean systems code, and finish all 4 waves cleanly.

## 1. Architectural Authority
- If you find a cleaner, faster, or safer systems design backed by real Linux/Firecracker mechanics and measured data, implement it and note the reasoning in your PR summary.
- Keep total codebase tight (~2,200 to 2,800 LOC of clean Go). Avoid bloat.

## 2. Speed & Anti-Waste Rules (Mandatory)
- **Skip unnecessary pre-checks.** Do not waste turns running speculative read/status loops before coding. Read the ticket, write all owned files in one clean pass, then run ONE targeted `go vet` / `go test -race` verification command, fix all errors together, and finish the PR.
- **1 Ticket = 1 Git Worktree Branch = 1 PR.** Stay strictly inside the files listed under your ticket so parallel Wave 2 worktrees merge with zero conflicts.

## 3. Hard Technical Invariants (Never Violate)
1. **No web UI, no TLS MITM fake root CAs, no `virtio-vsock`, no `firecracker-go-sdk`.**
2. **Relative rootfs path:** Always pass `"path_on_host": "rootfs.ext4"` to Firecracker and set `cmd.Dir` to each clone's isolated XFS `reflink` directory so `base/rootfs.ext4` is never opened writable by any clone.
3. **Cross-compile guest PID 1:** `cmd/execd` must always build with `CGO_ENABLED=0 GOOS=linux GOARCH=<target>` (never a macOS Mach-O binary).
4. **Postgres non-root startup:** Postgres 16 refuses to run as root; `execd` (PID 1) must drop UID/GID to user `postgres` (with `/run/postgresql` owned by `postgres`) and wait for `pg_isready`.
5. **Gateway binding:** Bind `pkg/gateway` strictly to each branch's host `veth` IP (`10.200.i.1:18080`), never `0.0.0.0`.
6. **Firecracker version:** Pin `>= v1.17.0` (`CVE-2026-122192` patched), keep memory ballooning disabled on File-backend restores.
7. **Honest benchmarks only:** Drop Linux page caches (`sync && echo 3 > /proc/sys/vm/drop_caches`) before cold runs; report real measured numbers including where `UFFD` loses on warm local SSD.
8. **Zero em-dashes and zero AI prose** in `README.md`, comments, and docs.
