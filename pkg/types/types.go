// Package types holds the shared contracts between the branchvm host
// orchestrator, the in-guest agent, and the benchmark harness.
package types

import "time"

// RestoreMode selects how a branch VM is brought up.
type RestoreMode = string

const (
	// ColdRestore boots the kernel from scratch.
	ColdRestore RestoreMode = "cold"
	// FileRestore restores a snapshot with file-backed guest memory.
	FileRestore RestoreMode = "file"
	// UffdRestore restores a snapshot with memory served by a userfaultfd pager.
	UffdRestore RestoreMode = "uffd"
)

// ExecRequest asks the guest agent to run a command.
type ExecRequest struct {
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

// ExecResponse is the result of an ExecRequest.
type ExecResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

// HealthResponse reports guest liveness and Postgres readiness.
type HealthResponse struct {
	OK        bool   `json:"ok"`
	PgReady   bool   `json:"pg_ready"`
	UptimeSec int64  `json:"uptime_sec"`
	Hostname  string `json:"hostname"`
}

// EntropyResponse exposes per-guest identity so tests can prove clones
// diverged after restore (VMGenID reseed, regenerated machine-id).
type EntropyResponse struct {
	Hex       string `json:"hex"`
	MachineID string `json:"machine_id"`
	BootID    string `json:"boot_id"`
}

// BranchSpec is everything needed to launch one branch VM.
type BranchSpec struct {
	Index       int         `json:"index"`
	RunID       string      `json:"run_id"`
	Name        string      `json:"name"`
	GuestIP     string      `json:"guest_ip"`
	HostIP      string      `json:"host_ip"`
	GatewayAddr string      `json:"gateway_addr"`
	APIKey      string      `json:"api_key"`
	MemMB       int64       `json:"mem_mb"`
	VCPUs       int         `json:"vcpus"`
	KernelPath  string      `json:"kernel_path"`
	RootfsPath  string      `json:"rootfs_path"`
	CloneDir    string      `json:"clone_dir"`
	SnapshotDir string      `json:"snapshot_dir"`
	RestoreMode RestoreMode `json:"restore_mode"`
}

// BranchResult is the outcome of launching one branch VM.
type BranchResult struct {
	Index     int    `json:"index"`
	Status    string `json:"status"`
	RestoreMs int64  `json:"restore_ms"`
	PgPort    int    `json:"pg_port"`
	HostRAMMB int64  `json:"host_ram_mb"`
	ExitCode  int    `json:"exit_code"`
	Err       string `json:"err,omitempty"`
}

// RunState is persisted to disk so a later process can garbage-collect
// resources left behind by a crashed run.
type RunState struct {
	RunID     string        `json:"run_id"`
	StartedAt time.Time     `json:"started_at"`
	Branches  []BranchState `json:"branches"`
}

// BranchState records every host resource owned by one branch.
type BranchState struct {
	Index          int    `json:"index"`
	FirecrackerPID int    `json:"firecracker_pid"`
	NetNS          string `json:"netns"`
	VethHost       string `json:"veth_host"`
	TapName        string `json:"tap_name"`
	CgroupPath     string `json:"cgroup_path"`
	CloneDir       string `json:"clone_dir"`
	GatewayPID     int    `json:"gateway_pid"`
	PagerPID       int    `json:"pager_pid,omitempty"`
	Status         string `json:"status"`
}

// BenchRecord is one benchmark sample. Table selects which report it
// feeds: "boot" (cold vs file vs uffd), "pager" (pager strategies), or
// "density" (RAM per clone as clone count grows).
type BenchRecord struct {
	Table        string    `json:"table"`
	Mode         string    `json:"mode"`
	Pager        string    `json:"pager,omitempty"`
	Iteration    int       `json:"iteration"`
	Clones       int       `json:"clones"`
	ColdCache    bool      `json:"cold_cache"`
	RestoreMs    int64     `json:"restore_ms"`
	FirstQueryMs int64     `json:"first_query_ms,omitempty"`
	TotalMs      int64     `json:"total_ms"`
	HostRAMMB    int64     `json:"host_ram_mb"`
	PerCloneMB   float64   `json:"per_clone_mb,omitempty"`
	BytesFaulted int64     `json:"bytes_faulted"`
	PageFaults   int64     `json:"page_faults,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
}
