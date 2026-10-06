package integration

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/AbhinavSingh95/mica-mesh/internal/client"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A joined local display failure must not make the selected runtime reusable
// before its remote cleanup finishes. The same session connection can then
// accept a fresh request, without replaying the failed one.
func TestSessionOutputFailureKeepsRemoteCleanupOwned(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	rt := newRuntime()
	delta, cleanup := f.gate(), f.gate()
	rt.deltaGate, rt.CleanupGate = delta.ch, cleanup.ch
	w := f.worker(rt)
	f.runtimeReady(w)
	f.membership(c, w)
	f.ready(c, 1)
	session, err := client.New(c.rpc.address)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	req := request()
	var partialText strings.Builder
	err = session.Generate(f.ctx, req, func(e *meshv1.InferenceEvent) error {
		if d := e.GetTextDelta(); d != nil {
			partialText.WriteString(d.Text)
			return io.ErrClosedPipe
		}
		return nil
	})
	if !errors.Is(err, io.ErrClosedPipe) || partialText.String() != "partial 🌏" {
		t.Fatalf("consumer result: %q %v", partialText.String(), err)
	}
	f.signal(rt.Cleaning)
	if report := w.svc.Report(); !report.Active || report.GetActiveRequestId() != req.RequestId {
		t.Fatalf("local display failure erased remote ownership: %v", report)
	}
	err = session.Generate(f.ctx, request(), func(*meshv1.InferenceEvent) error { return nil })
	if code := status.Code(err); code != codes.Unavailable && code != codes.ResourceExhausted {
		t.Fatalf("cleanup admission returned %v", err)
	}
	if rt.attempts() != 1 {
		t.Fatal("failed display request replayed or cleanup overlapped")
	}
	cleanup.open()
	delta.open()
	f.signal(rt.Released)
	f.ready(c, 1)
	var output strings.Builder
	err = session.Generate(f.ctx, request(), func(e *meshv1.InferenceEvent) error {
		if d := e.GetTextDelta(); d != nil {
			output.WriteString(d.Text)
		}
		return nil
	})
	if err != nil || output.String() != "partial 🌏" {
		t.Fatalf("reused session result: %q %v", output.String(), err)
	}
	if got := rt.Counters(); got.Active != 0 || got.Peak != 1 || rt.attempts() != 2 {
		t.Fatalf("session reuse ownership: %+v attempts=%d", got, rt.attempts())
	}
}
