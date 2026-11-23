// Package rootfs converts OCI/Docker images or local Dockerfiles into
// ext4 root filesystems injected with static execd and branchvm metadata.
package rootfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

// DefaultCacheDir holds cached image-derived rootfs.ext4 files on the host.
const DefaultCacheDir = "/var/lib/branchvm/images"

const (
	defaultExecdAsset = "/var/lib/branchvm/assets/execd"
	minRootfsMB       = int64(1536)
	headroomMB        = int64(512)
	roundStepMB       = int64(64)
	minInodes         = int64(65536)
	inodeMargin       = int64(32768)
	maxSymlinkHops    = 40
	tarExitTimeoutErr = "tar extract"
)

// BuildOptions configures BuildFromImage.
type BuildOptions struct {
	Image        string
	Dockerfile   string
	RepoDir      string
	ExecdPath    string
	Services     []types.ServiceSpec
	RootfsSizeMB int64
	CacheDir     string
	Out          io.Writer
}

// BuildResult describes the built or cached ext4 rootfs image.
type BuildResult struct {
	RootfsPath  string
	ImageDigest string
	Cached      bool
	SizeMB      int64
}

// FindExecd locates and validates a static Linux execd ELF matching the
// current host architecture.
func FindExecd(explicit string) (string, error) {
	if explicit != "" {
		if err := ValidateExecdELF(explicit, runtime.GOARCH); err != nil {
			return "", fmt.Errorf("invalid --execd %s: %w", explicit, err)
		}
		return explicit, nil
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "execd"))
	}
	candidates = append(candidates, defaultExecdAsset, filepath.Join("bin", "execd"))

	var errs []string
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		if err := ValidateExecdELF(c, runtime.GOARCH); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", c, err))
			continue
		}
		return c, nil
	}
	if len(errs) > 0 {
		return "", fmt.Errorf("no valid static linux/%s execd found (%s)", runtime.GOARCH, strings.Join(errs, "; "))
	}
	return "", fmt.Errorf("static linux/%s execd binary not found (run `make build-guest` or pass --execd)", runtime.GOARCH)
}

// ValidateExecdELF verifies path is a 64-bit static Linux ELF for goarch.
func ValidateExecdELF(path, goarch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("open ELF %s: %w", path, err)
	}
	defer f.Close()

	if f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("expected 64-bit ELF, got %s", f.Class)
	}
	var wantMachine elf.Machine
	switch goarch {
	case "amd64":
		wantMachine = elf.EM_X86_64
	case "arm64":
		wantMachine = elf.EM_AARCH64
	default:
		return fmt.Errorf("unsupported target architecture %q", goarch)
	}
	if f.Machine != wantMachine {
		return fmt.Errorf("ELF arch %s does not match %s (%s)", f.Machine, goarch, wantMachine)
	}
	for _, ph := range f.Progs {
		if ph.Type == elf.PT_INTERP {
			return errors.New("execd is dynamically linked (must build with CGO_ENABLED=0)")
		}
	}
	return nil
}

type imageInspect struct {
	ID           string `json:"Id"`
	Architecture string `json:"Architecture"`
	Config       struct {
		Env []string `json:"Env"`
	} `json:"Config"`
}

