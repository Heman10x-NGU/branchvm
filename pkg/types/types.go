// Package types holds the shared contracts between the branchvm host
// orchestrator, the in-guest agent, and the benchmark harness.
package types

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

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

// PTY framing protocol constants shared by cmd/execd and pkg/ptyclient.
const (
	PTYFrameData     byte = 1
	PTYFrameResize   byte = 2
	PTYFrameExit     byte = 3
	PTYFrameEOF      byte = 4
	PTYMaxFrameBytes      = 64 << 10
)

// WritePTYFrame writes one length-prefixed PTY frame to w.
func WritePTYFrame(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > PTYMaxFrameBytes {
		return fmt.Errorf("pty frame payload too large: %d > %d", len(payload), PTYMaxFrameBytes)
	}
	var hdr [5]byte
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if len(payload) == 0 {
		n, err := w.Write(hdr[:])
		if err != nil {
			return err
		}
		if n != len(hdr) {
			return io.ErrShortWrite
		}
		return nil
	}
	buf := make([]byte, len(hdr)+len(payload))
	copy(buf, hdr[:])
	copy(buf[len(hdr):], payload)
	n, err := w.Write(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return io.ErrShortWrite
	}
	return nil
}

// ReadPTYFrame reads one length-prefixed PTY frame from r.
func ReadPTYFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > PTYMaxFrameBytes {
		return 0, nil, fmt.Errorf("pty frame too large: %d > %d", n, PTYMaxFrameBytes)
	}
	if n == 0 {
		return hdr[0], nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// PTYRequest configures an interactive PTY session inside a guest.
type PTYRequest struct {
	Cmd  []string `json:"cmd,omitempty"`
	Env  []string `json:"env,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

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

// HealthResponse reports guest liveness and service readiness.
type HealthResponse struct {
	OK            bool            `json:"ok"`
	PgReady       bool            `json:"pg_ready"`
	PresetsReady  bool            `json:"presets_ready"`
	ServicesReady bool            `json:"services_ready"`
	Services      map[string]bool `json:"services,omitempty"`
	Version       string          `json:"version,omitempty"`
	UptimeSec     int64           `json:"uptime_sec"`
	Hostname      string          `json:"hostname"`
}

// ProbeRequest asks execd to test guest-side network reachability.
type ProbeRequest struct {
	Kind      string `json:"kind"` // "tcp", "udp-dns", "http"
	Addr      string `json:"addr,omitempty"`
	URL       string `json:"url,omitempty"`
	HeaderKey string `json:"header_key,omitempty"`
	HeaderVal string `json:"header_val,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// ProbeResponse reports the outcome of a guest-side network probe.
type ProbeResponse struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

// EntropyResponse exposes per-guest identity so tests can prove clones
// diverged after restore (VMGenID reseed, regenerated machine-id).
type EntropyResponse struct {
	Hex       string `json:"hex"`
	MachineID string `json:"machine_id"`
	BootID    string `json:"boot_id"`
}

// ServiceSpec describes a guest background service supervised by execd.
type ServiceSpec struct {
	Name            string   `json:"name"`
	Preset          string   `json:"preset,omitempty"`
	Cmd             []string `json:"cmd,omitempty"`
	User            string   `json:"user,omitempty"`
	Env             []string `json:"env,omitempty"`
	ReadyTCP        string   `json:"ready_tcp,omitempty"`
	ReadyCmd        []string `json:"ready_cmd,omitempty"`
	CheckpointCmd   []string `json:"checkpoint_cmd,omitempty"`
	ReadyTimeoutSec int      `json:"ready_timeout_sec,omitempty"`
}

// SyncMeta carries delta sync metadata alongside the tar archive.
type SyncMeta struct {
	Mode   string   `json:"mode"`
	Delete []string `json:"delete,omitempty"`
}

// ProviderSpec configures a custom upstream LLM API route in the host gateway.
type ProviderSpec struct {
	Name       string `json:"name"`
	Upstream   string `json:"upstream"`
	APIKeyEnv  string `json:"api_key_env"`
	AuthHeader string `json:"auth_header,omitempty"`
	BaseURLEnv string `json:"base_url_env,omitempty"`
	KeyEnv     string `json:"key_env,omitempty"`
}

// GoldenMeta records the golden snapshot configuration and file manifest
// so later runs inherit settings and sync only deltas and deletions.
type GoldenMeta struct {
	RepoPath    string            `json:"repo_path"`
	HostSHA     string            `json:"host_sha,omitempty"`
	Image       string            `json:"image,omitempty"`
	Dockerfile  string            `json:"dockerfile,omitempty"`
	ImageDigest string            `json:"image_digest,omitempty"`
	MemMB       int64             `json:"mem_mb"`
	VCPUs       int               `json:"vcpus"`
	Database    string            `json:"database,omitempty"`
	Services    []ServiceSpec     `json:"services"`
	Env         []string          `json:"env,omitempty"`
	Providers   []ProviderSpec    `json:"providers,omitempty"`
	GoldenFiles map[string]string `json:"golden_files,omitempty"`
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
