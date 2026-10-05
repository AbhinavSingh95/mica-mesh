package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"golang.org/x/sys/unix"
)

func TestGuidedPTYRestoresTerminal(t *testing.T) {
	binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	for _, ending := range []string{"key", "interrupt", "term"} {
		t.Run(ending, func(t *testing.T) {
			master, slave, fd := openPTY(t)
			slave.Write([]byte("\n"))
			unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: 90, Row: 24})
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
			// Keep the session leader alive while inspecting the child's restored PTY.
			script := `trap '' INT
printf 'ready\n' >&3
read start <&4
"$1" ui <&0 &
child=$!
printf '%s\n' "$child" >&3
wait "$child"
printf '%s\n' "$?" >&3
read release <&4
`
			cmd := exec.Command("/bin/sh", "-c", script, "session-owner", binary)
			cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=1", "HOME="+t.TempDir())
			cmd.Stdin = slave
			cmd.Stdout = slave
			cmd.Stderr = slave
			cmd.ExtraFiles = []*os.File{statusW, releaseR}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var child *os.Process
			waited := false
			defer func() {
				if !waited {
					if child != nil {
						child.Kill()
					}
					cmd.Process.Kill()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("session leader not reaped")
					}
				}
			}()
			statusW.Close()
			releaseR.Close()
			statusR.SetReadDeadline(time.Now().Add(5 * time.Second))
			status := bufio.NewReader(statusR)
			if line, err := status.ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("barrier: %q %v", line, err)
			}
			originalFlags := flags(t, fd)
			originalState, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(releaseW, "start")
			pidLine, err := status.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(pidLine))
			if err != nil {
				t.Fatal(err)
			}
			child, _ = os.FindProcess(pid)
			master.SetReadDeadline(time.Now().Add(5 * time.Second))
			var output strings.Builder
			var buf [1024]byte
			for !strings.Contains(output.String(), "Ctrl-C or q") {
				n, err := master.Read(buf[:])
				output.Write(buf[:n])
				if err != nil {
					t.Fatalf("no guided view: %v %q", err, output.String())
				}
			}
			drained := make(chan struct{})
			go func() { defer close(drained); io.Copy(io.Discard, master) }()
			defer func() { master.SetReadDeadline(time.Now()); <-drained }()
			switch ending {
			case "key":
				_, err = master.Write([]byte("q"))
			case "interrupt":
				_, err = master.Write([]byte{3})
			case "term":
				err = child.Signal(syscall.SIGTERM)
			}
			if err != nil {
				t.Fatal(err)
			}
			line, err := status.ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "0" {
				t.Fatalf("guided exit: %q %v", line, err)
			}
			restored, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
			if err != nil || *restored != *originalState || flags(t, fd) != originalFlags {
				t.Fatalf("terminal not restored: %v", err)
			}
			child.Release()
			child = nil
			fmt.Fprintln(releaseW, "release")
			select {
			case err := <-done:
				waited = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("session did not join")
			}
		})
	}
}
func TestExplicitUIRejectsPipesWithoutHiddenTTY(t *testing.T) {
	binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"ui"}, {"--plain", "ui"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, binary, args...)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(out), "ui needs terminal") {
			t.Fatalf("args=%v err=%v output=%q", args, err, out)
		}
	}
}
