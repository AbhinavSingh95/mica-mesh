package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	"golang.org/x/sys/unix"
)

const releases = "Library/Application Support/Mica Mesh"
const commandDir = "usr/local/bin"

// layout is fixed by the executable. Tests use an isolated filesystem root.
// All directories below root must belong to uid and exclude other writers.
// An existing Homebrew-owned directory is refused, never taken over.
type layout struct {
	root string
	uid  uint32
}

func trusted(file *os.File, uid uint32, directory bool) error {
	var s unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &s); err != nil {
		return err
	}
	kind := uint32(unix.S_IFREG)
	if directory {
		kind = unix.S_IFDIR
	}
	if s.Mode&unix.S_IFMT != uint16(kind) || s.Uid != uid || s.Mode&0022 != 0 || (!directory && s.Nlink != 1) {
		return fmt.Errorf("unsafe owner, mode, or file type at %s; use a root-owned directory without group or public write access", file.Name())
	}
	return checkACL(file, false)
}

// publicAccess checks the permissions needed by ordinary installed-command
// users. Private staging directories and the installation lock do not use it.
func publicAccess(file *os.File, required os.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&required != required {
		return fmt.Errorf("installed path %s is inaccessible to ordinary users; preserve it and resolve its access permissions before retrying", file.Name())
	}
	return checkACL(file, true)
}

func openDirectory(parent *os.File, name string, uid uint32, mode os.FileMode) (*os.File, error) {
	mkdirErr := unix.Mkdirat(int(parent.Fd()), name, uint32(mode))
	if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
		return nil, mkdirErr
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	if err := trusted(file, uid, true); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	// Only a directory created by this attempt may have its umask adjusted.
	if mkdirErr == nil {
		if err := file.Chmod(mode); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	if mode == 0755 {
		if err := publicAccess(file, 0111); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	return file, nil
}

func managedDirectory(paths layout, relative string) (*os.File, error) {
	fd, err := unix.Open(paths.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(fd), paths.root)
	if err := trusted(parent, paths.uid, true); err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	if err := publicAccess(parent, 0111); err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	for _, part := range strings.Split(relative, "/") {
		next, openErr := openDirectory(parent, part, paths.uid, 0755)
		closeErr := parent.Close()
		if err := errors.Join(openErr, closeErr); err != nil {
			if next != nil {
				err = errors.Join(err, next.Close())
			}
			return nil, err
		}
		parent = next
	}
	return parent, nil
}

func acquireLock(directory *os.File, uid uint32) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), ".install.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.Name(), ".install.lock"))
	if err := trusted(file, uid, false); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("another installation is active: %w; wait for it to finish and retry", err), file.Close())
	}
	// Closing this descriptor releases the OS lock, including after process exit.
	return file, nil
}

func checkTree(root string, uid uint32) error {
	return filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
		if entry.IsDir() {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Open(name, flags, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), name)
		info, err := f.Stat()
		if err != nil {
			return errors.Join(err, f.Close())
		}
		required := os.FileMode(0444)
		if entry.IsDir() || info.Mode().Perm()&0111 != 0 {
			required = 0555
		}
		return errors.Join(trusted(f, uid, entry.IsDir()), publicAccess(f, required), f.Close())
	})
}

func manifestHash(root string) (string, error) {
	fd, err := unix.Open(filepath.Join(root, "release.json"), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), "release.json")
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return "", errors.Join(errors.New("invalid release manifest; rebuild the package"), err, f.Close())
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func verify(ctx context.Context, root, digest string) (setup.Release, error) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return setup.Release{}, errors.New("invalid package manifest digest; rebuild the package")
	}
	actual, err := manifestHash(root)
	if err != nil {
		return setup.Release{}, err
	}
	if actual != digest {
		return setup.Release{}, errors.New("package manifest identity differs; obtain a fresh package")
	}
	return setup.LoadRelease(ctx, root)
}

