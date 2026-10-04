package setup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Installation contains derived paths to prepared files. Preparation does not
// prove runtime readiness; the Agent verifies readiness when it starts.
type Installation struct{ RuntimeBinary, ModelPath, Backend, Version string }

// Phase describes the current setup operation.
type Phase string

const (
	Checking    Phase = "checking"
	Downloading Phase = "downloading"
	Verifying   Phase = "verifying"
	Saving      Phase = "saving"
	Complete    Phase = "complete"
)

// Progress reports exact model byte counts, without a time estimate.
type Progress struct {
	Phase                      Phase
	CompletedBytes, TotalBytes int64
}

// ErrSetupBusy means another process owns the non-waiting setup lock.
var ErrSetupBusy = errors.New("setup is active; wait for it to finish, then run setup again")

const spaceReserveBytes int64 = 64 * 1024 * 1024
const progressInterval = 100 * time.Millisecond

type modelDescriptor struct {
	id, digest, url string
	size            int64
}

// Manager owns one managed setup attempt at a time under the user data lock.
type Manager struct {
	layout                                   Layout
	release                                  Release
	client                                   *http.Client
	ownedTransport                           *http.Transport
	model                                    modelDescriptor
	totalTimeout, headerTimeout, idleTimeout time.Duration
}

func pinnedModel() modelDescriptor {
	return modelDescriptor{"qwen2.5-0.5b-instruct-q4_k_m", "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db", "https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/9217f5db79a29953eb74d5343926648285ec7e67/qwen2.5-0.5b-instruct-q4_k_m.gguf", 491400032}
}

// New constructs a manager. It copies release metadata and HTTP settings. The
// supplied client must honor request cancellation; Prepare owns response bodies.
// The production model descriptor and HTTPS address cannot be overridden.
func New(layout Layout, release Release, client *http.Client) *Manager {
	release.Files = slices.Clone(release.Files)
	release.TestedOS = slices.Clone(release.TestedOS)
	copied := http.Client{}
	if client != nil {
		copied = *client
	}
	var ownedTransport *http.Transport
	transport := copied.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.ResponseHeaderTimeout = 15 * time.Second
		clone.MaxResponseHeaderBytes = maxManifestBytes
		clone.DisableKeepAlives = true
		copied.Transport = clone
		ownedTransport = clone
	}
	copied.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("download redirect must use HTTPS; run setup again after checking the network")
		}
		if len(via) > 5 {
			return errors.New("download exceeds five redirects; run setup again after checking the network")
		}
		return nil
	}
	return &Manager{layout: layout, release: release, client: &copied, ownedTransport: ownedTransport, model: pinnedModel(), totalTimeout: 30 * time.Minute, headerTimeout: 15 * time.Second, idleTimeout: 30 * time.Second}
}

func installation(layout Layout, release Release, model modelDescriptor) Installation {
	return Installation{RuntimeBinary: filepath.Join(layout.ReleaseRoot, "runtime", release.Architecture, "llama-server"), ModelPath: filepath.Join(layout.DataRoot, "models", model.digest, "model.gguf"), Backend: release.Backend, Version: release.Version}
}

