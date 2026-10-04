package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var fixtureModel = []byte("GGUF small fixed model fixture\n")

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func setupFixture(t *testing.T, client *http.Client, url string) *Manager {
	t.Helper()
	root, release := releaseFixture(t)
	m := New(Layout{ReleaseRoot: root, DataRoot: canonicalTempDir(t)}, release, client)
	sum := sha256.Sum256(fixtureModel)
	m.model = modelDescriptor{id: "fixture-model", size: int64(len(fixtureModel)), digest: fmt.Sprintf("%x", sum), url: url}
	return m
}
func fixtureServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Cleanup(s.Close)
	return s
}
func activeModel(m *Manager) string {
	return filepath.Join(m.layout.DataRoot, "models", m.model.digest, "model.gguf")
}
func activeReceipt(m *Manager) string {
	return filepath.Join(m.layout.DataRoot, "installations", m.release.Version+"-"+m.release.Architecture+".json")
}
func requireAbsent(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", name, err)
	}
}
func requireFile(t *testing.T, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file %s = %q: %v", name, got, err)
	}
}
func writeModel(t *testing.T, m *Manager, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(activeModel(m)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeModel(m), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func requireCleanStaging(t *testing.T, m *Manager) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.layout.DataRoot, "staging"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging contains %v", entries)
	}
}

