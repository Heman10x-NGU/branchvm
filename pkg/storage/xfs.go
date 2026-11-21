//go:build linux

package storage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Default pool locations.
const (
	DefaultPoolImage = "/var/lib/branchvm/xfs.img"
	DefaultPoolMount = "/var/lib/branchvm/pool"
)

// runCmd executes a command and wraps failures with its stderr.
func runCmd(name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// EnsurePool creates the XFS image (reflink enabled) if missing and
// loop-mounts it at mountPoint if not already mounted. It is idempotent.
func EnsurePool(imgPath, mountPoint string, sizeGB int) error {
	if sizeGB <= 0 {
		return fmt.Errorf("ensure pool: sizeGB must be positive, got %d", sizeGB)
	}
	if _, err := os.Stat(imgPath); errors.Is(err, os.ErrNotExist) {
		if err := createImage(imgPath, sizeGB); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("stat pool image %s: %w", imgPath, err)
	}
	mounted, err := isMounted(mountPoint)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("create mount point %s: %w", mountPoint, err)
	}
	return runCmd("mount", "-o", "loop", imgPath, mountPoint)
}

func createImage(imgPath string, sizeGB int) error {
	if err := os.MkdirAll(filepath.Dir(imgPath), 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	f, err := os.OpenFile(imgPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create pool image %s: %w", imgPath, err)
	}
	truncErr := f.Truncate(int64(sizeGB) << 30)
	closeErr := f.Close()
	if err := errors.Join(truncErr, closeErr); err != nil {
		_ = os.Remove(imgPath)
		return fmt.Errorf("size pool image %s: %w", imgPath, err)
	}
	if err := runCmd("mkfs.xfs", "-m", "reflink=1,crc=1", "-f", imgPath); err != nil {
		_ = os.Remove(imgPath)
		return err
	}
	return nil
}

// isMounted reports whether mountPoint appears in /proc/self/mountinfo.
func isMounted(mountPoint string) (bool, error) {
	clean := filepath.Clean(mountPoint)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("open mountinfo: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescapeMount(fields[4]) == clean {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	return false, nil
}

// unescapeMount decodes the octal escapes (\040 etc.) used in mountinfo.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// IsXFSMount reports whether path is the root of a mounted XFS filesystem.
func IsXFSMount(path string) bool {
	mounted, err := isMounted(path)
	if err != nil || !mounted {
		return false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	const xfsMagic = 0x58465342
	return st.Type == xfsMagic
}

// CloneRootfs makes a reflink copy of base at dst. It fails rather than
// falling back to a full copy when reflink is unsupported.
func CloneRootfs(base, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create clone dir for %s: %w", dst, err)
	}
	return runCmd("cp", "--reflink=always", base, dst)
}

// RemoveClone deletes the clone file and its parent dir when left empty.
func RemoveClone(dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove clone %s: %w", dst, err)
	}
	dir := filepath.Dir(dst)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read clone dir %s: %w", dir, err)
	}
	if len(entries) == 0 {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove clone dir %s: %w", dir, err)
		}
	}
	return nil
}