func ownedLink(ctx context.Context, paths layout, bin *os.File) error {
	if err := errors.Join(trusted(bin, paths.uid, true), publicAccess(bin, 0111)); err != nil {
		return err
	}
	var stat unix.Stat_t
	err := unix.Fstatat(int(bin.Fd()), "mica-mesh", &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK || stat.Uid != paths.uid {
		return errors.New("mica-mesh is an unrelated command; preserve it and choose how to move it before installing")
	}
	data := make([]byte, 4096)
	n, err := unix.Readlinkat(int(bin.Fd()), "mica-mesh", data)
	if err != nil {
		return err
	}
	target := string(data[:n])
	root := filepath.Join(paths.root, releases)
	relative, err := filepath.Rel(root, target)
	parts := strings.Split(relative, string(os.PathSeparator))
	if err != nil || !filepath.IsAbs(target) || filepath.Clean(target) != target || len(parts) != 3 || parts[0] == ".." || parts[1] != "bin" || parts[2] != "mica-mesh" {
		return errors.New("mica-mesh links to an unrelated location; preserve the link and resolve the conflict before installing")
	}
	versionRoot := filepath.Join(root, parts[0])
	if err := checkTree(versionRoot, paths.uid); err != nil {
		return err
	}
	release, err := setup.LoadRelease(ctx, versionRoot)
	if err != nil {
		return err
	}
	if release.Version != parts[0] {
		return errors.New("linked version identity differs; repair the existing installation before retrying")
	}
	return nil
}

func uniqueName(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func syncDirectory(file *os.File) error {
	err := file.Sync()
	// Some filesystems cannot sync directories. Atomic rename still provides
	// process-interruption safety; this does not claim power-loss durability.
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	return err
}

func copyFile(ctx context.Context, source, target string, size int64, mode os.FileMode) (err error) {
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	input := os.NewFile(uintptr(fd), source)
	defer func() { err = errors.Join(err, input.Close()) }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("package file changed while copying; obtain a fresh package")
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	buffer := make([]byte, 64*1024)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := int64(len(buffer))
		if count > remaining {
			count = remaining
		}
		n, err := io.ReadFull(input, buffer[:count])
		if err != nil {
			return err
		}
		if _, err := output.Write(buffer[:n]); err != nil {
			return err
		}
		remaining -= int64(n)
	}
	if err := output.Chmod(mode); err != nil {
		return err
	}
	return output.Sync()
}

func stageRelease(ctx context.Context, paths layout, source, digest string, staging *os.File) (stage string, err error) {
	release, err := verify(ctx, source, digest)
	if err != nil {
		return "", err
	}
	name, err := uniqueName("install-")
	if err != nil {
		return "", err
	}
	if err := unix.Mkdirat(int(staging.Fd()), name, 0700); err != nil {
		return "", err
	}
	stage = filepath.Join(staging.Name(), name)
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(stage))
			stage = ""
		}
	}()
	for _, entry := range release.Files {
		target := filepath.Join(stage, entry.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return stage, err
		}
		mode := os.FileMode(0644)
		if entry.Purpose != "license" {
			mode = 0755
		}
		if err := copyFile(ctx, filepath.Join(source, entry.Path), target, entry.SizeBytes, mode); err != nil {
			return stage, err
		}
	}
	info, err := os.Stat(filepath.Join(source, "release.json"))
	if err != nil {
		return stage, err
	}
	if err := copyFile(ctx, filepath.Join(source, "release.json"), filepath.Join(stage, "release.json"), info.Size(), 0644); err != nil {
		return stage, err
	}
	if _, err := verify(ctx, stage, digest); err != nil {
		return stage, err
	}
	// Every directory in this private stage belongs to this attempt. Set public
	// traversal independently of umask, then sync before publishing its name.
	err = filepath.WalkDir(stage, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		if err := f.Chmod(0755); err != nil {
			return errors.Join(err, f.Close())
		}
		return errors.Join(syncDirectory(f), f.Close())
	})
	if err != nil {
		return stage, err
	}
	return stage, checkTree(stage, paths.uid)
}

func activateVersion(ctx context.Context, paths layout, root *os.File, stage, digest string) (string, error) {
	release, err := verify(ctx, stage, digest)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(root.Name(), release.Version)
	var s unix.Stat_t
	err = unix.Fstatat(int(root.Fd()), release.Version, &s, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if err := checkTree(destination, paths.uid); err != nil {
			return "", err
		}
		if _, err := verify(ctx, destination, digest); err != nil {
			return "", fmt.Errorf("version %s already has different or invalid contents: %w; preserve it and use a new product version", release.Version, err)
		}
		return destination, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := unix.RenameatxNp(unix.AT_FDCWD, stage, int(root.Fd()), release.Version, unix.RENAME_EXCL); err != nil {
		return "", err
	}
	if err := syncDirectory(root); err != nil {
		return "", err
	}
	return destination, nil
}

func activateLink(ctx context.Context, paths layout, bin *os.File, destination string) (err error) {
	if err := ownedLink(ctx, paths, bin); err != nil {
		return err
	}
	name, err := uniqueName(".mica-mesh-")
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(filepath.Join(destination, "bin/mica-mesh"), int(bin.Fd()), name); err != nil {
		return err
	}
	defer func() {
		cleanup := unix.Unlinkat(int(bin.Fd()), name, 0)
		if !errors.Is(cleanup, unix.ENOENT) {
			err = errors.Join(err, cleanup)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ownedLink(ctx, paths, bin); err != nil {
		return err
	}
	if err := unix.Renameat(int(bin.Fd()), name, int(bin.Fd()), "mica-mesh"); err != nil {
		return err
	}
	return syncDirectory(bin)
}

func install(ctx context.Context, paths layout, source, digest string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := verify(ctx, source, digest); err != nil {
		return err
	}
	root, err := managedDirectory(paths, releases)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	lock, err := acquireLock(root, paths.uid)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	bin, err := managedDirectory(paths, commandDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, bin.Close()) }()
	if err := ownedLink(ctx, paths, bin); err != nil {
		return err
	}
	staging, err := openDirectory(root, ".staging", paths.uid, 0700)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, staging.Close()) }()
	stage, err := stageRelease(ctx, paths, source, digest, staging)
	if err != nil {
		return err
	}
	// Only this attempt's private stage is removed. Crashed attempts' stages are
	// retained; deleting them cannot be justified merely by their names or age.
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	destination, err := activateVersion(ctx, paths, root, stage, digest)
	if err != nil {
		return err
	}
	return activateLink(ctx, paths, bin, destination)
}
