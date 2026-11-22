#!/usr/bin/env python3
"""Live demo server for branchvm. Runs inside the fc-lab Linux VM as root.

Serves demo/index.html and streams real branchvm runs over Server-Sent Events.
Lima forwards the guest port to the Mac, so open http://localhost:8090.
"""
import glob
import json
import os
import re
import shutil
import statistics
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
TREE = os.environ.get("BVM_TREE", os.path.dirname(HERE))
DEMO = os.path.join(TREE, "examples/auth-demo")
RUNS = "/var/lib/branchvm/runs"
PORT = int(os.environ.get("PORT", "8090"))
SENTINEL_KEY = "sk-ant-demo-sentinel-never-in-guest"
LOCK = threading.Lock()
ACTIVE_RUN_ID = None


def resolve_bin():
    for candidate in (
        os.environ.get("BVM_BIN"),
        os.path.join(TREE, "branchvm"),
        os.path.join(TREE, "bin/branchvm"),
        shutil.which("branchvm"),
    ):
        if candidate and os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    return os.path.join(TREE, "branchvm")


def sh(cmd, cwd=None):
    return subprocess.run(cmd, shell=True, cwd=cwd, capture_output=True, text=True).stdout.strip()


def demo_dirty():
    out = sh("git status --short -- examples/auth-demo", cwd=TREE)
    return bool(out)


def discard_live_runs(bin_path):
    global ACTIVE_RUN_ID
    for path in glob.glob(os.path.join(RUNS, "*.json")):
        rid = os.path.basename(path)[:-5]
        if rid == "golden":
            continue
        subprocess.run([bin_path, "gc", "--run", rid], cwd=DEMO, capture_output=True)
    ACTIVE_RUN_ID = None


def leak_line():
    netns = sh("ip netns list | grep -c bvm- || true")
    veth = sh("ip -o link | grep -c bvmh- || true")
    fc = sh("pgrep -c firecracker || true")
    return f"leak check: netns={netns} veth={veth} firecracker={fc}"


def host_info():
    cpu = sh("nproc")
    mem = sh("awk '/MemTotal/{printf \"%.1f\", $2/1048576}' /proc/meminfo")
    fc = sh("firecracker --version | head -1")
    bin_path = resolve_bin()
    return {
        "arch": os.uname().machine,
        "cpus": cpu,
        "mem_gib": mem,
        "kernel": os.uname().release,
        "firecracker": fc,
        "binary_ready": os.path.isfile(bin_path) and os.access(bin_path, os.X_OK),
        "active_run": ACTIVE_RUN_ID,
        "demo_dirty": demo_dirty(),
    }


