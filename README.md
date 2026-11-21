# branchvm

branchvm runs N parallel agent experiments against a real Postgres-backed app on one Linux host. Each experiment is a Firecracker microVM restored from one shared memory snapshot of a booted guest with Postgres already accepting connections. Each guest disk is an XFS reflink copy of one golden rootfs, so a branch costs the blocks it writes and the pages it dirties. Each branch sits in its own network namespace whose firewall reaches one place: a host gateway that holds the real LLM API keys and swaps them in for a per-branch dummy key. When the agents finish, you read each branch's diff, apply the one you want to the host repo, and the rest are torn down with a leak scan.

## How it works

```
host (Linux, /dev/kvm)
+--------------------------------------------------------------------------+
| branchvm CLI          XFS pool (reflink)           golden snapshot       |
| init run apply gc     golden/rootfs.ext4           snapshot + mem        |
| verify bench            | cp --reflink=always      one copy, mapped      |
|    |                    v                          by every restored VM  |
|    |                  clones/<run>-<i>/rootfs.ext4        |              |
|    |                          |                           |              |
|    | sync tar.gz,             v                           v              |
|    | exec, diff   +-- netns bvm-<run>-<i>, nftables: gateway only ---+   |
|    +------------->| veth 10.200.<i>.2 --DNAT--> tap0 --> firecracker |   |
|                   | cgroup branchvm/<run>-<i>: memory, cpu, pids     |   |
|                   | guest: execd as PID 1 on :8080, postgres 16,     |   |
|                   |        /workspace, API key bvm_dummy_<i>         |   |
|                   +-------------|------------------------------------+   |
|                                 |  request with the dummy key            |
|                                 v                                        |
| gateway 10.200.<i>.1:18080: checks the dummy key, swaps in the real      |
| host key, forwards ------------------------------------------------------+--> api.anthropic.com
|                                                                          |    api.openai.com
+--------------------------------------------------------------------------+
```

`init` cold boots the golden VM, syncs the repo, runs an optional warm command, checkpoints Postgres, and writes the snapshot. `run` restores N branches concurrently, syncs the current workspace into each, runs your command, and stores each branch's `git diff`. `apply` applies one diff to the host repo and tears the whole run down.

## Quickstart

On an Apple silicon Mac (M3 or newer, macOS 15 or newer), create the lab VM, then work inside it:

```
scripts/setup-lima.sh
limactl shell fc-lab sudo bash scripts/fetch-assets.sh
limactl shell fc-lab
```

Inside the lab VM, from this repo (Lima mounts your home directory at the same path):

```
sudo scripts/build-rootfs.sh
go build -o branchvm ./cmd/branchvm
cd examples/auth-demo
sudo -E ../../branchvm init --warm 'go test -run ^$ ./...'
sudo -E ../../branchvm run --branches 3 -- ./demo-agents.sh
sudo -E ../../branchvm apply 2
git diff --stat
sudo ../../branchvm verify --repo .
cd ../..
sudo ./branchvm bench
```

On a Linux host with KVM, skip the Lima steps and run `scripts/fetch-assets.sh` as root. `sudo -E` keeps `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` for the gateway; the demo itself makes no network calls.

Measured in the lab: golden `init` takes 11.1 s cold without a warm command and about 7 min with a full `--warm` build. Per-branch snapshot restore took 246 to 421 ms with 3 branches restoring concurrently. In the demo, branch 1 fails 1 of 14 tests (13/14), branches 2 and 3 pass 14/14, and `apply 2` applies a 632 byte patch.

## Verification

`branchvm verify` restores 5 branches and fires six attacks at the isolation. Output from the lab:

