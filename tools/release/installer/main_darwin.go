// The installer helper is a package script asset, never an installed service.
package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func run(ctx context.Context, args []string) (err error) {
	if len(args) == 2 && args[0] == "build-info" {
		info, err := buildinfo.ReadFile(args[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	if len(args) == 3 && args[0] == "verify" {
		_, err := verify(ctx, args[1], args[2])
		return err
	}
	if len(args) != 4 || (args[0] != "preinstall" && args[0] != "postinstall") {
		return errors.New("invalid installer arguments; run the native package installer")
	}
	if os.Geteuid() != 0 || args[3] != "/" {
		return errors.New("install on the startup volume with administrator authorization")
	}
	translated, err := unix.SysctlUint32("sysctl.proc_translated")
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if translated == 1 {
		return errors.New("Rosetta is unsupported; use the package for this Mac's native architecture")
	}
	paths := layout{root: "/", uid: 0}
	if args[0] == "postinstall" {
		return install(ctx, paths, args[1], args[2])
	}
	if _, err := verify(ctx, args[1], args[2]); err != nil {
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
	return ownedLink(ctx, paths, bin)
}

func main() {
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(signals, 10*time.Minute)
	err := run(ctx, os.Args[1:])
	cancel()
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Mica Mesh installation failed:", err)
		os.Exit(1)
	}
}
