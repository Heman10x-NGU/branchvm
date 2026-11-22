// Package storage manages the XFS reflink pool used for copy-on-write
// rootfs clones and host-side workspace sync.
package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
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

func repoLocation(repoDir string) (top, prefix string, err error) {
	out, err := gitOutput(repoDir, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return repoDir, "", nil
	}
	top = lines[0]
	if len(lines) > 1 {
		prefix = lines[1]
	}
	return top, prefix, nil
}

func stripRepoPrefix(path, prefix string) (string, bool) {
	if prefix == "" {
		return path, path != ""
	}
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(path, prefix)
	return rel, rel != ""
}

// DirtyFiles returns modified, added, staged, renamed (new path) and
// untracked non-ignored files, sorted. Deleted paths are skipped.
func DirtyFiles(repoDir string) ([]string, error) {
	_, prefix, err := repoLocation(repoDir)
	if err != nil {
		return nil, err
	}
	out, err := gitOutput(repoDir, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	if err != nil {
		return nil, err
	}
	return parsePorcelainZ(out, prefix), nil
}

func parsePorcelainZ(out []byte, prefix string) []string {
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
		rel, ok := stripRepoPrefix(path, prefix)
		if !ok {
			continue
		}
		seen[rel] = true
	}
	files := make([]string, 0, len(seen))
	for p := range seen {
		files = append(files, p)
	}
	sort.Strings(files)
	return files
}

// WorkspaceManifest computes the current HEAD SHA and a deterministic content
// signature map for all tracked and untracked non-ignored files in repoDir.
func WorkspaceManifest(repoDir string) (string, map[string]string, error) {
	_, prefix, err := repoLocation(repoDir)
	if err != nil {
		return "", nil, err
	}

	var hostSHA string
	if out, err := gitOutput(repoDir, "rev-parse", "HEAD"); err == nil {
		hostSHA = strings.TrimSpace(string(out))
	}

	idxOut, err := gitOutput(repoDir, "ls-files", "-s", "-z")
	if err != nil {
		return "", nil, err
	}
	files := map[string]string{}
	for _, rec := range strings.Split(string(idxOut), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok || path == "" {
			continue
		}
		parts := strings.Fields(meta)
		if len(parts) < 2 || parts[0] == "160000" {
			continue
		}
		files[path] = "git:" + parts[1] + ":" + parts[0]
	}

	stOut, err := gitOutput(repoDir, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	if err != nil {
		return "", nil, err
	}
	recs := strings.Split(string(stOut), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if len(r) < 4 {
			continue
		}
		x, y, path := r[0], r[1], r[3:]
		if x == 'R' || x == 'C' {
			if i+1 < len(recs) {
				if x == 'R' {
					if oldRel, ok := stripRepoPrefix(recs[i+1], prefix); ok {
						delete(files, oldRel)
					}
				}
				i++
			}
		}
		if x == '!' {
			continue
		}
		rel, ok := stripRepoPrefix(path, prefix)
		if !ok {
			continue
		}
		if x == 'D' || y == 'D' {
			delete(files, rel)
			continue
		}
		sig, present, err := diskEntrySignature(repoDir, rel)
		if err != nil {
			return "", nil, err
		}
		if !present {
			delete(files, rel)
			continue
		}
		files[rel] = sig
	}
	return hostSHA, files, nil
}

func diskEntrySignature(repoDir, rel string) (string, bool, error) {
	full := filepath.Join(repoDir, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lstat %s: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return "", false, fmt.Errorf("readlink %s: %w", rel, err)
		}
		return "link:" + target, true, nil
	}
	if !info.Mode().IsRegular() {
		return "", false, nil
	}
	f, err := os.Open(full)
	if err != nil {
		return "", false, fmt.Errorf("open %s: %w", rel, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false, fmt.Errorf("hash %s: %w", rel, err)
	}
	return fmt.Sprintf("sha:%s:%04o", hex.EncodeToString(h.Sum(nil)), info.Mode().Perm()), true, nil
}

// DiffManifests returns sorted paths that were added/modified and paths that
// were deleted between golden and current manifests.
func DiffManifests(golden, current map[string]string) (changed []string, deleted []string) {
	for path, curSig := range current {
		if golden[path] != curSig {
			changed = append(changed, path)
		}
	}
	for path := range golden {
		if _, ok := current[path]; !ok {
			deleted = append(deleted, path)
		}
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	return changed, deleted
}

// EncodeSyncMultipart serializes sync metadata and an optional gzip tar archive
// as a multipart/form-data request body.
func EncodeSyncMultipart(meta types.SyncMeta, archive []byte) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mp, err := mw.CreateFormField("meta")
	if err != nil {
		return nil, "", fmt.Errorf("create meta field: %w", err)
	}
	if err := json.NewEncoder(mp).Encode(meta); err != nil {
		return nil, "", fmt.Errorf("encode sync meta: %w", err)
	}
	if len(archive) > 0 {
		ap, err := mw.CreateFormFile("archive", "workspace.tar.gz")
		if err != nil {
			return nil, "", fmt.Errorf("create archive field: %w", err)
		}
		if _, err := ap.Write(archive); err != nil {
			return nil, "", fmt.Errorf("write archive field: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
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
	applyDir := repoDir
	var dirArgs []string
	if top, prefix, err := repoLocation(repoDir); err == nil && prefix != "" {
		applyDir = top
		dirArgs = []string{"--directory=" + strings.TrimSuffix(prefix, "/")}
	}
	if _, err := gitOutput(applyDir, append(append([]string{"apply", "--check", "--binary"}, dirArgs...), tmp.Name())...); err != nil {
		return fmt.Errorf("patch check failed: %w", err)
	}
	if _, err := gitOutput(applyDir, append(append([]string{"apply", "--binary", "--whitespace=nowarn"}, dirArgs...), tmp.Name())...); err != nil {
		return fmt.Errorf("patch apply failed: %w", err)
	}
	return nil
}
