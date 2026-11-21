package fcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// The host part is ignored; every request is dialed to the socket.
	baseURL        = "http://firecracker"
	requestTimeout = 2 * time.Minute
	maxErrorBody   = 64 << 10
)

// APIError is returned when Firecracker answers with a non-2xx status.
type APIError struct {
	Method       string
	Path         string
	StatusCode   int
	FaultMessage string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecracker %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.FaultMessage)
}

// Client talks to one Firecracker process over its API socket.
type Client struct {
	socketPath string
	http       *http.Client
}

// New returns a Client for the Firecracker API socket at socketPath.
func New(socketPath string) *Client {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &Client{
		socketPath: socketPath,
		http:       &http.Client{Transport: transport, Timeout: requestTimeout},
	}
}

// PutMachineConfig sets vCPU count and memory size before boot.
func (c *Client) PutMachineConfig(cfg MachineConfig) error {
	return c.do(http.MethodPut, "/machine-config", cfg)
}

// PutBootSource sets the kernel image and boot arguments.
func (c *Client) PutBootSource(src BootSource) error {
	return c.do(http.MethodPut, "/boot-source", src)
}

// PutDrive attaches a block device.
func (c *Client) PutDrive(d Drive) error {
	if d.DriveID == "" {
		return errors.New("fcclient: put drive: empty drive_id")
	}
	return c.do(http.MethodPut, "/drives/"+url.PathEscape(d.DriveID), d)
}

// PutNetworkInterface attaches a tap-backed virtio-net device.
func (c *Client) PutNetworkInterface(iface NetworkInterface) error {
	if iface.IfaceID == "" {
		return errors.New("fcclient: put network interface: empty iface_id")
	}
	return c.do(http.MethodPut, "/network-interfaces/"+url.PathEscape(iface.IfaceID), iface)
}

// StartInstance boots the configured microVM.
func (c *Client) StartInstance() error {
	return c.do(http.MethodPut, "/actions", InstanceAction{ActionType: ActionInstanceStart})
}

// PauseVM pauses all vCPUs.
func (c *Client) PauseVM() error {
	return c.do(http.MethodPatch, "/vm", VMState{State: StatePaused})
}

// ResumeVM resumes a paused microVM.
func (c *Client) ResumeVM() error {
	return c.do(http.MethodPatch, "/vm", VMState{State: StateResumed})
}

// CreateSnapshot writes a full snapshot of a paused microVM.
func (c *Client) CreateSnapshot(snapshotPath, memFilePath string) error {
	return c.do(http.MethodPut, "/snapshot/create", SnapshotCreateParams{
		SnapshotType: SnapshotTypeFull,
		SnapshotPath: snapshotPath,
		MemFilePath:  memFilePath,
	})
}

// LoadSnapshot restores a snapshot into a fresh Firecracker process.
func (c *Client) LoadSnapshot(snapshotPath string, memBackend MemBackend, resume bool) error {
	if memBackend.BackendType != BackendFile && memBackend.BackendType != BackendUffd {
		return fmt.Errorf("fcclient: load snapshot: unsupported backend_type %q", memBackend.BackendType)
	}
	return c.do(http.MethodPut, "/snapshot/load", SnapshotLoadParams{
		SnapshotPath: snapshotPath,
		MemBackend:   memBackend,
		ResumeVM:     resume,
	})
}

func (c *Client) do(method, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("fcclient: encode %s %s: %w", method, path, err)
	}
	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("fcclient: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcclient: %s %s via %s: %w", method, path, c.socketPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return fmt.Errorf("fcclient: %w", newAPIError(method, path, resp))
}

func newAPIError(method, path string, resp *http.Response) *APIError {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := strings.TrimSpace(string(raw))
	var fault faultResponse
	if json.Unmarshal(raw, &fault) == nil && fault.FaultMessage != "" {
		msg = fault.FaultMessage
	}
	if err != nil && msg == "" {
		msg = fmt.Sprintf("read error body: %v", err)
	}
	return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, FaultMessage: msg}
}
