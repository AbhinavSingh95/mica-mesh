package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixtureBinary(arch string, metal bool) []byte {
	cpu := uint32(macho.CpuAmd64)
	if arch == "arm64" {
		cpu = uint32(macho.CpuArm64)
	}
	var b bytes.Buffer
	ncmd, cmdsize := uint32(0), uint32(0)
	if metal {
		ncmd, cmdsize = 1, 152
	}
	_ = binary.Write(&b, binary.LittleEndian, []uint32{macho.Magic64, cpu, 0, uint32(macho.TypeExec), ncmd, cmdsize, 0, 0})
	if metal {
		_ = binary.Write(&b, binary.LittleEndian, []uint32{uint32(macho.LoadCmdSegment64), 152})
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
	}
	return b.Bytes()
}

func releaseFixture(t *testing.T) (string, Release) {
	t.Helper()
	root := t.TempDir()
	arch := runtime.GOARCH
	backend := "cpu"
	if arch == "arm64" {
		backend = "metal"
	}
	r := Release{Schema: 1, Version: "0.1.0", Architecture: arch, RuntimeVersion: "0.5.0", RuntimeCommit: "7fe450e19305b828c199d602c23a8337aaa1f03b", Backend: backend, TestedOS: []string{"fixture"}}
	for _, f := range []struct {
		path, purpose string
		data          []byte
	}{
		{"bin/mica-mesh", "cli", fixtureBinary(arch, false)},
		{"runtime/" + arch + "/llama-server", "runtime", fixtureBinary(arch, backend == "metal")},
		{"licenses/llama.cpp.txt", "license", []byte("MIT fixture")},
		{"licenses/third-party.txt", "license", []byte("aggregate fixture")},
	} {
		p := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.data, 0755); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(f.data)
		r.Files = append(r.Files, ReleaseFile{f.path, int64(len(f.data)), fmt.Sprintf("%x", digest), f.purpose})
	}
	writeRelease(t, root, r)
	return root, r
}
func writeRelease(t *testing.T, root string, r Release) []byte {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return data
}
func rejectRelease(t *testing.T, root string) {
	t.Helper()
	if _, err := LoadRelease(context.Background(), root); err == nil {
		t.Fatal("accepted invalid release")
	}
}
func TestReleaseValid(t *testing.T) {
	root, _ := releaseFixture(t)
	got, err := LoadRelease(context.Background(), root)
	if err != nil || got.Version != "0.1.0" || len(got.Files) != 4 {
		t.Fatalf("release = %+v, err = %v", got, err)
	}
}
func TestReleaseRejectsEscapingPath(t *testing.T) {
	for _, path := range []string{"../outside", "/outside", "runtime/../outside", "runtime//file", "./file", "runtime/./file", "runtime\\file"} {
		t.Run(path, func(t *testing.T) {
			root, r := releaseFixture(t)
			r.Files[1].Path = path
			writeRelease(t, root, r)
			rejectRelease(t, root)
		})
	}
}
func TestReleaseRejectsLinks(t *testing.T) {
	for _, target := range []string{"release.json", "runtime", "runtime/" + runtime.GOARCH + "/llama-server"} {
		t.Run(target, func(t *testing.T) {
			root, _ := releaseFixture(t)
			p := filepath.Join(root, target)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.Rename(p, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, p); err != nil {
				t.Fatal(err)
			}
			rejectRelease(t, root)
		})
	}
	t.Run("root", func(t *testing.T) {
		root, _ := releaseFixture(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		rejectRelease(t, link)
	})
}
func TestReleaseSizeBound(t *testing.T) {
	for _, size := range []int{65536, 65537} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root, r := releaseFixture(t)
			data := writeRelease(t, root, r)
			data = append(data, bytes.Repeat([]byte(" "), size-len(data))...)
			if err := os.WriteFile(filepath.Join(root, "release.json"), data, 0644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadRelease(context.Background(), root)
			if (err != nil) != (size > 65536) {
				t.Fatalf("size %d: %v", size, err)
			}
		})
	}
}
func TestReleaseRejectsDuplicateAndUnknownFields(t *testing.T) {
	for _, edit := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"schema":1`, `"schema":1,"schema":1`, 1) },
		func(s string) string { return strings.Replace(s, `"schema":1`, `"schema":1,"Schema":1`, 1) },
		func(s string) string { return strings.Replace(s, `"size_bytes":`, `"unknown":1,"size_bytes":`, 1) },
		func(s string) string { return strings.Replace(s, `"path":`, `"purpose":"cli","path":`, 1) },
		func(s string) string {
			return strings.Replace(s, `"schema":1`, `"schema":1,"model_url":"https://example.test"`, 1)
		},
		func(s string) string { return strings.Replace(s, `"schema":1`, `"schema":null`, 1) },
		func(s string) string { return s + `{}` },
	} {
		root, r := releaseFixture(t)
		data := edit(string(writeRelease(t, root, r)))
		if err := os.WriteFile(filepath.Join(root, "release.json"), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		rejectRelease(t, root)
	}
	root, r := releaseFixture(t)
	r.Files = append(r.Files, r.Files[0])
	writeRelease(t, root, r)
	rejectRelease(t, root)
}
func TestReleaseRequiresCompleteRuntime(t *testing.T) {
	for i := 0; i < 4; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			root, r := releaseFixture(t)
			r.Files = append(r.Files[:i], r.Files[i+1:]...)
			writeRelease(t, root, r)
			rejectRelease(t, root)
		})
	}
	t.Run("unexpected", func(t *testing.T) {
		root, _ := releaseFixture(t)
		if err := os.WriteFile(filepath.Join(root, "extra"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		rejectRelease(t, root)
	})
	t.Run("missing", func(t *testing.T) {
		root, r := releaseFixture(t)
		if err := os.Remove(filepath.Join(root, r.Files[1].Path)); err != nil {
			t.Fatal(err)
		}
		rejectRelease(t, root)
	})
	if runtime.GOARCH == "arm64" {
		t.Run("missing embedded metal", func(t *testing.T) {
			root, r := releaseFixture(t)
			data := fixtureBinary("arm64", false)
			if err := os.WriteFile(filepath.Join(root, r.Files[1].Path), data, 0755); err != nil {
				t.Fatal(err)
			}
			r.Files[1].SizeBytes = int64(len(data))
			sum := sha256.Sum256(data)
			r.Files[1].SHA256 = fmt.Sprintf("%x", sum)
			writeRelease(t, root, r)
			rejectRelease(t, root)
		})
	}
}
func TestReleaseArchitectureMismatch(t *testing.T) {
	root, r := releaseFixture(t)
	r.Architecture = "unknown"
	writeRelease(t, root, r)
	rejectRelease(t, root)
	root, r = releaseFixture(t)
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	data := fixtureBinary(other, false)
	sum := sha256.Sum256(data)
	r.Files[0].SHA256 = fmt.Sprintf("%x", sum)
	r.Files[0].SizeBytes = int64(len(data))
	if err := os.WriteFile(filepath.Join(root, r.Files[0].Path), data, 0755); err != nil {
		t.Fatal(err)
	}
	writeRelease(t, root, r)
	rejectRelease(t, root)
}
func TestReleaseChecksIdentity(t *testing.T) {
	for _, edit := range []func(*Release){
		func(r *Release) { r.RuntimeVersion = "0.5.1" }, func(r *Release) { r.RuntimeCommit = strings.Repeat("0", 40) }, func(r *Release) { r.Backend = "other" }, func(r *Release) { r.Schema = 2 }, func(r *Release) { r.Version = "../outside" }, func(r *Release) { r.TestedOS = nil },
		func(r *Release) { r.Files[1].SHA256 = strings.Repeat("0", 64) }, func(r *Release) { r.Files[1].SizeBytes++ }, func(r *Release) { r.Files[1].Purpose = "license" },
	} {
		root, r := releaseFixture(t)
		edit(&r)
		writeRelease(t, root, r)
		rejectRelease(t, root)
	}
}
func TestReleaseCancellation(t *testing.T) {
	root, _ := releaseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LoadRelease(ctx, root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestReleaseRejectsExternalDependencies(t *testing.T) {
	for _, command := range []uint32{0xc, 0x80000018, 0x8000001f, 0x80000023, 0x20, 0x8000001c} {
		t.Run(fmt.Sprintf("command%x", command), func(t *testing.T) {
			root, r := releaseFixture(t)
			data := fixtureBinary(runtime.GOARCH, false)
			name := []byte("/opt/homebrew/lib/unsafe.dylib\x00")
			fixed := 24
			if command == 0x8000001c {
				fixed = 12
			}
			size := (fixed + len(name) + 7) &^ 7
			binary.LittleEndian.PutUint32(data[16:20], 1)
			binary.LittleEndian.PutUint32(data[20:24], uint32(size))
			load := make([]byte, size)
			binary.LittleEndian.PutUint32(load[:4], command)
			binary.LittleEndian.PutUint32(load[4:8], uint32(size))
			binary.LittleEndian.PutUint32(load[8:12], uint32(fixed))
			copy(load[fixed:], name)
			data = append(data, load...)
			sum := sha256.Sum256(data)
			r.Files[0].SHA256 = fmt.Sprintf("%x", sum)
			r.Files[0].SizeBytes = int64(len(data))
			if err := os.WriteFile(filepath.Join(root, r.Files[0].Path), data, 0755); err != nil {
				t.Fatal(err)
			}
			writeRelease(t, root, r)
			rejectRelease(t, root)
		})
	}
}

func TestReleaseRejectsMalformedSymbolTable(t *testing.T) {
	root, r := releaseFixture(t)
	data := fixtureBinary(runtime.GOARCH, false)
	binary.LittleEndian.PutUint32(data[16:20], 1)
	binary.LittleEndian.PutUint32(data[20:24], 24)
	load := make([]byte, 24)
	binary.LittleEndian.PutUint32(load[:4], 2)
	binary.LittleEndian.PutUint32(load[4:8], 24)
	binary.LittleEndian.PutUint32(load[12:16], 0xffffffff)
	data = append(data, load...)
	sum := sha256.Sum256(data)
	r.Files[0].SHA256 = fmt.Sprintf("%x", sum)
	r.Files[0].SizeBytes = int64(len(data))
	if err := os.WriteFile(filepath.Join(root, r.Files[0].Path), data, 0755); err != nil {
		t.Fatal(err)
	}
	writeRelease(t, root, r)
	rejectRelease(t, root)
}
func TestReleaseRejectsHardLinks(t *testing.T) {
	root, r := releaseFixture(t)
	p := filepath.Join(root, r.Files[0].Path)
	if err := os.Link(p, filepath.Join(t.TempDir(), "outside")); err != nil {
		t.Fatal(err)
	}
	rejectRelease(t, root)
}
