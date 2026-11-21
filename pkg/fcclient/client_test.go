package fcclient

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// macOS caps sun_path at 104 bytes; long t.TempDir paths need a fallback.
const maxSocketPath = 100

type recorded struct {
	method      string
	path        string
	contentType string
	body        string
}

type fakeFirecracker struct {
	mu       sync.Mutex
	requests []recorded
	status   int
	reply    string
}

func (f *fakeFirecracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{
		method:      r.Method,
		path:        r.URL.EscapedPath(),
		contentType: r.Header.Get("Content-Type"),
		body:        string(body),
	})
	status, reply := f.status, f.reply
	f.mu.Unlock()
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

func (f *fakeFirecracker) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("server received no request")
	}
	return f.requests[len(f.requests)-1]
}

func socketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(filepath.Join(dir, "fc.sock")) <= maxSocketPath {
		return dir
	}
	short, err := os.MkdirTemp("", "fcc")
	if err != nil {
		t.Fatalf("mkdir short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	return short
}

func startServer(t *testing.T, status int, reply string) (*Client, *fakeFirecracker) {
	t.Helper()
	sock := filepath.Join(socketDir(t), "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}
	fake := &fakeFirecracker{status: status, reply: reply}
	srv := &http.Server{Handler: fake}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return New(sock), fake
}

func TestClientRequests(t *testing.T) {
	cases := []struct {
		name   string
		call   func(*Client) error
		method string
		path   string
		body   string
	}{
		{
			name: "machine config",
			call: func(c *Client) error {
				return c.PutMachineConfig(MachineConfig{VcpuCount: 2, MemSizeMib: 1024})
			},
			method: http.MethodPut,
			path:   "/machine-config",
			body:   `{"vcpu_count":2,"mem_size_mib":1024,"smt":false,"track_dirty_pages":false}`,
		},
		{
			name: "boot source",
			call: func(c *Client) error {
				return c.PutBootSource(BootSource{
					KernelImagePath: "/var/lib/branchvm/base/vmlinux",
					BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off",
				})
			},
			method: http.MethodPut,
			path:   "/boot-source",
			body:   `{"kernel_image_path":"/var/lib/branchvm/base/vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 pci=off"}`,
		},
		{
			name: "drive",
			call: func(c *Client) error {
				return c.PutDrive(Drive{DriveID: "rootfs", PathOnHost: "rootfs.ext4", IsRootDevice: true})
			},
			method: http.MethodPut,
			path:   "/drives/rootfs",
			body:   `{"drive_id":"rootfs","path_on_host":"rootfs.ext4","is_root_device":true,"is_read_only":false}`,
		},
		{
			name: "network interface",
			call: func(c *Client) error {
				return c.PutNetworkInterface(NetworkInterface{
					IfaceID:     "eth0",
					HostDevName: "tap0",
					GuestMAC:    "06:00:AC:10:00:02",
				})
			},
			method: http.MethodPut,
			path:   "/network-interfaces/eth0",
			body:   `{"iface_id":"eth0","host_dev_name":"tap0","guest_mac":"06:00:AC:10:00:02"}`,
		},
		{
			name:   "start instance",
			call:   func(c *Client) error { return c.StartInstance() },
			method: http.MethodPut,
			path:   "/actions",
			body:   `{"action_type":"InstanceStart"}`,
		},
		{
			name:   "pause",
			call:   func(c *Client) error { return c.PauseVM() },
			method: http.MethodPatch,
			path:   "/vm",
			body:   `{"state":"Paused"}`,
		},
		{
			name:   "resume",
			call:   func(c *Client) error { return c.ResumeVM() },
			method: http.MethodPatch,
			path:   "/vm",
			body:   `{"state":"Resumed"}`,
		},
		{
			name: "create snapshot",
			call: func(c *Client) error {
				return c.CreateSnapshot("snap/vmstate", "snap/mem")
			},
			method: http.MethodPut,
			path:   "/snapshot/create",
			body:   `{"snapshot_type":"Full","snapshot_path":"snap/vmstate","mem_file_path":"snap/mem"}`,
		},
		{
			name: "load snapshot file backend",
			call: func(c *Client) error {
				return c.LoadSnapshot("snap/vmstate", MemBackend{BackendType: BackendFile, BackendPath: "snap/mem"}, true)
			},
			method: http.MethodPut,
			path:   "/snapshot/load",
			body:   `{"snapshot_path":"snap/vmstate","mem_backend":{"backend_type":"File","backend_path":"snap/mem"},"resume_vm":true}`,
		},
		{
			name: "load snapshot uffd backend",
			call: func(c *Client) error {
				return c.LoadSnapshot("snap/vmstate", MemBackend{BackendType: BackendUffd, BackendPath: "/run/bvm/uffd.sock"}, false)
			},
			method: http.MethodPut,
			path:   "/snapshot/load",
			body:   `{"snapshot_path":"snap/vmstate","mem_backend":{"backend_type":"Uffd","backend_path":"/run/bvm/uffd.sock"},"resume_vm":false}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := startServer(t, http.StatusNoContent, "")
			if err := tc.call(client); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			got := fake.last(t)
			if got.method != tc.method {
				t.Errorf("method = %q, want %q", got.method, tc.method)
			}
			if got.path != tc.path {
				t.Errorf("path = %q, want %q", got.path, tc.path)
			}
			if got.contentType != "application/json" {
				t.Errorf("content-type = %q, want application/json", got.contentType)
			}
			if got.body != tc.body {
				t.Errorf("body mismatch\n got: %s\nwant: %s", got.body, tc.body)
			}
		})
	}
}

func TestClientFaultMessage(t *testing.T) {
	const fault = "The requested operation is not supported after starting the microVM."
	client, _ := startServer(t, http.StatusBadRequest, `{"fault_message":"`+fault+`"}`)

	err := client.PutMachineConfig(MachineConfig{VcpuCount: 1, MemSizeMib: 128})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v does not wrap *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.FaultMessage != fault {
		t.Errorf("fault message = %q, want %q", apiErr.FaultMessage, fault)
	}
	if apiErr.Method != http.MethodPut || apiErr.Path != "/machine-config" {
		t.Errorf("error names %s %s, want PUT /machine-config", apiErr.Method, apiErr.Path)
	}
	if !strings.Contains(err.Error(), fault) {
		t.Errorf("error string %q lacks fault message", err.Error())
	}
}

func TestClientNonJSONErrorBody(t *testing.T) {
	client, _ := startServer(t, http.StatusInternalServerError, "boom\n")
	err := client.StartInstance()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v does not wrap *APIError", err)
	}
	if apiErr.FaultMessage != "boom" {
		t.Errorf("fault message = %q, want boom", apiErr.FaultMessage)
	}
}

func TestClientValidation(t *testing.T) {
	client, fake := startServer(t, http.StatusNoContent, "")
	checks := map[string]error{
		"empty drive id": client.PutDrive(Drive{PathOnHost: "rootfs.ext4"}),
		"empty iface id": client.PutNetworkInterface(NetworkInterface{HostDevName: "tap0"}),
		"bad backend":    client.LoadSnapshot("s", MemBackend{BackendType: "Mmap", BackendPath: "m"}, true),
	}
	for name, err := range checks {
		if err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Errorf("invalid calls reached the server: %d requests", len(fake.requests))
	}
}

func TestClientDialError(t *testing.T) {
	client := New(filepath.Join(socketDir(t), "missing.sock"))
	if err := client.PauseVM(); err == nil {
		t.Fatal("expected dial error for missing socket")
	}
}