def bench_summary():
    path = os.path.join(TREE, "results/raw.jsonl")
    rows = []
    try:
        with open(path) as f:
            rows = [json.loads(line) for line in f if line.strip()]
    except OSError:
        return {"samples": 0}

    def med(pred, key):
        vals = [r[key] for r in rows if pred(r) and r.get(key)]
        return round(statistics.median(vals)) if vals else None

    cold = lambda r: r.get("cold_cache")
    out = {
        "samples": len(rows),
        "restore": med(lambda r: r["table"] == "boot" and r["mode"] == "snapshot-restore" and cold(r), "restore_ms"),
        "coldboot": med(lambda r: r["table"] == "boot" and r["mode"] == "cold-boot" and cold(r), "restore_ms"),
        "pagers": {},
        "density": [],
    }
    for p in ("mmap", "fadvise", "uffd"):
        out["pagers"][p] = {
            "cold": med(lambda r, p=p: r["table"] == "pager" and r.get("pager") == p and cold(r), "first_query_ms"),
            "warm": med(lambda r, p=p: r["table"] == "pager" and r.get("pager") == p and not cold(r), "first_query_ms"),
        }
    for r in rows:
        if r["table"] == "density":
            out["density"].append({
                "clones": r["clones"],
                "total_mb": r["host_ram_mb"],
                "per_clone_mb": round(r.get("per_clone_mb") or 0),
            })
    return out


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send_json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def event(self, kind, data):
        self.wfile.write(f"event: {kind}\ndata: {json.dumps(data)}\n\n".encode())
        self.wfile.flush()

    def run_cmd_stream(self, argv, cwd=DEMO, env=None):
        proc = subprocess.Popen(
            argv,
            cwd=cwd,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )
        run_id = None
        try:
            for line in proc.stdout:
                line = line.rstrip("\n")
                m = re.search(r"\brun (r[0-9a-f]+)", line)
                if m:
                    run_id = m.group(1)
                self.event("line", line)
            return proc.wait(), run_id
        except (BrokenPipeError, ConnectionResetError):
            proc.terminate()
            raise

    def do_GET(self):
        path = self.path.split("?")[0]
        if path in ("/", "/index.html"):
            with open(os.path.join(HERE, "index.html"), "rb") as f:
                body = f.read()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif path == "/api/host":
            self.send_json(host_info())
        elif path == "/api/bench":
            self.send_json(bench_summary())
        elif path.startswith("/api/run/"):
            self.stream(path.rsplit("/", 1)[-1])
        else:
            self.send_json({"error": "not found"}, 404)

    def stream(self, action):
        global ACTIVE_RUN_ID
        if action not in ("fork", "agents", "apply", "reset", "attack"):
            self.send_json({"error": "unknown action"}, 404)
            return
        if not LOCK.acquire(blocking=False):
            self.send_json({"error": "a run is already in progress"}, 409)
            return
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()

            bin_path = resolve_bin()
            if action != "reset" and not (os.path.isfile(bin_path) and os.access(bin_path, os.X_OK)):
                self.event("line", f"error: branchvm binary not found at {bin_path}; run `go build -o branchvm ./cmd/branchvm` in {TREE}")
                self.event("done", {"exit": 1, "active_run": ACTIVE_RUN_ID, "demo_dirty": demo_dirty()})
                return

            if action == "reset":
                if os.path.isfile(bin_path) and os.access(bin_path, os.X_OK):
                    discard_live_runs(bin_path)
                self.event("line", "$ git checkout -- examples/auth-demo")
                code, _ = self.run_cmd_stream(["git", "checkout", "--", "examples/auth-demo"], cwd=TREE)
                self.event("line", "examples/auth-demo restored to clean state")
                self.event("line", leak_line())
                self.event("done", {"exit": code, "active_run": ACTIVE_RUN_ID, "demo_dirty": demo_dirty()})
                return

            if action == "apply":
                cmd = [bin_path, "apply", "2"]
                if ACTIVE_RUN_ID:
                    cmd.extend(["--run", ACTIVE_RUN_ID])
                self.event("line", "$ branchvm " + " ".join(cmd[1:]))
                code, _ = self.run_cmd_stream(cmd, cwd=DEMO)
                if code == 0:
                    ACTIVE_RUN_ID = None
                    self.event("line", "$ git diff --stat -- examples/auth-demo")
                    self.run_cmd_stream(["git", "diff", "--stat", "--", "examples/auth-demo"], cwd=TREE)
                self.event("line", leak_line())
                self.event("done", {"exit": code, "active_run": ACTIVE_RUN_ID, "demo_dirty": demo_dirty()})
                return

            discard_live_runs(bin_path)

            if action == "fork":
                cmd = [bin_path, "run", "--branches", "3", "--", "pg_isready", "-h", "127.0.0.1"]
                self.event("line", "$ branchvm " + " ".join(cmd[1:]))
                code, run_id = self.run_cmd_stream(cmd, cwd=DEMO)
                if run_id:
                    self.event("line", f"$ branchvm gc --run {run_id}")
                    subprocess.run([bin_path, "gc", "--run", run_id], cwd=DEMO, capture_output=True)
                self.event("line", leak_line())
                self.event("done", {"exit": code, "active_run": ACTIVE_RUN_ID, "demo_dirty": demo_dirty()})
                return

            if action == "agents":
                if demo_dirty():
                    self.event("line", "$ git checkout -- examples/auth-demo")
                    subprocess.run(["git", "checkout", "--", "examples/auth-demo"], cwd=TREE, capture_output=True)
                cmd = [bin_path, "run", "--branches", "3", "--", "./demo-agents.sh"]
                self.event("line", "$ branchvm " + " ".join(cmd[1:]))
                code, run_id = self.run_cmd_stream(cmd, cwd=DEMO)
                ACTIVE_RUN_ID = run_id if code == 0 else None
                self.event("line", "$ git status --short -- examples/auth-demo")
                st = sh("git status --short -- examples/auth-demo", cwd=TREE)
                self.event("line", st if st else "(clean: host repo untouched while 3 microVMs ran)")
                self.event("done", {"exit": code, "active_run": ACTIVE_RUN_ID, "demo_dirty": demo_dirty()})
                return

            if action == "attack":
                env = os.environ.copy()
                if not env.get("ANTHROPIC_API_KEY"):
                    env["ANTHROPIC_API_KEY"] = SENTINEL_KEY
                cmd = [bin_path, "verify", "--repo", DEMO]
                self.event("line", "$ branchvm verify --repo examples/auth-demo")
                code, _ = self.run_cmd_stream(cmd, cwd=DEMO, env=env)
                self.event("line", leak_line())
                self.event("done", {"exit": code, "active_run": ACTIVE_RUN_ID, "demo_dirty": demo_dirty()})
                return
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            LOCK.release()


if __name__ == "__main__":
    print(f"branchvm demo on http://localhost:{PORT} (repo: {TREE}, bin: {resolve_bin()})", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
