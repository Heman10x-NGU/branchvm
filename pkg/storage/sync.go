// Package storage manages the XFS reflink pool used for copy-on-write
// rootfs clones and host-side workspace sync.
package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// gitOutput runs git in repoDir and returns stdout, wrapping stderr on error.
func gitOutput(repoDir string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// DirtyFiles returns modified, added, staged, renamed (new path) and
// untracked non-ignored files, sorted. Deleted paths are skipped.
func DirtyFiles(repoDir string) ([]string, error) {
	out, err := gitOutput(repoDir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	return parsePorcelainZ(out), nil
}

func parsePorcelainZ(out []byte) []string {
	recs := strings.Split(string(out), "\x00")
	seen := map[string]bool{}
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if len(r) < 4 {
			continue
		}
		x, y, path := r[0], r[1], r[3:]
		if x == 'R' || x == 'C' {
			i++ // next record is the original path
		}
		if x == 'D' || y == 'D' || x == '!' {
			continue
		}
		seen[path] = true
	}
	files := make([]string, 0, len(seen))
	for p := range seen {
		files = append(files, p)
	}
	sort.Strings(files)
	return files
}

// PackWorkspace returns a gzip tar of all tracked files plus dirty and
// untracked non-ignored files, with relative paths in sorted order.
func PackWorkspace(repoDir string) ([]byte, error) {
	out, err := gitOutput(repoDir, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	dirty, err := DirtyFiles(repoDir)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	for _, p := range dirty {
		set[p] = true
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return PackFiles(repoDir, paths)
}

// PackFiles writes the given repo-relative regular files into a gzip tar.
// Paths missing on disk (for example tracked but deleted) are skipped.
func PackFiles(repoDir string, paths []string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, rel := range paths {
		if err := addFile(tw, repoDir, rel); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

func addFile(tw *tar.Writer, repoDir, rel string) error {
	full := filepath.Join(repoDir, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rel, err)
		}
		hdr := &tar.Header{Typeflag: tar.TypeSymlink, Name: rel, Linkname: target, Mode: 0o777}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header %s: %w", rel, err)
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     filepath.ToSlash(rel),
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", rel, err)
	}
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("open %s: %w", rel, err)
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("tar write %s: %w", rel, err)
	}
	return nil
}

// ApplyPatch checks then applies a binary-capable git patch in repoDir.
// An empty patch is a no-op.
func ApplyPatch(repoDir string, patch []byte) error {
	if len(bytes.TrimSpace(patch)) == 0 {
		return nil
	}
	tmp, err := os.CreateTemp("", "branchvm-patch-*.diff")
	if err != nil {
		return fmt.Errorf("create patch temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(patch)
	cerr := tmp.Close()
	if werr != nil {
		return fmt.Errorf("write patch: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("close patch: %w", cerr)
	}
	if _, err := gitOutput(repoDir, "apply", "--check", "--binary", tmp.Name()); err != nil {
		return fmt.Errorf("patch check failed: %w", err)
	}
	if _, err := gitOutput(repoDir, "apply", "--binary", "--whitespace=nowarn", tmp.Name()); err != nil {
		return fmt.Errorf("patch apply failed: %w", err)
	}
	return nil
}