// BuildFromImage builds (or reuses a cached) Firecracker ext4 rootfs from
// a Docker image or local Dockerfile.
func BuildFromImage(ctx context.Context, opt BuildOptions) (BuildResult, error) {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.CacheDir == "" {
		opt.CacheDir = DefaultCacheDir
	}
	if opt.Image == "" && opt.Dockerfile == "" {
		return BuildResult{}, errors.New("rootfs build requires either Image or Dockerfile")
	}
	if opt.Image != "" && opt.Dockerfile != "" {
		return BuildResult{}, errors.New("cannot specify both Image and Dockerfile")
	}

	execdPath, err := FindExecd(opt.ExecdPath)
	if err != nil {
		return BuildResult{}, err
	}
	execdHash, err := sha256File(execdPath)
	if err != nil {
		return BuildResult{}, err
	}
	if err := checkDocker(ctx); err != nil {
		return BuildResult{}, err
	}

	imageRef := opt.Image
	if opt.Dockerfile != "" {
		imageRef, err = buildLocalDockerfile(ctx, opt.RepoDir, opt.Dockerfile, opt.Out)
		if err != nil {
			return BuildResult{}, err
		}
	}

	info, err := inspectOrPullImage(ctx, imageRef, opt.Dockerfile == "", opt.Out)
	if err != nil {
		return BuildResult{}, err
	}
	arch := normArch(info.Architecture)
	if arch != runtime.GOARCH {
		return BuildResult{}, fmt.Errorf("image %s has architecture %q, host needs linux/%s (rebuild or pull with --platform linux/%s)",
			imageRef, info.Architecture, runtime.GOARCH, runtime.GOARCH)
	}

	key, err := CacheKey(info.ID, execdHash, opt.Services, opt.RootfsSizeMB)
	if err != nil {
		return BuildResult{}, err
	}
	outDir := filepath.Join(opt.CacheDir, key)
	finalPath := filepath.Join(outDir, "rootfs.ext4")
	if fi, err := os.Stat(finalPath); err == nil && fi.Size() > 0 {
		fmt.Fprintf(opt.Out, "rootfs cache hit: %s (%d MiB)\n", finalPath, fi.Size()>>20)
		return BuildResult{
			RootfsPath:  finalPath,
			ImageDigest: info.ID,
			Cached:      true,
			SizeMB:      fi.Size() >> 20,
		}, nil
	}

	if err := os.MkdirAll(opt.CacheDir, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create cache dir %s: %w", opt.CacheDir, err)
	}
	staging, err := os.MkdirTemp(opt.CacheDir, ".staging-*")
	if err != nil {
		return BuildResult{}, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	if err := exportImageToDir(ctx, imageRef, staging, opt.Out); err != nil {
		return BuildResult{}, err
	}
	if err := validateGuestBins(staging, info.Config.Env); err != nil {
		return BuildResult{}, err
	}
	if err := stageGuestOverlay(staging, execdPath, opt.Services, info.Config.Env); err != nil {
		return BuildResult{}, err
	}

	bytesTotal, entries, err := measureDir(staging)
	if err != nil {
		return BuildResult{}, fmt.Errorf("measure staging rootfs: %w", err)
	}
	sizeMB, inodes := ComputeExt4Sizing(bytesTotal, entries, opt.RootfsSizeMB)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create cache dir %s: %w", outDir, err)
	}
	pf, err := os.CreateTemp(outDir, "rootfs.ext4.partial-*")
	if err != nil {
		return BuildResult{}, fmt.Errorf("create temp rootfs %s: %w", outDir, err)
	}
	partial := pf.Name()
	_ = pf.Close()
	defer os.Remove(partial)
	if err := buildExt4FromDir(ctx, staging, partial, sizeMB, inodes, opt.Out); err != nil {
		return BuildResult{}, err
	}
	if err := os.Rename(partial, finalPath); err != nil {
		return BuildResult{}, fmt.Errorf("install rootfs image: %w", err)
	}
	return BuildResult{
		RootfsPath:  finalPath,
		ImageDigest: info.ID,
		Cached:      false,
		SizeMB:      sizeMB,
	}, nil
}

// ResolveDockerfile verifies dockerfile resolves inside repoDir and exists.
func ResolveDockerfile(repoDir, dockerfile string) (string, string, error) {
	if repoDir == "" {
		repoDir = "."
	}
	absRepo, err := filepath.Abs(repoDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve repo dir: %w", err)
	}
	if dockerfile == "" {
		return "", "", errors.New("dockerfile path is empty")
	}
	dfPath := dockerfile
	if !filepath.IsAbs(dfPath) {
		dfPath = filepath.Join(absRepo, dockerfile)
	}
	dfAbs, err := filepath.Abs(dfPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve dockerfile: %w", err)
	}
	rel, err := filepath.Rel(absRepo, dfAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("dockerfile %q escapes repo directory %s", dockerfile, absRepo)
	}
	fi, err := os.Stat(dfAbs)
	if err != nil {
		return "", "", fmt.Errorf("stat dockerfile %s: %w", dfAbs, err)
	}
	if fi.IsDir() {
		return "", "", fmt.Errorf("dockerfile %s is a directory", dfAbs)
	}
	realRepo, err := filepath.EvalSymlinks(absRepo)
	if err != nil {
		return "", "", fmt.Errorf("resolve real repo dir: %w", err)
	}
	realDF, err := filepath.EvalSymlinks(dfAbs)
	if err != nil {
		return "", "", fmt.Errorf("resolve real dockerfile: %w", err)
	}
	realRel, err := filepath.Rel(realRepo, realDF)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("dockerfile %q resolves outside repo directory %s", dockerfile, absRepo)
	}
	return absRepo, dfAbs, nil
}

