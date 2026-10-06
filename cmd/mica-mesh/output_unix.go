//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type ownedOutput struct {
	file  *os.File
	fd    int
	flags int
}

// ownOutputs duplicates inherited descriptors above stdio and prepares their
// shared open descriptions for Go polling. Capture both original flags before
// changing either: stdout/stderr may share one open description via 2>&1.
func ownOutputs(stdoutFD, stderrFD int) ([]ownedOutput, error) {
	originals := []int{stdoutFD, stderrFD}
	flags := make([]int, len(originals))
	for i, fd := range originals {
		value, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return nil, err
		}
		flags[i] = value
	}
	var outputs []ownedOutput
	fail := func(err error) ([]ownedOutput, error) { return nil, errors.Join(err, closeOutputs(outputs)) }
	for i, fd := range originals {
		duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
		if err != nil {
			return fail(err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(duplicate, &stat); err != nil {
			return fail(errors.Join(err, unix.Close(duplicate)))
		}
		knownSink := stat.Mode&unix.S_IFMT == unix.S_IFREG || isNullOutput(stat)
		if !knownSink {
			if _, err := unix.FcntlInt(uintptr(duplicate), unix.F_SETFL, flags[i]|unix.O_NONBLOCK); err != nil {
				return fail(errors.Join(err, unix.Close(duplicate)))
			}
		}
		file := os.NewFile(uintptr(duplicate), fmt.Sprintf("mica-mesh-output-%d", fd))
		outputs = append(outputs, ownedOutput{file: file, fd: duplicate, flags: flags[i]})
		if !knownSink {
			if err := file.SetWriteDeadline(time.Time{}); err != nil {
				return fail(fmt.Errorf("stdio fd %d cannot support cancellation: %w", fd, err))
			}
		}
	}
	return outputs, nil
}
func closeOutputs(outputs []ownedOutput) (err error) {
	// Main has joined deadline callbacks and writes before restoration. Use the
	// captured raw fd; calling File.Fd during use could disable Go polling.
	for i := len(outputs) - 1; i >= 0; i-- {
		o := outputs[i]
		_, restoreErr := unix.FcntlInt(uintptr(o.fd), unix.F_SETFL, o.flags)
		err = errors.Join(err, restoreErr, o.file.Close())
	}
	return err
}

// A single nonblocking diagnostic attempt keeps setup failure reporting bounded
// even when the inherited stderr is itself a full pipe. Restore shared flags.
func reportOutputError(err error) {
	flags, flagErr := unix.FcntlInt(2, unix.F_GETFL, 0)
	if flagErr != nil {
		return
	}
	if _, flagErr = unix.FcntlInt(2, unix.F_SETFL, flags|unix.O_NONBLOCK); flagErr != nil {
		return
	}
	_, _ = unix.Write(2, []byte(fmt.Sprintf("mica-mesh: %v\n", err)))
	_, _ = unix.FcntlInt(2, unix.F_SETFL, flags)
}

// isNullOutput compares descriptor identity with the actual null device. Names
// supplied to os.NewFile are not evidence that a destination cannot block.
func isNullOutput(stat unix.Stat_t) bool {
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		return false
	}
	var null unix.Stat_t
	return unix.Stat("/dev/null", &null) == nil && stat.Dev == null.Dev && stat.Ino == null.Ino && stat.Rdev == null.Rdev
}

type diagnosticSink struct {
	file   *os.File
	cancel context.CancelFunc
	failed atomic.Bool
}

func (s *diagnosticSink) Write(data []byte) (int, error) {
	n, err := s.file.Write(data)
	if err != nil {
		s.failed.Store(true)
		s.cancel()
	}
	return n, err
}

// ownLogger's synchronous sink participates in CLI process output deadlines and
// cancels the process on diagnostic failure. Restore only after every service
// owner has joined, including standard-log bridge settings changed by SetDefault.
func ownLogger(stderr *os.File, cancel context.CancelFunc) (func(), func() bool) {
	sink := &diagnosticSink{file: stderr, cancel: cancel}
	return installLogger(sink), sink.failed.Load
}

func installLogger(sink io.Writer) func() {
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, nil)))
	return func() { slog.SetDefault(previous); log.SetOutput(writer); log.SetFlags(flags) }
}