func TestSetupDownloadsAndActivates(t *testing.T) {
	var calls atomic.Int32
	s := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(fixtureModel) })
	m := setupFixture(t, s.Client(), s.URL)
	var phases []Phase
	got, err := m.Prepare(context.Background(), func(p Progress) error {
		phases = append(phases, p.Phase)
		if p.TotalBytes != int64(len(fixtureModel)) || p.CompletedBytes > p.TotalBytes {
			t.Errorf("bad progress %+v", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelPath != activeModel(m) || got.RuntimeBinary != filepath.Join(m.layout.ReleaseRoot, "runtime", m.release.Architecture, "llama-server") || got.Backend != m.release.Backend || got.Version != "0.1.0" {
		t.Fatalf("installation %+v", got)
	}
	requireFile(t, got.ModelPath, fixtureModel)
	data, err := os.ReadFile(activeReceipt(m))
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]json.RawMessage
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "version", "architecture", "backend", "files", "model", "verified_at"} {
		if len(receipt[key]) == 0 {
			t.Errorf("missing receipt %s", key)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("calls %d", calls.Load())
	}
	if len(phases) == 0 || phases[0] != Checking || phases[len(phases)-1] != Complete {
		t.Errorf("phases %v", phases)
	}
	for _, name := range []string{activeReceipt(m), activeModel(m)} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Errorf("permissions %s: %v %v", name, info, err)
		}
	}
	requireCleanStaging(t, m)
}
func TestSetupReusesVerifiedModelOffline(t *testing.T) {
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("offline reuse used network")
		return nil, errors.New("offline")
	})}, "https://fixture.invalid/model")
	writeModel(t, m, fixtureModel)
	before, _ := os.Stat(activeModel(m))
	_, err := m.Prepare(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(activeModel(m))
	if !os.SameFile(before, after) {
		t.Fatal("replaced valid model")
	}
	if _, err := os.Stat(activeReceipt(m)); err != nil {
		t.Fatal(err)
	}
}
func TestSetupRejectsWrongDigestAndSize(t *testing.T) {
	for _, body := range [][]byte{bytes.Repeat([]byte{'x'}, len(fixtureModel)), fixtureModel[:3], append(bytes.Clone(fixtureModel), 'x')} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			var calls atomic.Int32
			s := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(body) })
			m := setupFixture(t, s.Client(), s.URL)
			if _, err := m.Prepare(context.Background(), nil); err == nil {
				t.Fatal("accepted bad bytes")
			}
			requireAbsent(t, activeModel(m))
			requireAbsent(t, activeReceipt(m))
			requireCleanStaging(t, m)
			if calls.Load() != 1 {
				t.Fatalf("download retried %d times", calls.Load())
			}
		})
	}
}
func TestSetupRejectsHTTPRedirect(t *testing.T) {
	var calls atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(fixtureModel) }))
	defer plain.Close()
	s := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, http.StatusFound) })
	m := setupFixture(t, s.Client(), s.URL)
	if _, err := m.Prepare(context.Background(), nil); err == nil {
		t.Fatal("accepted HTTP redirect")
	}
	if calls.Load() != 0 {
		t.Fatal("contacted HTTP target")
	}
}
func TestSetupRedirectLimit(t *testing.T) {
	for _, redirects := range []int{5, 6} {
		t.Run(fmt.Sprint(redirects), func(t *testing.T) {
			var calls atomic.Int32
			s := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1))
				if n <= redirects {
					http.Redirect(w, r, fmt.Sprintf("/hop/%d", n), 302)
					return
				}
				w.Write(fixtureModel)
			})
			m := setupFixture(t, s.Client(), s.URL)
			_, err := m.Prepare(context.Background(), nil)
			if (err != nil) != (redirects > 5) {
				t.Fatalf("redirects %d: %v", redirects, err)
			}
			if calls.Load() > 6 {
				t.Fatalf("too many calls %d", calls.Load())
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type stalledBody struct {
	ctx     context.Context
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	first   bool
}

func (b *stalledBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		copy(p, fixtureModel[:3])
		close(b.started)
		return 3, nil
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}
func (b *stalledBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }
func stalledManager(t *testing.T) (*Manager, chan struct{}, chan struct{}) {
	t.Helper()
	started, closed := make(chan struct{}), make(chan struct{})
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: make(http.Header), Body: &stalledBody{ctx: r.Context(), started: started, closed: closed}}, nil
	})}, "https://fixture.invalid/model")
	return m, started, closed
}
func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("barrier timeout")
	}
}
func TestSetupCancelPreservesInstallation(t *testing.T) {
	m, started, closed := stalledManager(t)
	valid := filepath.Join(m.layout.DataRoot, "models", "older", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(valid), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valid, fixtureModel, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.Prepare(ctx, nil); done <- err }()
	await(t, started)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not join")
	}
	await(t, closed)
	requireAbsent(t, activeReceipt(m))
	requireAbsent(t, activeModel(m))
	requireFile(t, valid, fixtureModel)
	requireCleanStaging(t, m)
	writeModel(t, m, fixtureModel)
	if _, err := m.Prepare(context.Background(), nil); err != nil {
		t.Fatalf("lock reuse: %v", err)
	}
}
func TestSetupConcurrentAttemptFailsImmediately(t *testing.T) {
	m, started, _ := stalledManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.Prepare(ctx, nil); done <- err }()
	await(t, started)
	second := New(m.layout, m.release, m.client)
	second.model = m.model
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := second.Prepare(ctx2, nil); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overlap = %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not finish")
	}
}
func TestSetupHonorsEarlierDeadline(t *testing.T) {
	m, _, closed := stalledManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := m.Prepare(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline = %v", err)
	}
	await(t, closed)
	requireCleanStaging(t, m)
}
func TestSetupIdleTimeout(t *testing.T) {
	m, _, closed := stalledManager(t)
	m.idleTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Prepare(ctx, nil); err == nil || errors.Is(err, ctx.Err()) {
		t.Fatalf("idle = %v", err)
	}
	await(t, closed)
	requireCleanStaging(t, m)
}
func TestSetupResponseHeaderTimeout(t *testing.T) {
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}, "https://fixture.invalid/model")
	m.headerTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Prepare(ctx, nil); err == nil || errors.Is(err, ctx.Err()) {
		t.Fatalf("header timeout = %v", err)
	}
	requireCleanStaging(t, m)
}
func TestSetupDiskFull(t *testing.T) {
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("network called without free space")
		return nil, errors.New("unexpected network")
	})}, "https://fixture.invalid/model")
	m.model.size = math.MaxInt64 - (64 * 1024 * 1024)
	if _, err := m.Prepare(context.Background(), nil); !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("disk full = %v", err)
	}
	requireAbsent(t, activeReceipt(m))
	requireAbsent(t, activeModel(m))
}
func TestSetupPermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	m := setupFixture(t, nil, "https://fixture.invalid/model")
	if err := os.Chmod(m.layout.DataRoot, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(m.layout.DataRoot, 0700)
	if _, err := m.Prepare(context.Background(), nil); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("permission = %v", err)
	}
	requireAbsent(t, activeReceipt(m))
}
func TestSetupCrashStagingCleanup(t *testing.T) {
	m := setupFixture(t, nil, "https://fixture.invalid/model")
	writeModel(t, m, fixtureModel)
	stage := filepath.Join(m.layout.DataRoot, "staging")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(stage, "setup-crashed")
	if err := os.WriteFile(stale, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, stale)
	requireFile(t, activeModel(m), fixtureModel)
}
func TestSetupDoesNotReplaceCorruptActiveFile(t *testing.T) {
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("network for corrupt active model")
		return nil, errors.New("unexpected network")
	})}, "https://fixture.invalid/model")
	writeModel(t, m, []byte("corrupt"))
	if _, err := m.Prepare(context.Background(), nil); err == nil {
		t.Fatal("accepted corrupt active model")
	}
	requireFile(t, activeModel(m), []byte("corrupt"))
	requireAbsent(t, activeReceipt(m))
}
func TestSetupProgressFailureStopsDownload(t *testing.T) {
	body := &pacedBody{Reader: bytes.NewReader(fixtureModel)}
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: make(http.Header), Body: body}, nil
	})}, "https://fixture.invalid/model")
	stopped := errors.New("stop progress")
	if _, err := m.Prepare(context.Background(), func(p Progress) error {
		if p.Phase == Downloading {
			return stopped
		}
		return nil
	}); !errors.Is(err, stopped) {
		t.Fatalf("progress failure = %v", err)
	}
	requireAbsent(t, activeModel(m))
	requireAbsent(t, activeReceipt(m))
	requireCleanStaging(t, m)
}
func TestSetupProductionIdentity(t *testing.T) {
	m := New(Layout{}, Release{}, nil)
	if m.model.size != 491400032 || m.model.digest != "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db" || m.model.url != "https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/9217f5db79a29953eb74d5343926648285ec7e67/qwen2.5-0.5b-instruct-q4_k_m.gguf" {
		t.Fatalf("unpinned production model %+v", m.model)
	}
}
func TestSetupRejectsManagedLinks(t *testing.T) {
	for _, name := range []string{"setup.lock", "models", "staging", "installations"} {
		t.Run(name, func(t *testing.T) {
			m := setupFixture(t, nil, "https://fixture.invalid/model")
			outside := t.TempDir()
			target := outside
			if name == "setup.lock" {
				target = filepath.Join(outside, "lock")
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(m.layout.DataRoot, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Prepare(context.Background(), nil); err == nil {
				t.Fatal("accepted managed link")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) > 0 && name != "setup.lock" {
				t.Fatal("wrote through link")
			}
		})
	}
}
func TestSetupRechecksRelease(t *testing.T) {
	m := setupFixture(t, nil, "https://fixture.invalid/model")
	writeModel(t, m, fixtureModel)
	runtime := filepath.Join(m.layout.ReleaseRoot, "runtime", m.release.Architecture, "llama-server")
	if err := os.WriteFile(runtime, []byte("changed"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare(context.Background(), nil); err == nil {
		t.Fatal("accepted modified release")
	}
	requireAbsent(t, activeReceipt(m))
}
func TestSetupBoundsUnknownLength(t *testing.T) {
	var read atomic.Int64
	body := &countingBody{remaining: 1024 * 1024, read: &read}
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: make(http.Header), Body: body}, nil
	})}, "https://fixture.invalid/model")
	if _, err := m.Prepare(context.Background(), nil); err == nil {
		t.Fatal("accepted oversized stream")
	}
	if read.Load() > int64(len(fixtureModel)+1) {
		t.Fatalf("read too many bytes %d", read.Load())
	}
	if !body.closed.Load() {
		t.Fatal("body not closed")
	}
}