// CacheKey computes the deterministic content hash for a cached ext4 build.
func CacheKey(imageID, execdSHA string, services []types.ServiceSpec, rootfsSizeMB int64) (string, error) {
	if services == nil {
		services = []types.ServiceSpec{}
	}
	svcJSON, err := json.Marshal(services)
	if err != nil {
		return "", fmt.Errorf("marshal services: %w", err)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n%d\n", imageID, execdSHA, svcJSON, rootfsSizeMB)
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}

// ComputeExt4Sizing computes ext4 image MiB size and inode count from
// unpacked staging byte and entry totals.
func ComputeExt4Sizing(bytesTotal, entries, requestedSizeMB int64) (sizeMB, inodes int64) {
	inodes = entries*2 + inodeMargin
	if inodes < minInodes {
		inodes = minInodes
	}
	needBytes := (bytesTotal*135)/100 + inodes*256 + (64 << 20) + (headroomMB << 20)
	needMB := (needBytes + (1 << 20) - 1) >> 20
	if needMB < minRootfsMB {
		needMB = minRootfsMB
	}
	sizeMB = ((needMB + roundStepMB - 1) / roundStepMB) * roundStepMB
	if requestedSizeMB > sizeMB {
		sizeMB = requestedSizeMB
	}
	return sizeMB, inodes
}

func checkDocker(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not found in PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, "docker", "info")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker daemon unavailable: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func buildLocalDockerfile(ctx context.Context, repoDir, dockerfile string, out io.Writer) (string, error) {
	absRepo, dfAbs, err := ResolveDockerfile(repoDir, dockerfile)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(absRepo + ":" + dfAbs))
	tag := "branchvm-local-" + hex.EncodeToString(h[:])[:12]
	platform := "linux/" + runtime.GOARCH
	fmt.Fprintf(out, "building %s from %s (%s)\n", tag, dockerfile, platform)
	cmd := exec.CommandContext(ctx, "docker", "build", "--platform", platform, "-t", tag, "-f", dfAbs, absRepo)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build %s: %w", dockerfile, err)
	}
	return tag, nil
}

func inspectOrPullImage(ctx context.Context, image string, allowPull bool, out io.Writer) (imageInspect, error) {
	info, err := inspectImage(ctx, image)
	if err == nil {
		return info, nil
	}
	if !allowPull {
		return imageInspect{}, err
	}
	platform := "linux/" + runtime.GOARCH
	fmt.Fprintf(out, "pulling %s (%s)\n", image, platform)
	pull := exec.CommandContext(ctx, "docker", "pull", "--platform", platform, image)
	pull.Stdout = out
	pull.Stderr = out
	if perr := pull.Run(); perr != nil {
		return imageInspect{}, fmt.Errorf("docker pull %s: %w", image, perr)
	}
	return inspectImage(ctx, image)
}

func inspectImage(ctx context.Context, image string) (imageInspect, error) {
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .}}", image)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return imageInspect{}, fmt.Errorf("docker image inspect %s: %w: %s", image, err, strings.TrimSpace(stderr.String()))
	}
	var info imageInspect
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		return imageInspect{}, fmt.Errorf("decode docker inspect for %s: %w", image, err)
	}
	return info, nil
}

func exportImageToDir(ctx context.Context, image, staging string, out io.Writer) error {
	fmt.Fprintf(out, "exporting %s into ext4 staging dir\n", image)
	var cidBuf, cErr bytes.Buffer
	create := exec.CommandContext(ctx, "docker", "create", "--platform", "linux/"+runtime.GOARCH, image, "/bin/sh")
	create.Stdout = &cidBuf
	create.Stderr = &cErr
	if err := create.Run(); err != nil {
		return fmt.Errorf("docker create %s: %w: %s", image, err, strings.TrimSpace(cErr.String()))
	}
	cid := strings.TrimSpace(cidBuf.String())
	if cid == "" {
		return fmt.Errorf("docker create %s returned empty container id", image)
	}
	defer func() {
		rm := exec.CommandContext(context.Background(), "docker", "rm", "-f", cid)
		_ = rm.Run()
	}()

	exp := exec.CommandContext(ctx, "docker", "export", cid)
	pipe, err := exp.StdoutPipe()
	if err != nil {
		return fmt.Errorf("docker export pipe: %w", err)
	}
	var expErr bytes.Buffer
	exp.Stderr = &expErr
	if err := exp.Start(); err != nil {
		return fmt.Errorf("start docker export: %w", err)
	}

	untarErr := untarPreservingRoot(ctx, pipe, staging)
	expWaitErr := exp.Wait()
	if untarErr != nil {
		return fmt.Errorf("%s: %w", tarExitTimeoutErr, untarErr)
	}
	if expWaitErr != nil {
		return fmt.Errorf("docker export %s: %w: %s", image, expWaitErr, strings.TrimSpace(expErr.String()))
	}
	return nil
}

