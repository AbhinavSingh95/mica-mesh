//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"time"
)

// ownedInput captures flags before output setup can mutate a shared terminal.
// Initialization stays lazy so unread commands tolerate a closed stdin.
// The reader and its deadline callback must be joined before close.
type ownedInput struct {
	originalFD, flags int
	captureErr        error
	file              *os.File
	fd                int
}

func captureInput(fd int) *ownedInput {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	return &ownedInput{originalFD: fd, flags: flags, captureErr: err}
}
func (i *ownedInput) initialize() error {
	if i.captureErr != nil {
		return i.captureErr
	}
	if i.file != nil {
		return nil
	}
	fd, err := unix.FcntlInt(uintptr(i.originalFD), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, i.flags|unix.O_NONBLOCK); err != nil {
		return errors.Join(err, unix.Close(fd))
	}
	i.fd = fd
	i.file = os.NewFile(uintptr(fd), "mica-mesh-input")
	return nil
}
func (i *ownedInput) IsTerminal() (bool, error) {
	if err := i.initialize(); err != nil {
		return false, err
	}
	return isTerminal(i.fd), nil
}
func (i *ownedInput) Read(data []byte) (int, error) {
	if err := i.initialize(); err != nil {
		return 0, err
	}
	return i.file.Read(data)
}
func (i *ownedInput) SetReadDeadline(deadline time.Time) error {
	if err := i.initialize(); err != nil {
		return err
	}
	if err := i.file.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("stdin cannot support cancellation: %w", err)
	}
	return nil
}
func (i *ownedInput) close() error {
	if i.file == nil {
		return nil
	}
	_, err := unix.FcntlInt(uintptr(i.fd), unix.F_SETFL, i.flags)
	return errors.Join(err, i.file.Close())
}