```
ID  ATTACK              RESULT  EVIDENCE
P1  env-key             PASS    printenv="bvm_dummy_1" want "bvm_dummy_1"; env+/proc/1/environ (371 bytes, exit 0): host real keys unset, substring scan skipped
P2  firewall            PASS    93.184.216.34 exit=7, evil.example.com exit=6, 169.254.169.254 exit=7; control gateway exit=0 http=401
P3  dns-exfil           PASS    nslookup udp/53 exit=1, dig tcp/53 exit=9
P4  cow-disk            PASS    golden sha256 before=c86d3fc22bb2 after=c86d3fc22bb2 unchanged=true; 5 clone sha256 pairwise distinct=true [738720217d0b e029a0d2dede eeebe1c22d09 dc52d31b8432 09bca70094ef]; each DB saw only its own row
P5  vmgenid-entropy     PASS    5/5 distinct 32-byte /dev/urandom samples read right after restore [be429238 723fd09b 3e0bcf73 b1821112 a3a24c51]; boot_id: 5 distinct across 5 clones (clones resume one kernel boot, so a shared boot_id is expected)
P6  zero-leak teardown  PASS    netns=0 veth=0 cgroups=0 clone_dirs=0 firecracker=0 gateways=0 state_file_removed=true
6/6 PASS
```

P1 ran with the host keys unset, so the substring scan for real keys inside the guest had nothing to search for.

## Alternatives

| Tool | Isolation boundary | State cloning | Cost | Where it runs |
|---|---|---|---|---|
| branchvm | Firecracker microVM per branch, netns firewall, cgroup | Running guest memory and disk, Postgres included, from one snapshot | Free, your hardware | Local Linux with KVM |
| boat.dev | Full Ubuntu VM per sandbox | Fork a sandbox with its disk; a DB counts only if it runs inside | Per-second billing, plans from $20 a month | Hosted |
| Modal | gVisor sandbox | Sandbox snapshots; your existing DB stays outside | Per-second billing | Hosted |
| E2B | Firecracker microVM per sandbox | Pause, resume, and fork with memory; a DB counts only if it runs inside | Per-second billing | Hosted |
| Docker Sandboxes (sbx) | MicroVM with its own kernel and Docker Engine | Workspace mount or in-VM repo clone; services start fresh per sandbox | Free locally, cloud from $0.07 an hour | Local or Docker cloud |
| Microsandbox | libkrun microVM | New sandbox from an OCI image | Free, open source | Local |
| E2B Embed | Firecracker microVM per sandbox | Same pause, resume, and fork as E2B Cloud | Free, Apache-2.0, your hardware | One self-hosted Linux machine with KVM |
| Anthropic sandbox runtime (srt) | bubblewrap on Linux, sandbox-exec on macOS, proxy network filter | None; processes share the host filesystem and services | Free, open source | Local, no VM |
| git worktree | None | Files only; every worktree shares one DB | Free | Local |
| vmsan | Firecracker microVM with jailer and netns | Per-VM snapshot and restore; a DB counts only if it runs inside | Free, open source | Local Linux with KVM |

## Benchmarks

![branchvm benchmark summary: snapshot restore 190 ms vs 6234 ms cold boot, first query 1111 ms with uffd, 62 MiB per clone at 10 clones vs 1024 MiB private copies, 6 of 6 attacks pass](docs/benchmarks.svg)

See [BENCHMARKS.md](BENCHMARKS.md). Every number there comes from `branchvm bench`, with page caches dropped before each cold-cache sample; raw samples are in `results/raw.jsonl`.

## Limits

- Requires Linux with KVM (`/dev/kvm`). Firecracker does not run on macOS; on a Mac you need the Lima lab VM, which needs nested virtualization.
- The guest is one fixed rootfs: Ubuntu 24.04 with Postgres 16, git, and Go, built by `scripts/build-rootfs.sh`. Other stacks mean editing that script.
- The gateway forwards the Anthropic and OpenAI API paths only. The netns firewall drops every other outbound packet, so tools that need package mirrors or other APIs fail inside a branch.
- Branch IPs are per index, so one run owns the host at a time; `run` refuses to start while another run has live VMs.
- This is a lab tool, not production hardening. It runs as root, starts Firecracker without the jailer, and has no multi-tenant controls.
