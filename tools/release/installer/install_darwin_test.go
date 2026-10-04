package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
)

func fixture(t *testing.T, version, content string) (string, string) {
	t.Helper()
	root := t.TempDir()
	cpu := uint32(macho.CpuAmd64)
	backend := "cpu"
	if runtime.GOARCH == "arm64" {
		cpu, backend = uint32(macho.CpuArm64), "metal"
	}
	var executable bytes.Buffer
	_ = binary.Write(&executable, binary.LittleEndian, []uint32{macho.Magic64, cpu, 0, uint32(macho.TypeExec), 0, 0, 0, 0})
	runtimeBytes := executable.Bytes()
	if backend == "metal" {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.LittleEndian, []uint32{macho.Magic64, cpu, 0, uint32(macho.TypeExec), 1, 152, 0, 0, uint32(macho.LoadCmdSegment64), 152})
		var name [16]byte
		copy(name[:], "__DATA")
		b.Write(name[:])
		_ = binary.Write(&b, binary.LittleEndian, []uint64{0, 1, 184, 1})
		_ = binary.Write(&b, binary.LittleEndian, []uint32{0, 0, 1, 0})
		name = [16]byte{}
		copy(name[:], "__ggml_metallib")
		b.Write(name[:])
		name = [16]byte{}
		copy(name[:], "__DATA")
		b.Write(name[:])
		_ = binary.Write(&b, binary.LittleEndian, []uint64{0, 1})
		_ = binary.Write(&b, binary.LittleEndian, []uint32{184, 0, 0, 0, 0, 0, 0, 0})
		b.WriteByte('x')
		runtimeBytes = b.Bytes()
	}
	r := setup.Release{Schema: 1, Version: version, Architecture: runtime.GOARCH, RuntimeVersion: "0.5.0", RuntimeCommit: "7fe450e19305b828c199d602c23a8337aaa1f03b", Backend: backend, TestedOS: []string{"fixture"}}
	for _, file := range []struct {
		name, purpose string
		data          []byte
	}{
		{"bin/mica-mesh", "cli", executable.Bytes()},
		{"runtime/" + runtime.GOARCH + "/llama-server", "runtime", runtimeBytes},
		{"licenses/llama.cpp.txt", "license", []byte("runtime notice")},
		{"licenses/third-party.txt", "license", []byte(content)},
	} {
		p := filepath.Join(root, file.name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if file.purpose != "license" {
			mode = 0755
		}
		if err := os.WriteFile(p, file.data, mode); err != nil {
			t.Fatal(err)
		}
		r.Files = append(r.Files, setup.ReleaseFile{Path: file.name, Purpose: file.purpose, SizeBytes: int64(len(file.data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(file.data))})
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return root, fmt.Sprintf("%x", sha256.Sum256(data))
}

func testLayout(t *testing.T) layout {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// This isolated directory represents the public filesystem root.
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	return layout{root: root, uid: uint32(os.Getuid())}
}

func TestInaccessibleExistingPathsAreRejectedWithoutRepair(t *testing.T) {
	for _, relative := range []string{
		"Library", "usr/local/bin", releases + "/0.1.0/bin",
		releases + "/0.1.0/bin/mica-mesh", releases + "/0.1.0/runtime/" + runtime.GOARCH + "/llama-server",
		releases + "/0.1.0/licenses/third-party.txt", releases + "/0.1.0/release.json",
	} {
		t.Run(relative, func(t *testing.T) {
			paths := testLayout(t)
			source, digest := fixture(t, "0.1.0", "notice")
			if err := install(context.Background(), paths, source, digest); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(paths.root, relative)
			if err := os.Chmod(target, 0700); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := install(context.Background(), paths, source, digest); err == nil {
				t.Error("inaccessible existing path was accepted")
			}
			after, err := os.Stat(target)
			if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0700 {
				t.Fatalf("existing path was rewritten or repaired: %v", err)
			}
			active, err := os.Readlink(filepath.Join(paths.root, commandDir, "mica-mesh"))
			if err != nil || active != filepath.Join(paths.root, releases, "0.1.0/bin/mica-mesh") {
				t.Fatalf("existing link changed: %q, %v", active, err)
			}
		})
	}
}

func TestInstallActivatesCompleteReleaseAndExactReinstallPreservesInode(t *testing.T) {
	paths := testLayout(t)
	source, digest := fixture(t, "0.1.0", "notice")
	if err := install(context.Background(), paths, source, digest); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(paths.root, releases, "0.1.0/bin/mica-mesh")
	before, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(paths.root, commandDir, "mica-mesh")
	target, err := os.Readlink(link)
	if err != nil || target != executable {
		t.Fatalf("target = %q, %v", target, err)
	}
	if err := install(context.Background(), paths, source, digest); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(executable)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("reinstall rewrote executable: %v", err)
	}
}

func TestConflictingVersionPreservesPayloadAndActiveLink(t *testing.T) {
	paths := testLayout(t)
	source, digest := fixture(t, "0.1.0", "original")
	if err := install(context.Background(), paths, source, digest); err != nil {
		t.Fatal(err)
	}
	conflict, other := fixture(t, "0.1.0", "different")
	if err := install(context.Background(), paths, conflict, other); err == nil {
		t.Fatal("accepted different payload under same version")
	}
	data, err := os.ReadFile(filepath.Join(paths.root, releases, "0.1.0/licenses/third-party.txt"))
	if err != nil || string(data) != "original" {
		t.Fatalf("old payload changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(paths.root, commandDir, "mica-mesh")); err != nil {
		t.Fatal(err)
	}
}

func TestUnrelatedLinkAndFileArePreserved(t *testing.T) {
	for _, kind := range []string{"link", "file"} {
		t.Run(kind, func(t *testing.T) {
			paths := testLayout(t)
			source, digest := fixture(t, "0.1.0", "notice")
			dir := filepath.Join(paths.root, commandDir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "mica-mesh")
			var err error
			if kind == "link" {
				err = os.Symlink("/unrelated/tool", link)
			} else {
				err = os.WriteFile(link, []byte("unrelated"), 0755)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.Lstat(link)
			if err := install(context.Background(), paths, source, digest); err == nil {
				t.Fatal("accepted unrelated command")
			}
			after, err := os.Lstat(link)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("unrelated command changed: %v", err)
			}
		})
	}
}

func TestManifestAnchorAndCancellationRejectBeforeActivation(t *testing.T) {
	paths := testLayout(t)
	source, _ := fixture(t, "0.1.0", "notice")
	if err := install(context.Background(), paths, source, fmt.Sprintf("%064d", 0)); err == nil {
		t.Fatal("accepted unexpected manifest")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := install(ctx, paths, source, ""); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := os.Lstat(filepath.Join(paths.root, commandDir, "mica-mesh")); !os.IsNotExist(err) {
		t.Fatalf("activated rejected payload: %v", err)
	}
}

func TestRetryAfterVersionActivationPreservesBothVersions(t *testing.T) {
	paths := testLayout(t)
	old, oldDigest := fixture(t, "0.1.0", "old")
	if err := install(context.Background(), paths, old, oldDigest); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(paths.root, commandDir, "mica-mesh")
	oldExecutable, err := os.Open(link)
	if err != nil {
		t.Fatal(err)
	}
	defer oldExecutable.Close()
	next, nextDigest := fixture(t, "0.2.0", "next")
	root, err := managedDirectory(paths, releases)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	staging, err := openDirectory(root, ".staging", paths.uid, 0700)
	if err != nil {
		t.Fatal(err)
	}
	defer staging.Close()
	stage, err := stageRelease(context.Background(), paths, next, nextDigest, staging)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := activateVersion(context.Background(), paths, root, stage, nextDigest)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(destination, "bin/mica-mesh"))
	if err != nil {
		t.Fatal(err)
	}
	bin, err := managedDirectory(paths, commandDir)
	if err != nil {
		t.Fatal(err)
	}
	defer bin.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := activateLink(ctx, paths, bin, destination); err == nil {
		t.Fatal("ignored cancellation before link activation")
	}
	target, err := os.Readlink(link)
	if err != nil || target != filepath.Join(paths.root, releases, "0.1.0/bin/mica-mesh") {
		t.Fatalf("old active link lost: %q, %v", target, err)
	}
	if err := install(context.Background(), paths, next, nextDigest); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(destination, "bin/mica-mesh"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("retry rewrote completed version: %v", err)
	}
	oldInfo, err := oldExecutable.Stat()
	if err != nil {
		t.Fatal(err)
	}
	retained, err := os.Stat(filepath.Join(paths.root, releases, "0.1.0/bin/mica-mesh"))
	if err != nil || !os.SameFile(oldInfo, retained) {
		t.Fatalf("old executable replaced: %v", err)
	}
}

func TestUnsafeParentsAndDanglingOwnedLinkAreRejected(t *testing.T) {
	for _, kind := range []string{"writable", "symlink", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			paths := testLayout(t)
			source, digest := fixture(t, "0.1.0", "notice")
			bin := filepath.Join(paths.root, commandDir)
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "writable":
				if err := os.Chmod(bin, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(bin); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), bin); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				if err := os.Symlink(filepath.Join(paths.root, releases, "missing/bin/mica-mesh"), filepath.Join(bin, "mica-mesh")); err != nil {
					t.Fatal(err)
				}
			}
			if err := install(context.Background(), paths, source, digest); err == nil {
				t.Fatal("accepted unsafe destination")
			}
			if _, err := os.Lstat(filepath.Join(paths.root, releases, "0.1.0")); !os.IsNotExist(err) {
				t.Fatalf("published version into unsafe destination: %v", err)
			}
		})
	}
}