// Prepare verifies the installed release and prepares the pinned model after
// caller-owned consent. A concurrent attempt fails immediately. Callbacks run
// synchronously and must honor cancellation. A callback error stops setup.
func (m *Manager) Prepare(ctx context.Context, progress func(Progress) error) (result Installation, err error) {
	ctx, cancel := context.WithTimeout(ctx, m.totalTimeout)
	defer cancel()
	if m.ownedTransport != nil {
		defer m.ownedTransport.CloseIdleConnections()
	}
	var lastProgress time.Time
	emit := func(phase Phase, completed int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if progress == nil {
			return nil
		}
		remaining := progressInterval - time.Since(lastProgress)
		if !lastProgress.IsZero() && remaining > 0 {
			if phase != Complete {
				return nil
			}
			// Completion is delivered within the same rate limit. The caller context
			// bounds this final wait, which is at most one progress interval.
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		lastProgress = time.Now()
		return progress(Progress{phase, completed, m.model.size})
	}
	if err := emit(Checking, 0); err != nil {
		return Installation{}, err
	}
	verified, err := LoadRelease(ctx, m.layout.ReleaseRoot)
	if err != nil {
		return Installation{}, err
	}
	if !sameRelease(verified, m.release) {
		return Installation{}, errors.New("release changed; restart setup from the installed package")
	}
	root, err := openDataRoot(m.layout.DataRoot, true)
	if err != nil {
		return Installation{}, fmt.Errorf("open setup data directory: %w; check directory permissions", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	lock, err := openLock(root)
	if err != nil {
		return Installation{}, err
	}
	defer func() { err = errors.Join(err, unix.Flock(int(lock.Fd()), unix.LOCK_UN), lock.Close()) }()
	dirs := []*os.File{}
	defer func() {
		for i := len(dirs) - 1; i >= 0; i-- {
			err = errors.Join(err, dirs[i].Close())
		}
	}()
	openDir := func(parent *os.File, name string) (*os.File, error) {
		f, e := managedDirectory(parent, name, true)
		if e == nil {
			e = privateDirectory(f)
			if e != nil {
				e = errors.Join(e, f.Close())
				return nil, e
			}
			dirs = append(dirs, f)
		}
		return f, e
	}
	models, err := openDir(root, "models")
	if err != nil {
		return Installation{}, err
	}
	modelDir, err := openDir(models, m.model.digest)
	if err != nil {
		return Installation{}, err
	}
	receipts, err := openDir(root, "installations")
	if err != nil {
		return Installation{}, err
	}
	staging, err := openDir(root, "staging")
	if err != nil {
		return Installation{}, err
	}
	if err := cleanStaging(ctx, staging); err != nil {
		return Installation{}, err
	}
	existing, err := openRegular(int(modelDir.Fd()), "model.gguf")
	if err == nil {
		verifyErr := verifyModel(ctx, existing, m.model)
		if err := errors.Join(verifyErr, existing.Close()); err != nil {
			if ctx.Err() != nil {
				return Installation{}, errors.Join(ctx.Err(), err)
			}
			return Installation{}, fmt.Errorf("managed model is corrupt: %w; stop Agents that use it, move the file aside, then run setup again", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installation{}, fmt.Errorf("open managed model: %w; check the model directory", err)
	} else {
		var space unix.Statfs_t
		if err := unix.Fstatfs(int(modelDir.Fd()), &space); err != nil {
			return Installation{}, fmt.Errorf("check free space: %w", err)
		}
		// Compare by division to avoid overflow in the platform's block count.
		if space.Bsize <= 0 || m.model.size > int64(^uint64(0)>>1)-spaceReserveBytes || uint64(space.Bavail) < requiredBlocks(m.model.size+spaceReserveBytes, uint64(space.Bsize)) {
			return Installation{}, fmt.Errorf("model needs %d bytes plus 64 MiB: %w; free disk space, then run setup again", m.model.size, unix.ENOSPC)
		}
		if err := emit(Downloading, 0); err != nil {
			return Installation{}, err
		}
		temp, name, err := privateTemp(staging)
		if err != nil {
			return Installation{}, fmt.Errorf("create model staging file: %w; check free space and permissions", err)
		}
		defer func() { err = errors.Join(err, removeTemp(staging, name)) }()
		downloadErr := m.download(ctx, temp, func(p Progress) error { return emit(p.Phase, p.CompletedBytes) })
		if downloadErr == nil {
			downloadErr = emit(Verifying, m.model.size)
		}
		if downloadErr == nil {
			downloadErr = temp.Sync()
		}
		downloadErr = errors.Join(downloadErr, temp.Close())
		if downloadErr != nil {
			return Installation{}, fmt.Errorf("prepare model: %w; run setup again", downloadErr)
		}
		if err := ctx.Err(); err != nil {
			return Installation{}, err
		}
		// The setup lock does not exclude operator writes. An exclusive rename
		// preserves any entry created during the download, including a link.
		if err := unix.RenameatxNp(int(staging.Fd()), name, int(modelDir.Fd()), "model.gguf", unix.RENAME_EXCL); err != nil {
			if errors.Is(err, os.ErrExist) {
				return Installation{}, fmt.Errorf("model entry appeared during setup: %w; preserve valid files, or stop its Agents and move unsafe files aside, then run setup again", err)
			}
			return Installation{}, fmt.Errorf("activate model: %w; check free space and permissions", err)
		}
		if err := modelDir.Sync(); err != nil {
			return Installation{}, fmt.Errorf("sync model directory: %w", err)
		}
	}
	if err := emit(Saving, m.model.size); err != nil {
		return Installation{}, err
	}
	if err := saveReceipt(ctx, receipts, staging, m.release, m.model); err != nil {
		return Installation{}, err
	}
	if err := emit(Complete, m.model.size); err != nil {
		return Installation{}, err
	}
	return installation(m.layout, m.release, m.model), nil
}
func requiredBlocks(bytes int64, blockSize uint64) uint64 {
	n := uint64(bytes)
	blocks := n / blockSize
	if n%blockSize != 0 {
		blocks++
	}
	return blocks
}
func sameRelease(a, b Release) bool {
	return a.Schema == b.Schema && a.Version == b.Version && a.Architecture == b.Architecture && a.RuntimeVersion == b.RuntimeVersion && a.RuntimeCommit == b.RuntimeCommit && a.Backend == b.Backend && slices.Equal(a.TestedOS, b.TestedOS) && slices.Equal(a.Files, b.Files)
}

// Resolve each directory through an owned descriptor. No path component can
// redirect setup through a symbolic link, including the user data root.
func openDataRoot(name string, create bool) (file *os.File, err error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" {
		return nil, errors.New("data root must be an absolute directory path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		next, openErr := managedDirectory(current, part, create)
		closeErr := current.Close()
		if openErr != nil || closeErr != nil {
			if next != nil {
				closeErr = errors.Join(closeErr, next.Close())
			}
			return nil, errors.Join(openErr, closeErr)
		}
		current = next
	}
	if err := privateDirectory(current); err != nil {
		return nil, errors.Join(err, current.Close())
	}
	return current, nil
}
func privateDirectory(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 {
		return errors.New("managed directory must belong to this user with no group or other access; restrict its permissions, then run setup again")
	}
	return nil
}
func managedDirectory(parent *os.File, name string, create bool) (*os.File, error) {
	if !validPath(name) || strings.Contains(name, "/") {
		return nil, errors.New("unsafe managed directory name")
	}
	if create {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create managed directory %q: %w", name, err)
		}
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open managed directory %q: %w", name, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}
func openLock(root *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), "setup.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open setup lock: %w; check data directory permissions", err)
	}
	lock := os.NewFile(uintptr(fd), "setup.lock")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return nil, errors.Join(errors.New("setup lock must be a regular private file"), lock.Close())
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			err = ErrSetupBusy
		}
		return nil, errors.Join(err, lock.Close())
	}
	return lock, nil
}
func privateTemp(dir *os.File) (*os.File, string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", err
	}
	name := "setup-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), name), name, nil
}
func removeTemp(dir *os.File, name string) error {
	err := unix.Unlinkat(int(dir.Fd()), name, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func cleanStaging(ctx context.Context, dir *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(1)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := entries[0].Name()
		if !strings.HasPrefix(name, "setup-") {
			return fmt.Errorf("unexpected staging file %q; move it aside, then run setup again", name)
		}
		f, err := openRegular(int(dir.Fd()), name)
		if err != nil {
			return fmt.Errorf("unsafe staging file %q: %w; move it aside, then run setup again", name, err)
		}
		if err := errors.Join(f.Close(), removeTemp(dir, name)); err != nil {
			return err
		}
	}
}
func verifyModel(ctx context.Context, file *os.File, model modelDescriptor) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != model.size {
		return errors.New("model size differs from the pinned size")
	}
	h := sha256.New()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != model.digest {
		return errors.New("model SHA-256 differs from the pinned digest")
	}
	return nil
}