// untarPreservingRoot invokes GNU tar to preserve numeric uid/gid, device
// nodes, hardlinks, and permissions while avoiding Go archive/tar symlink bugs.
func untarPreservingRoot(ctx context.Context, r io.Reader, staging string) error {
	tarBin, err := exec.LookPath("tar")
	if err != nil {
		return fmt.Errorf("tar binary not found in PATH (required to extract rootfs safely): %w", err)
	}
	cmd := exec.CommandContext(ctx, tarBin, "-xpf", "-", "--numeric-owner", "--same-permissions", "-C", staging)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func validateGuestBins(staging string, imageEnv []string) error {
	if _, ok := resolveGuestBin(staging, "/bin/sh"); !ok {
		return errors.New("guest image is missing /bin/sh; add a POSIX shell to your Dockerfile")
	}
	dirs := []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	for _, kv := range imageEnv {
		if pathVal, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, d := range strings.Split(pathVal, ":") {
				if d != "" {
					dirs = append(dirs, d)
				}
			}
		}
	}
	for _, d := range dirs {
		if _, ok := resolveGuestBin(staging, filepath.ToSlash(filepath.Join(d, "git"))); ok {
			return nil
		}
	}
	return errors.New("guest image is missing `git` (required for workspace diff/sync); install git in your Dockerfile")
}

func resolveGuestBin(staging, guestPath string) (string, bool) {
	resolved, err := resolveInsideStaging(staging, guestPath, true)
	if err != nil {
		return "", false
	}
	fi, err := os.Stat(resolved)
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return "", false
	}
	return resolved, true
}

