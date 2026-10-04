package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type crashInput struct{ Root, Source, Digest, Phase string }

func TestInstallerUmaskFixture(t *testing.T) {
	input := os.Getenv("MICA_INSTALLER_UMASK_FIXTURE")
	if input == "" {
		return
	}
	var in crashInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatal(err)
	}
	// Umask is process-wide. Change it only in this isolated child.
	unix.Umask(0077)
	if err := install(context.Background(), layout{root: in.Root, uid: uint32(os.Getuid())}, in.Source, in.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictiveUmaskStillPublishesAccessibleRelease(t *testing.T) {
	paths := testLayout(t)
	source, digest := fixture(t, "0.1.0", "notice")
	encoded, err := json.Marshal(crashInput{Root: paths.root, Source: source, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstallerUmaskFixture$")
	cmd.Env = append(os.Environ(), "MICA_INSTALLER_UMASK_FIXTURE="+string(encoded))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install with umask 077: %v\n%s", err, output)
	}
	err = filepath.WalkDir(paths.root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == ".staging" {
			return filepath.SkipDir
		}
		if entry.Name() == ".install.lock" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		required := os.FileMode(0444)
		if entry.IsDir() {
			required = 0111
		} else if entry.Name() == "mica-mesh" || entry.Name() == "llama-server" {
			required = 0555
		}
		if info.Mode().Perm()&required != required {
			t.Errorf("installed path %s mode %o lacks public access %o", name, info.Mode().Perm(), required)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The child uses the same real filesystem operations as the helper. Its pipe
// barrier exists only in tests, at an ownership boundary where a crash matters.
func TestInstallerCrashFixture(t *testing.T) {
	input := os.Getenv("MICA_INSTALLER_CRASH_FIXTURE")
	if input == "" {
		return
	}
	var in crashInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatal(err)
	}
	paths := layout{root: in.Root, uid: uint32(os.Getuid())}
	root, err := managedDirectory(paths, releases)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := acquireLock(root, paths.uid)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if in.Phase != "lock" {
		staging, err := openDirectory(root, ".staging", paths.uid, 0700)
		if err != nil {
			t.Fatal(err)
		}
		defer staging.Close()
		stage, err := stageRelease(context.Background(), paths, in.Source, in.Digest, staging)
		if err != nil {
			t.Fatal(err)
		}
		if in.Phase == "version" {
			if _, err := activateVersion(context.Background(), paths, root, stage, in.Digest); err != nil {
				t.Fatal(err)
			}
		}
	}
	fmt.Fprintln(os.Stdout, "owned-boundary")
	var gate [1]byte
	if _, err := os.Stdin.Read(gate[:]); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash fixture unexpectedly resumed")
}

func TestProcessInterruptionKeepsOldLinkAndReleasesLock(t *testing.T) {
	for _, phase := range []string{"lock", "stage", "version"} {
		t.Run(phase, func(t *testing.T) {
			paths := testLayout(t)
			old, oldDigest := fixture(t, "0.1.0", "old")
			if err := install(context.Background(), paths, old, oldDigest); err != nil {
				t.Fatal(err)
			}
			next, nextDigest := fixture(t, "0.2.0", "next")
			encoded, err := json.Marshal(crashInput{paths.root, next, nextDigest, phase})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstallerCrashFixture$")
			cmd.Env = append(os.Environ(), "MICA_INSTALLER_CRASH_FIXTURE="+string(encoded))
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer writer.Close()
			cmd.Stdin = input
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Read the barrier before killing. Wait observes/reaps the owned process;
			// cancellation and closing output join the reader even on a failed barrier.
			ready := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(output).ReadString('\n'); ready <- line }()
			var line string
			select {
			case line = <-ready:
			case <-ctx.Done():
			}
			killErr := cmd.Process.Kill()
			waitErr := cmd.Wait()
			_ = output.Close()
			if line == "" {
				select {
				case <-ready:
				case <-time.After(time.Second):
					t.Fatal("barrier reader did not stop")
				}
			}
			if killErr != nil || waitErr == nil || line != "owned-boundary\n" {
				t.Fatalf("child boundary %q: kill %v, wait %v", line, killErr, waitErr)
			}
			active, err := os.Readlink(filepath.Join(paths.root, commandDir, "mica-mesh"))
			if err != nil || active != filepath.Join(paths.root, releases, "0.1.0/bin/mica-mesh") {
				t.Fatalf("crash lost old executable: %q, %v", active, err)
			}
			if err := install(context.Background(), paths, next, nextDigest); err != nil {
				t.Fatalf("retry after crash: %v", err)
			}
		})
	}
}
