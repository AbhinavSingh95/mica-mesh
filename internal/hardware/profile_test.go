package hardware

import (
	"context"
	"testing"
	"time"
)

func TestProfileCanceledIsNonNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	p := Profile(ctx)
	if p == nil || p.Architecture == "" || p.Hostname == "" {
		t.Fatalf("profile=%v", p)
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled profile blocked")
	}
}
