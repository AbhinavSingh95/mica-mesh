package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	"github.com/bazelbuild/rules_go/go/runfiles"
	"golang.org/x/sys/unix"
)

func openPTY(t *testing.T) (*os.File, *os.File, int) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "pty-master")
	t.Cleanup(func() { master.Close() })
	for _, op := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(fd, op, 0); err != nil {
			t.Fatal(err)
		}
	}
	// x/sys has no Darwin ptsname wrapper. This test-only ioctl fills its fixed
	// 128-byte kernel buffer; the pointer remains live through the syscall.
	var name [128]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		t.Fatal(errno)
	}
	slaveFD, err := unix.Open(string(bytes.TrimRight(name[:], "\x00")), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	slave := os.NewFile(uintptr(slaveFD), "pty-slave")
	t.Cleanup(func() { slave.Close() })
	return master, slave, slaveFD
}

// consentBundle puts the declared executable in a verified local release.
// The runtime is a minimal Mach-O fixture. Consent must never execute it.
func consentBundle(t *testing.T) (string, string, string) {
	t.Helper()
	source, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := t.TempDir()
	arch := runtime.GOARCH
	backend := "cpu"
	if arch == "arm64" {
		backend = "metal"
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	release := setup.Release{Schema: 1, Version: "0.1.0", Architecture: arch, Backend: backend, RuntimeVersion: "0.5.0", RuntimeCommit: "7fe450e19305b828c199d602c23a8337aaa1f03b", TestedOS: []string{"test fixture"}}
	for _, entry := range []struct {
		path, purpose string
		data          []byte
	}{{"bin/mica-mesh", "cli", payload}, {"runtime/" + arch + "/llama-server", "runtime", consentRuntime(arch)}, {"licenses/llama.cpp.txt", "license", []byte("MIT fixture")}, {"licenses/third-party.txt", "license", []byte("fixture")}} {
		path := filepath.Join(root, entry.path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, entry.data, 0755); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(entry.data)
		release.Files = append(release.Files, setup.ReleaseFile{Path: entry.path, Purpose: entry.purpose, SizeBytes: int64(len(entry.data)), SHA256: fmt.Sprintf("%x", digest)})
	}
	data, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "bin", "mica-mesh"), home, root
}
func consentRuntime(arch string) []byte {
	cpu := uint32(macho.CpuAmd64)
	count, size := uint32(0), uint32(0)
	if arch == "arm64" {
		cpu = uint32(macho.CpuArm64)
		count = 1
		size = 152
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint32{macho.Magic64, cpu, 0, uint32(macho.TypeExec), count, size, 0, 0})
	if arch == "arm64" {
		binary.Write(&b, binary.LittleEndian, []uint32{uint32(macho.LoadCmdSegment64), 152})
		var name [16]byte
		copy(name[:], "__DATA")
		b.Write(name[:])
		binary.Write(&b, binary.LittleEndian, []uint64{0, 1, 184, 1})
		binary.Write(&b, binary.LittleEndian, []uint32{0, 0, 1, 0})
		name = [16]byte{}
		copy(name[:], "__ggml_metallib")
		b.Write(name[:])
		name = [16]byte{}
		copy(name[:], "__DATA")
		b.Write(name[:])
		binary.Write(&b, binary.LittleEndian, []uint64{0, 1})
		binary.Write(&b, binary.LittleEndian, []uint32{184, 0, 0, 0, 0, 0, 0, 0})
		b.WriteByte('x')
	}
	return b.Bytes()
}
func TestSetupConsentInterrupt(t *testing.T) { testConsentEnd(t, "interrupt") }
func TestSetupConsentSIGTERM(t *testing.T)   { testConsentEnd(t, "term") }
func TestSetupConsentEOF(t *testing.T)       { testConsentEnd(t, "eof") }
func testConsentEnd(t *testing.T, ending string) {
	t.Helper()
	binary, home, root := consentBundle(t)
	master, slave, fd := openPTY(t)
	// Darwin sets a sticky kernel flag on the first terminal write. Prime the
	// terminal before the baseline so full flag equality measures CLI restoration.
	if _, err := slave.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	var originalFlags int
	var originalTerm *unix.Termios
	statusR, statusW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusR.Close()
	defer statusW.Close()
	releaseR, releaseW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseR.Close()
	defer releaseW.Close()
	// Keep the session leader alive after the CLI exits: Darwin otherwise revokes
	// the terminal before the test can inspect its flags and termios. The shell
	// ignores Ctrl-C; Go's signal owner enables it in the CLI child.
	script := `trap '' INT
printf 'ready\n' >&3
read start <&4
"$1" setup <&0 &
child=$!
printf '%s\n' "$child" >&3
wait "$child"
printf '%s\n' "$?" >&3
read release <&4
`
	cmd := exec.Command("/bin/sh", "-c", script, "session-owner", binary)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.ExtraFiles = []*os.File{statusW, releaseR}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	waited := false
	go func() { done <- cmd.Wait() }()
	var childProcess *os.Process
	t.Cleanup(func() {
		if !waited {
			if childProcess != nil {
				_ = childProcess.Kill()
			}
			master.Close()
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("session owner was not reaped")
			}
		}
	})
	statusW.Close()
	releaseR.Close()
	if err := statusR.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	status := bufio.NewReaderSize(statusR, 128)
	readyLine, err := status.ReadString('\n')
	if err != nil || readyLine != "ready\n" {
		t.Fatalf("session barrier: %q %v", readyLine, err)
	}
	originalFlags = flags(t, fd)
	originalTerm, err = unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseW.Write([]byte("start\n")); err != nil {
		t.Fatal(err)
	}
	pidLine, err := status.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidLine))
	if err != nil {
		t.Fatal(err)
	}
	childProcess, err = os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := master.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	var buf [512]byte
	for !strings.Contains(output.String(), "[y/N]") {
		n, err := master.Read(buf[:])
		output.Write(buf[:n])
		if err != nil {
			t.Fatalf("no consent prompt: %v; %s", err, output.String())
		}
	}
	// Continue draining output through shutdown; every test reader is bounded.
	drained := make(chan struct{})
	go func() { defer close(drained); _, _ = io.Copy(io.Discard, master) }()
	defer func() { _ = master.SetReadDeadline(time.Now()); <-drained }()
	switch ending {
	case "interrupt":
		_, err = master.Write([]byte{originalTerm.Cc[unix.VINTR]})
	case "term":
		err = childProcess.Signal(syscall.SIGTERM)
	case "eof":
		_, err = master.Write([]byte{originalTerm.Cc[unix.VEOF]})
	}
	if err != nil {
		t.Fatal(err)
	}
	exitLine, err := status.ReadString('\n')
	if err != nil {
		t.Fatalf("consent reader did not stop: %v", err)
	}
	childProcess.Release()
	childProcess = nil
	if strings.TrimSpace(exitLine) != "1" {
		t.Fatalf("CLI did not exit cooperatively with failure: %q", exitLine)
	}
	if got := flags(t, fd); got != originalFlags {
		t.Errorf("flags=%#x want=%#x", got, originalFlags)
	}
	term, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil || *term != *originalTerm {
		t.Errorf("terminal state changed: %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Errorf("setup changed home: %v %v", entries, err)
	}
	if _, err := setup.LoadRelease(context.Background(), root); err != nil {
		t.Errorf("release changed: %v", err)
	}
	if _, err := releaseW.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		waited = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session owner did not stop")
	}
}
func TestUnreadCommandWithClosedStdin(t *testing.T) {
	binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--help"}, {"setup", "--help"}, {"doctor", "--role", "client"}} {
		cmd := exec.Command("/bin/sh", append([]string{"-c", "exec 0<&-; exec \"$@\"", "sh", binary}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
}
