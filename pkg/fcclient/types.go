// Package fcclient is a stdlib-only client for the Firecracker HTTP API
// served on the VMM's Unix domain socket.
package fcclient

// MachineConfig is the body of PUT /machine-config.
type MachineConfig struct {
	VcpuCount       int   `json:"vcpu_count"`
	MemSizeMib      int64 `json:"mem_size_mib"`
	Smt             bool  `json:"smt"`
	TrackDirtyPages bool  `json:"track_dirty_pages"`
}

// BootSource is the body of PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// Drive is the body of PUT /drives/{drive_id}.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface is the body of PUT /network-interfaces/{iface_id}.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// InstanceAction is the body of PUT /actions.
type InstanceAction struct {
	ActionType string `json:"action_type"`
}

// VMState is the body of PATCH /vm.
type VMState struct {
	State string `json:"state"`
}

// SnapshotCreateParams is the body of PUT /snapshot/create.
type SnapshotCreateParams struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// MemBackend tells Firecracker where guest memory comes from on restore.
// BackendPath is the memory file for File and the pager's UDS for Uffd.
type MemBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// SnapshotLoadParams is the body of PUT /snapshot/load.
// EnableDiffSnapshots is omitted when false: Firecracker deprecated the
// field in favor of track_dirty_pages and rejects unknown fields, while an
// absent field means false on every version.
type SnapshotLoadParams struct {
	SnapshotPath        string     `json:"snapshot_path"`
	MemBackend          MemBackend `json:"mem_backend"`
	EnableDiffSnapshots bool       `json:"enable_diff_snapshots,omitempty"`
	ResumeVM            bool       `json:"resume_vm"`
}

// faultResponse is the error body Firecracker returns on non-2xx replies.
type faultResponse struct {
	FaultMessage string `json:"fault_message"`
}

// Firecracker enum values used in request bodies.
const (
	ActionInstanceStart = "InstanceStart"
	StatePaused         = "Paused"
	StateResumed        = "Resumed"
	SnapshotTypeFull    = "Full"
	BackendFile         = "File"
	BackendUffd         = "Uffd"
)