func TestInstallLockRejectsOverlapAndReleaseAllowsRetry(t *testing.T) {
	paths := testLayout(t)
	source, digest := fixture(t, "0.1.0", "notice")
	root, err := managedDirectory(paths, releases)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := acquireLock(root, paths.uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := install(context.Background(), paths, source, digest); err == nil {
		t.Fatal("overlapping installer acquired lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := install(context.Background(), paths, source, digest); err != nil {
		t.Fatal(err)
	}
}

func TestACLWriteGrantRejectsDirectoryAndInstalledFile(t *testing.T) {
	for _, kind := range []string{"directory", "file"} {
		t.Run(kind, func(t *testing.T) {
			paths := testLayout(t)
			source, digest := fixture(t, "0.1.0", "notice")
			if err := install(context.Background(), paths, source, digest); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(paths.root, commandDir)
			if kind == "file" {
				target = filepath.Join(paths.root, releases, "0.1.0/bin/mica-mesh")
			}
			file, err := os.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixtureWriteACL(file); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := install(context.Background(), paths, source, digest); err == nil {
				t.Fatal("accepted ACL write grant")
			}
		})
	}
}

func TestACLAbsentAndClosedDescriptor(t *testing.T) {
	for _, directory := range []bool{false, true} {
		path := t.TempDir()
		if !directory {
			path = filepath.Join(path, "file")
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkACL(file, false); err != nil {
			_ = file.Close()
			t.Fatalf("ordinary no-ACL object: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := checkACL(file, false); err == nil {
			t.Fatal("closed descriptor was accepted as an absent ACL")
		}
	}
}

func TestPublicACLAccessDenialsAreRejectedAndDeleteDenialIsAllowed(t *testing.T) {
	for _, right := range []string{"read", "execute", "attributes", "delete"} {
		for _, relative := range []string{commandDir, releases + "/0.1.0/bin", releases + "/0.1.0/bin/mica-mesh"} {
			t.Run(right+"/"+relative, func(t *testing.T) {
				paths := testLayout(t)
				source, digest := fixture(t, "0.1.0", "notice")
				if err := install(context.Background(), paths, source, digest); err != nil {
					t.Fatal(err)
				}
				file, err := os.Open(filepath.Join(paths.root, relative))
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := fixtureDenyACL(file, right); err != nil {
					t.Fatal(err)
				}
				err = install(context.Background(), paths, source, digest)
				if right == "delete" && err != nil {
					t.Fatalf("ordinary deny-delete ACL was refused: %v", err)
				}
				if right != "delete" && err == nil {
					t.Fatal("public access denial was accepted")
				}
			})
		}
	}
}

func TestUnreadableReleaseDirectoryIsRejected(t *testing.T) {
	paths := testLayout(t)
	source, digest := fixture(t, "0.1.0", "notice")
	if err := install(context.Background(), paths, source, digest); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.root, releases, "0.1.0/licenses")
	if err := os.Chmod(target, 0711); err != nil {
		t.Fatal(err)
	}
	if err := install(context.Background(), paths, source, digest); err == nil {
		t.Fatal("release directory cannot be enumerated by ordinary users")
	}
}
