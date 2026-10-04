// Package setup verifies installed release files and prepares managed assets.
package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// Release describes the immutable native release payload. Model identity and
// download addresses belong to compiled defaults, never this manifest.
type Release struct {
	Schema         int           `json:"schema"`
	Version        string        `json:"version"`
	Architecture   string        `json:"architecture"`
	RuntimeVersion string        `json:"runtime_version"`
	RuntimeCommit  string        `json:"runtime_commit"`
	Backend        string        `json:"backend"`
	TestedOS       []string      `json:"tested_os"`
	Files          []ReleaseFile `json:"files"`
}

// ReleaseFile records one regular payload file, relative to ReleaseRoot.
type ReleaseFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Purpose   string `json:"purpose"`
}

// Layout supplies the installed release and per-user managed data roots.
type Layout struct{ ReleaseRoot, DataRoot string }

const maxManifestBytes = 64 * 1024
const runtimeCommit = "7fe450e19305b828c199d602c23a8337aaa1f03b"

// LoadRelease verifies the bounded manifest and all payload files. It never
// follows a link within the release, launches a process, or selects a model.
// The signed installer owns this directory; callers must not mutate it while
// verification or a running process uses its files.
func LoadRelease(ctx context.Context, root string) (release Release, err error) {
	if err := ctx.Err(); err != nil {
		return Release{}, err
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Release{}, fmt.Errorf("open release directory: %w; reinstall the native package", err)
	}
	directory := os.NewFile(uintptr(fd), root)
	defer func() { err = errors.Join(err, directory.Close()) }()
	manifest, err := openRegular(fd, "release.json")
	if err != nil {
		return Release{}, fmt.Errorf("open release manifest: %w; reinstall the native package", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(manifest, maxManifestBytes+1))
	if err := errors.Join(readErr, manifest.Close()); err != nil {
		return Release{}, fmt.Errorf("read release manifest: %w", err)
	}
	if len(data) > maxManifestBytes {
		return Release{}, errors.New("release manifest exceeds 64 KiB; reinstall the native package")
	}
	if err := strictJSON(data); err != nil {
		return Release{}, fmt.Errorf("decode release manifest: %w; reinstall the native package", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&release); err != nil {
		return Release{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := validateRelease(release); err != nil {
		return Release{}, fmt.Errorf("invalid release: %w; reinstall the native package", err)
	}
	listed := make(map[string]ReleaseFile, len(release.Files))
	for _, entry := range release.Files {
		if _, exists := listed[entry.Path]; exists {
			return Release{}, fmt.Errorf("duplicate release path %q", entry.Path)
		}
		listed[entry.Path] = entry
	}
	if err := checkInventory(ctx, directory, "", listed); err != nil {
		return Release{}, fmt.Errorf("check release inventory: %w; reinstall the native package", err)
	}
	for _, entry := range release.Files {
		if err := verifyFile(ctx, fd, entry, release.Architecture); err != nil {
			return Release{}, fmt.Errorf("verify release file %q: %w; reinstall the native package", entry.Path, err)
		}
	}
	return release, nil
}

func validateRelease(r Release) error {
	if r.Schema != 1 {
		return errors.New("schema must be 1")
	}
	if r.Version == "" || r.Version == "." || r.Version == ".." || strings.ContainsAny(r.Version, "/\\") {
		return errors.New("product version must be a directory name")
	}
	for _, c := range r.Version {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.ContainsRune(".-_", c)) {
			return errors.New("invalid product version")
		}
	}
	if r.Architecture != runtime.GOARCH || (r.Architecture != "arm64" && r.Architecture != "amd64") {
		return errors.New("package architecture differs from the native process")
	}
	backend := "cpu"
	if r.Architecture == "arm64" {
		backend = "metal"
	}
	if r.Backend != backend || r.RuntimeVersion != "0.5.0" || r.RuntimeCommit != runtimeCommit {
		return errors.New("runtime identity or native backend differs from the pinned build")
	}
	if len(r.TestedOS) == 0 {
		return errors.New("tested OS versions are required")
	}
	for _, version := range r.TestedOS {
		if strings.TrimSpace(version) == "" {
			return errors.New("tested OS version must not be empty")
		}
	}
	// The pinned build is static and embeds Metal. Its only runtime file is the
	// server. The aggregate notice includes the enabled third-party components.
	required := map[string]string{"bin/mica-mesh": "cli", "runtime/" + r.Architecture + "/llama-server": "runtime", "licenses/llama.cpp.txt": "license", "licenses/third-party.txt": "license"}
	for _, f := range r.Files {
		if !validPath(f.Path) {
			return fmt.Errorf("unsafe path %q", f.Path)
		}
		purpose, ok := required[f.Path]
		if !ok || f.Purpose != purpose {
			return fmt.Errorf("unexpected file or purpose %q", f.Path)
		}
		digest, err := hex.DecodeString(f.SHA256)
		if err != nil || len(digest) != sha256.Size || f.SizeBytes <= 0 {
			return fmt.Errorf("invalid identity for %q", f.Path)
		}
	}
	for name := range required {
		found := false
		for _, f := range r.Files {
			if f.Path == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required file %q is not listed", name)
		}
	}
	return nil
}

func validPath(name string) bool {
	return name != "" && name != "." && !strings.ContainsAny(name, "\\\x00") && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

// Token validation rejects duplicate/null values and case variants before the
// typed decoder, which otherwise accepts case-insensitive JSON field names.
func strictJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	fields := map[string]bool{"schema": true, "version": true, "architecture": true, "runtime_version": true, "runtime_commit": true, "backend": true, "tested_os": true, "files": true, "path": true, "size_bytes": true, "sha256": true, "purpose": true}
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		if token == nil {
			return errors.New("null values are not allowed")
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || !fields[name] {
					return fmt.Errorf("unknown field %q", key)
				}
				if seen[name] {
					return fmt.Errorf("duplicate field %q", name)
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case json.Delim('['):
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

// Open each component relative to an owned directory descriptor. O_NOFOLLOW
// applies at every step, so swapped parent links cannot redirect file reads.
func openRegular(rootFD int, name string) (file *os.File, err error) {
	parts := strings.Split(name, "/")
	parent := rootFD
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, openErr := unix.Openat(parent, part, flags, 0)
		if parent != rootFD {
			openErr = errors.Join(openErr, unix.Close(parent))
		}
		if openErr != nil {
			if fd >= 0 {
				openErr = errors.Join(openErr, unix.Close(fd))
			}
			return nil, openErr
		}
		parent = fd
	}
	file = os.NewFile(uintptr(parent), name)
	var stat unix.Stat_t
	if err := unix.Fstat(parent, &stat); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if stat.Nlink != 1 {
		return nil, errors.Join(errors.New("hard links are not allowed"), file.Close())
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("file is not regular")
		}
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func checkInventory(ctx context.Context, directory *os.File, prefix string, listed map[string]ReleaseFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := directory.ReadDir(1)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entry := entries[0]
		name := prefix + entry.Name()
		var info unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), entry.Name(), &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if info.Mode&unix.S_IFMT == unix.S_IFDIR {
			allowed := false
			for p := range listed {
				if strings.HasPrefix(p, name+"/") {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("unexpected directory %q", name)
			}
			fd, err := unix.Openat(int(directory.Fd()), entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			child := os.NewFile(uintptr(fd), name)
			if err := errors.Join(checkInventory(ctx, child, name+"/", listed), child.Close()); err != nil {
				return err
			}
		} else {
			if info.Mode&unix.S_IFMT != unix.S_IFREG {
				return fmt.Errorf("nonregular file %q", name)
			}
			if name != "release.json" {
				if _, ok := listed[name]; !ok {
					return fmt.Errorf("unlisted file %q", name)
				}
			}
		}
	}
	return nil
}

func verifyFile(ctx context.Context, rootFD int, entry ReleaseFile, arch string) (err error) {
	file, err := openRegular(rootFD, entry.Path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != entry.SizeBytes {
		return errors.New("byte size differs from manifest")
	}
	if entry.Purpose == "cli" || entry.Purpose == "runtime" {
		if info.Mode()&0111 == 0 {
			return errors.New("binary is not executable")
		}
		if err := verifyNative(file, info.Size(), arch, entry.Purpose == "runtime" && arch == "arm64"); err != nil {
			return err
		}
	}
	h := sha256.New()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), entry.SHA256) {
		return errors.New("SHA-256 differs from manifest")
	}
	return nil
}

// Read only the load commands needed for the pinned static payload. A general
// Mach-O parser also loads command-selected symbol tables; these are unnecessary
// for release verification and could allocate beyond the manifest input bound.
func verifyNative(file *os.File, size int64, arch string, metal bool) error {
	var header [32]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return err
	}
	order := binary.LittleEndian
	if order.Uint32(header[:4]) != macho.Magic64 {
		return errors.New("binary is not native 64-bit Mach-O")
	}
	cpu := uint32(macho.CpuAmd64)
	if arch == "arm64" {
		cpu = uint32(macho.CpuArm64)
	}
	if order.Uint32(header[4:8]) != cpu || order.Uint32(header[12:16]) != uint32(macho.TypeExec) {
		return errors.New("binary architecture or executable type differs from package")
	}
	count := order.Uint32(header[16:20])
	commandBytes := order.Uint32(header[20:24])
	if count > 4096 || commandBytes > 4*1024*1024 || int64(commandBytes)+32 > size {
		return errors.New("invalid Mach-O command bounds")
	}
	commands := make([]byte, int(commandBytes))
	if _, err := file.ReadAt(commands, 32); err != nil {
		return err
	}
	embedded := false
	for i := uint32(0); i < count; i++ {
		if len(commands) < 8 {
			return errors.New("truncated Mach-O command")
		}
		kind := order.Uint32(commands[:4])
		length := order.Uint32(commands[4:8])
		if length < 8 || length%8 != 0 || uint64(length) > uint64(len(commands)) {
			return errors.New("invalid Mach-O command size")
		}
		command := commands[:int(length)]
		commands = commands[int(length):]
		switch kind {
		case 0xc, 0x80000018, 0x8000001f, 0x80000023, 0x20, 0x8000001c:
			// Include weak, re-exported, upward, and lazy library loads and LC_RPATH.
			fixed := 24
			if kind == 0x8000001c {
				fixed = 12
			}
			if len(command) < fixed {
				return errors.New("truncated dependency command")
			}
			offset := order.Uint32(command[8:12])
			if offset < uint32(fixed) || offset >= length {
				return errors.New("invalid dependency name offset")
			}
			text := command[int(offset):]
			end := bytes.IndexByte(text, 0)
			if end < 0 {
				return errors.New("unterminated dependency path")
			}
			name := string(text[:end])
			if path.Clean(name) != name || !(strings.HasPrefix(name, "/usr/lib/") || strings.HasPrefix(name, "/System/Library/")) {
				return fmt.Errorf("runtime dependency %q is outside system locations", name)
			}
		case 2: // LC_SYMTAB: validate bounds without reading or allocating tables.
			if len(command) != 24 {
				return errors.New("invalid symbol table command")
			}
			symbols := uint64(order.Uint32(command[12:16]))
			symbolOffset := uint64(order.Uint32(command[8:12]))
			stringsSize := uint64(order.Uint32(command[20:24]))
			stringsOffset := uint64(order.Uint32(command[16:20]))
			if symbolOffset > uint64(size) || symbols*16 > uint64(size)-symbolOffset || stringsOffset > uint64(size) || stringsSize > uint64(size)-stringsOffset {
				return errors.New("invalid symbol table bounds")
			}
		case uint32(macho.LoadCmdSegment64):
			if len(command) < 72 {
				return errors.New("truncated segment command")
			}
			sections := order.Uint32(command[64:68])
			if uint64(sections)*80+72 != uint64(len(command)) {
				return errors.New("invalid segment section bounds")
			}
			for j := uint32(0); j < sections; j++ {
				section := command[72+int(j)*80 : 72+int(j+1)*80]
				name := strings.TrimRight(string(section[:16]), "\x00")
				segment := strings.TrimRight(string(section[16:32]), "\x00")
				if name == "__ggml_metallib" && segment == "__DATA" {
					length := order.Uint64(section[40:48])
					offset := uint64(order.Uint32(section[48:52]))
					if length == 0 || offset > uint64(size) || length > uint64(size)-offset {
						return errors.New("invalid embedded Metal resource bounds")
					}
					embedded = true
				}
			}
		}
	}
	if len(commands) != 0 {
		return errors.New("Mach-O command count differs from command area")
	}
	if metal && !embedded {
		return errors.New("required embedded Metal resources are missing")
	}
	return nil
}