type countingBody struct {
	remaining int
	read      *atomic.Int64
	closed    atomic.Bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	b.remaining -= n
	b.read.Add(int64(n))
	return n, nil
}
func (b *countingBody) Close() error { b.closed.Store(true); return nil }

func TestSetupRejectsPublicManagedDirectories(t *testing.T) {
	for _, name := range []string{"root", "models", "staging", "installations"} {
		t.Run(name, func(t *testing.T) {
			m := setupFixture(t, nil, "https://fixture.invalid/model")
			writeModel(t, m, fixtureModel)
			dir := m.layout.DataRoot
			if name != "root" {
				dir = filepath.Join(dir, name)
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Prepare(context.Background(), nil); err == nil {
				t.Fatal("accepted public managed directory")
			}
			requireAbsent(t, activeReceipt(m))
			requireFile(t, activeModel(m), fixtureModel)
		})
	}
}
func TestSetupPreservesReceiptOnCancel(t *testing.T) {
	m := setupFixture(t, nil, "https://fixture.invalid/model")
	writeModel(t, m, fixtureModel)
	if _, err := m.Prepare(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	receipt, err := os.ReadFile(activeReceipt(m))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Prepare(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	requireFile(t, activeReceipt(m), receipt)
	requireFile(t, activeModel(m), fixtureModel)
}
func TestSetupClosesBodyAndPreservesCleanupError(t *testing.T) {
	cleanup := errors.New("body close failure")
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, ContentLength: -1, Header: make(http.Header), Body: &closeErrorBody{Reader: bytes.NewReader(nil), err: cleanup}}, nil
	})}, "https://fixture.invalid/model")
	if _, err := m.Prepare(context.Background(), nil); !errors.Is(err, cleanup) {
		t.Fatalf("lost cleanup = %v", err)
	}
	requireAbsent(t, activeReceipt(m))
	requireCleanStaging(t, m)
}