func stageGuestOverlay(staging, execdPath string, services []types.ServiceSpec, imageEnv []string) error {
	for _, d := range []struct {
		path string
		mode fs.FileMode
	}{
		{"/proc", 0o755},
		{"/sys", 0o755},
		{"/dev", 0o755},
		{"/dev/pts", 0o755},
		{"/dev/shm", 0o1777},
		{"/run", 0o755},
		{"/tmp", 0o1777},
		{"/workspace", 0o755},
		{"/etc/branchvm", 0o755},
		{"/root", 0o700},
	} {
		if err := safeMkdir(staging, d.path, d.mode); err != nil {
			return err
		}
	}

	execdData, err := os.ReadFile(execdPath)
	if err != nil {
		return fmt.Errorf("read execd %s: %w", execdPath, err)
	}
	if err := safeWriteFile(staging, "/sbin/init", execdData, 0o755); err != nil {
		return err
	}

	if services == nil {
		services = []types.ServiceSpec{}
	}
	svcBytes, err := json.MarshalIndent(services, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal services.json: %w", err)
	}
	if err := safeWriteFile(staging, "/etc/branchvm/services.json", append(svcBytes, '\n'), 0o644); err != nil {
		return err
	}

	if imageEnv == nil {
		imageEnv = []string{}
	}
	envBytes, err := json.MarshalIndent(imageEnv, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image-env.json: %w", err)
	}
	if err := safeWriteFile(staging, "/etc/branchvm/image-env.json", append(envBytes, '\n'), 0o644); err != nil {
		return err
	}

	if err := safeWriteFile(staging, "/etc/hostname", []byte("branchvm\n"), 0o644); err != nil {
		return err
	}
	hosts := "127.0.0.1 localhost\n127.0.1.1 branchvm\n::1 localhost ip6-localhost ip6-loopback\n"
	if err := safeWriteFile(staging, "/etc/hosts", []byte(hosts), 0o644); err != nil {
		return err
	}
	if err := safeWriteFile(staging, "/etc/resolv.conf", []byte(""), 0o644); err != nil {
		return err
	}
	if err := safeWriteFile(staging, "/etc/machine-id", []byte(""), 0o444); err != nil {
		return err
	}
	for _, rm := range []string{"/var/lib/dbus/machine-id", "/var/lib/systemd/random-seed"} {
		if err := safeRemovePath(staging, rm); err != nil {
			return err
		}
	}
	if !safePathExists(staging, "/root/.claude.json") {
		if err := safeWriteFile(staging, "/root/.claude.json", []byte("{\"hasCompletedOnboarding\":true}\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// InjectBaseOverlay loop-mounts an existing ext4 rootfs image and installs
// the validated static execd binary at /sbin/init along with services.json.
func InjectBaseOverlay(ctx context.Context, rootfsPath, execdPath string, services []types.ServiceSpec) (retErr error) {
	if err := ValidateExecdELF(execdPath, runtime.GOARCH); err != nil {
		return fmt.Errorf("invalid execd %s: %w", execdPath, err)
	}
	execdData, err := os.ReadFile(execdPath)
	if err != nil {
		return fmt.Errorf("read execd %s: %w", execdPath, err)
	}
	if services == nil {
		services = []types.ServiceSpec{}
	}
	svcBytes, err := json.MarshalIndent(services, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal services.json: %w", err)
	}

	mnt, err := os.MkdirTemp("", "branchvm-overlay-*")
	if err != nil {
		return fmt.Errorf("create overlay mountpoint: %w", err)
	}
	mounted := false
	defer func() {
		if !mounted {
			_ = os.Remove(mnt)
			return
		}
		var umountErr bytes.Buffer
		umountCmd := exec.Command("umount", mnt)
		umountCmd.Stderr = &umountErr
		if err := umountCmd.Run(); err != nil {
			var lazyErr bytes.Buffer
			lazyCmd := exec.Command("umount", "-l", mnt)
			lazyCmd.Stderr = &lazyErr
			if lerr := lazyCmd.Run(); lerr != nil {
				if retErr == nil {
					retErr = fmt.Errorf("umount %s: %v (%s); umount -l: %w (%s)",
						mnt, err, strings.TrimSpace(umountErr.String()), lerr, strings.TrimSpace(lazyErr.String()))
				}
				return
			}
		}
		_ = os.Remove(mnt)
	}()

	var mountErr bytes.Buffer
	mountCmd := exec.CommandContext(ctx, "mount", "-o", "loop", rootfsPath, mnt)
	mountCmd.Stderr = &mountErr
	if err := mountCmd.Run(); err != nil {
		return fmt.Errorf("loop-mount %s: %w: %s", rootfsPath, err, strings.TrimSpace(mountErr.String()))
	}
	mounted = true

	if err := safeMkdir(mnt, "/etc/branchvm", 0o755); err != nil {
		return err
	}
	if err := safeWriteFile(mnt, "/sbin/init", execdData, 0o755); err != nil {
		return err
	}
	if err := safeWriteFile(mnt, "/etc/branchvm/services.json", append(svcBytes, '\n'), 0o644); err != nil {
		return err
	}
	return nil
}

// safeStagePath resolves guestPath inside staging without ever following a
// symlink outside staging, creating intermediate directories as needed and
// unlinking any existing symlink at the leaf before returning its host path.
func safeStagePath(staging, guestPath string) (string, error) {
	p, err := resolveInsideStaging(staging, guestPath, false)
	if err != nil {
		return "", err
	}
	return p, nil
}

func safeMkdir(staging, guestDir string, perm fs.FileMode) error {
	p, err := resolveInsideStaging(staging, guestDir, false)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err == nil {
		if fi.IsDir() {
			return os.Chmod(p, perm)
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("replace non-dir %s: %w", guestDir, err)
		}
	}
	if err := os.MkdirAll(p, perm); err != nil {
		return fmt.Errorf("mkdir %s: %w", guestDir, err)
	}
	return os.Chmod(p, perm)
}

func safeWriteFile(staging, guestPath string, data []byte, perm fs.FileMode) error {
	p, err := safeStagePath(staging, guestPath)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing %s: %w", guestPath, err)
	}
	if err := os.WriteFile(p, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", guestPath, err)
	}
	return os.Chmod(p, perm)
}

func safeRemovePath(staging, guestPath string) error {
	p, err := resolveInsideStaging(staging, guestPath, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", guestPath, err)
	}
	return nil
}

func safePathExists(staging, guestPath string) bool {
	p, err := resolveInsideStaging(staging, guestPath, true)
	if err != nil {
		return false
	}
	_, err = os.Lstat(p)
	return err == nil
}

// resolveInsideStaging walks guestPath component by component strictly under
// staging using os.Lstat. Intermediate symlinks resolve relative to staging
// so absolute targets like /usr/sbin stay inside staging.
func resolveInsideStaging(staging, guestPath string, followLeaf bool) (string, error) {
	absRoot, err := filepath.Abs(staging)
	if err != nil {
		return "", err
	}
	parts := splitGuestParts(guestPath)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty guest path %q", guestPath)
	}

	var cur []string
	hops := 0
	for len(parts) > 0 {
		seg := parts[0]
		parts = parts[1:]
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if len(cur) > 0 {
				cur = cur[:len(cur)-1]
			}
			continue
		}
		hostPath := filepath.Join(absRoot, filepath.Join(append(cur, seg)...))
		fi, err := os.Lstat(hostPath)
		isLeaf := len(parts) == 0
		if errors.Is(err, os.ErrNotExist) {
			if followLeaf {
				return "", os.ErrNotExist
			}
			if !isLeaf {
				if mkErr := os.Mkdir(hostPath, 0o755); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
					return "", fmt.Errorf("mkdir staging %s: %w", hostPath, mkErr)
				}
			}
			cur = append(cur, seg)
			continue
		}
		if err != nil {
			return "", fmt.Errorf("lstat %s: %w", hostPath, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 && (!isLeaf || followLeaf) {
			hops++
			if hops > maxSymlinkHops {
				return "", fmt.Errorf("symlink loop resolving %s", guestPath)
			}
			target, err := os.Readlink(hostPath)
			if err != nil {
				return "", fmt.Errorf("readlink %s: %w", hostPath, err)
			}
			var next []string
			if strings.HasPrefix(target, "/") {
				cur = nil
				next = splitGuestParts(target)
			} else {
				next = splitGuestParts(target)
			}
			parts = append(next, parts...)
			continue
		}
		cur = append(cur, seg)
	}
	if len(cur) == 0 {
		return "", fmt.Errorf("guest path %q resolves to staging root", guestPath)
	}
	return filepath.Join(absRoot, filepath.Join(cur...)), nil
}

func splitGuestParts(p string) []string {
	var out []string
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

func measureDir(root string) (bytesTotal, entries int64, err error) {
	seenInodes := map[uint64]struct{}{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		entries++
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if ino, ok := inodeKey(info); ok {
				if _, dup := seenInodes[ino]; dup {
					return nil
				}
				seenInodes[ino] = struct{}{}
			}
			bytesTotal += ((info.Size() + 4095) / 4096) * 4096
		} else if info.IsDir() {
			bytesTotal += 4096
		}
		return nil
	})
	return bytesTotal, entries, err
}

func buildExt4FromDir(ctx context.Context, staging, dst string, sizeMB, inodes int64, out io.Writer) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create ext4 image %s: %w", dst, err)
	}
	terr := f.Truncate(sizeMB << 20)
	cerr := f.Close()
	if err := errors.Join(terr, cerr); err != nil {
		return fmt.Errorf("truncate ext4 image %s: %w", dst, err)
	}

	fmt.Fprintf(out, "creating %d MiB ext4 (%d inodes)\n", sizeMB, inodes)
	mkfs := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", "-L", "branchvm-root",
		"-N", strconv.FormatInt(inodes, 10), "-d", staging, dst)
	var mkErr bytes.Buffer
	mkfs.Stderr = &mkErr
	if err := mkfs.Run(); err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, strings.TrimSpace(mkErr.String()))
	}

	fsck := exec.CommandContext(ctx, "e2fsck", "-f", "-y", dst)
	var fsckOut bytes.Buffer
	fsck.Stdout = &fsckOut
	fsck.Stderr = &fsckOut
	if err := fsck.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() > 1 {
			return fmt.Errorf("e2fsck %s: %w: %s", dst, err, strings.TrimSpace(fsckOut.String()))
		}
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("sha256 %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sha256 %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func normArch(a string) string {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(a))
	}
}
