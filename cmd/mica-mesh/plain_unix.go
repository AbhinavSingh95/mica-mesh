//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// plainInvocation owns signal interception before descriptor mutation through
// restoration. Main receives its context and never adds another signal owner.
type plainInvocation struct {
	ctx         context.Context
	cancel      context.CancelFunc
	stopSignals context.CancelFunc
	input       *ownedInput
	outputs     []ownedOutput
	closeOnce   sync.Once
	closeErr    error
}

// openPlain always returns an owner, even on setup failure, so the caller can
// report that failure while signals are still intercepted, then close the owner.
func openPlain(parent context.Context) (*plainInvocation, error) {
	ctx, cancel := context.WithCancel(parent)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	p := &plainInvocation{ctx: ctx, cancel: cancel, stopSignals: stop, input: captureInput(0)}
	var err error
	p.outputs, err = ownOutputs(1, 2)
	return p, err
}

// close follows Main's join and clears interception only after restoring I/O.
func (p *plainInvocation) close() error {
	p.closeOnce.Do(func() {
		defer p.stopSignals()
		defer p.cancel()
		p.closeErr = errors.Join(p.input.close(), closeOutputs(p.outputs))
	})
	return p.closeErr
}