type closeErrorBody struct {
	io.Reader
	err error
}

func (b *closeErrorBody) Close() error { return b.err }

// Pacing lets the first byte callback cross the specified progress interval.
// It tests progress cadence; it does not coordinate concurrent state by sleep.
type pacedBody struct {
	io.Reader
	first  bool
	closed atomic.Bool
}

func (b *pacedBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		timer := time.NewTimer(2 * progressInterval)
		<-timer.C
	}
	return b.Reader.Read(p)
}
func (b *pacedBody) Close() error { b.closed.Store(true); return nil }
func TestSetupCoalescesIntermediateProgress(t *testing.T) {
	s := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write(fixtureModel) })
	m := setupFixture(t, s.Client(), s.URL)
	var last time.Time
	_, err := m.Prepare(context.Background(), func(p Progress) error {
		now := time.Now()
		if p.Phase != Complete && !last.IsZero() && now.Sub(last) < progressInterval {
			t.Errorf("intermediate progress less than 100ms apart: %v", p.Phase)
		}
		last = now
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetupTotalDownloadBudget(t *testing.T) {
	m, _, closed := stalledManager(t)
	m.totalTimeout = 30 * time.Millisecond
	if _, err := m.Prepare(context.Background(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("total budget = %v", err)
	}
	await(t, closed)
	requireAbsent(t, activeReceipt(m))
	requireCleanStaging(t, m)
}
func TestSetupRejectsBodyWithoutProgress(t *testing.T) {
	body := &emptyBody{}
	m := setupFixture(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: make(http.Header), Body: body}, nil
	})}, "https://fixture.invalid/model")
	m.idleTimeout = 20 * time.Millisecond
	if _, err := m.Prepare(context.Background(), nil); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("empty body = %v", err)
	}
	if body.reads.Load() > 100 {
		t.Fatalf("busy read loop: %d", body.reads.Load())
	}
	if !body.closed.Load() {
		t.Fatal("body not closed")
	}
}

type emptyBody struct {
	reads  atomic.Int64
	closed atomic.Bool
}

func (b *emptyBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, nil }
func (b *emptyBody) Close() error             { b.closed.Store(true); return nil }

func TestSetupStagingCleanupHonorsCancellation(t *testing.T) {
	dir := canonicalTempDir(t)
	stale := filepath.Join(dir, "setup-crashed")
	if err := os.WriteFile(stale, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cleanStaging(ctx, opened); !errors.Is(err, context.Canceled) {
		t.Fatalf("staging cancel = %v", err)
	}
	requireFile(t, stale, []byte("partial"))
}

func TestSetupPreservesDestinationCreatedDuringDownload(t *testing.T) {
	for _, kind := range []string{"valid", "corrupt", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			s := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if _, err := w.Write(fixtureModel[:3]); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write(fixtureModel[3:])
			})
			m := setupFixture(t, s.Client(), s.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := m.Prepare(ctx, nil); done <- err }()
			await(t, started)
			destination := activeModel(m)
			want := fixtureModel
			if kind == "corrupt" {
				want = []byte("operator restored corrupt model")
			}
			var linkTarget string
			if kind == "symlink" {
				linkTarget = filepath.Join(canonicalTempDir(t), "manual-model")
				if err := os.WriteFile(linkTarget, want, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(linkTarget, destination); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(destination, want, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			unblock()
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrExist) {
					t.Errorf("activation should preserve appearing destination: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("setup did not finish")
			}
			after, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Error("replaced appearing destination inode")
			}
			if kind == "symlink" {
				got, err := os.Readlink(destination)
				if err != nil || got != linkTarget {
					t.Errorf("changed appearing link: %q, %v", got, err)
				}
				requireFile(t, linkTarget, want)
			} else {
				requireFile(t, destination, want)
			}
			requireAbsent(t, activeReceipt(m))
			requireCleanStaging(t, m)
			// Complete the stated operator recovery, then prove immediate lock reuse.
			if kind != "valid" {
				if err := os.Remove(destination); err != nil {
					t.Fatal(err)
				}
				writeModel(t, m, fixtureModel)
			}
			if _, err := m.Prepare(context.Background(), nil); err != nil {
				t.Fatalf("lock reuse: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("reused model caused another download: %d", calls.Load())
			}
		})
	}
}
