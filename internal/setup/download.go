package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// One watchdog owns the timeout and joins before download returns. The body
// close unblocks stalled reads. The transport must honor request cancellation.
type downloadWatch struct {
	mu               sync.Mutex
	body             io.ReadCloser
	closeOnce        sync.Once
	closeErr         error
	lastByte         time.Time
	header           bool
	wake, stop, done chan struct{}
}

func (w *downloadWatch) closeBody() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		body := w.body
		w.mu.Unlock()
		if body != nil {
			w.closeErr = body.Close()
		}
	})
	return w.closeErr
}
func (w *downloadWatch) activity(body io.ReadCloser) {
	w.mu.Lock()
	if body != nil {
		w.body = body
		w.header = false
	}
	w.lastByte = time.Now()
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) download(ctx context.Context, file *os.File, progress func(Progress) error) (err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	w := &downloadWatch{lastByte: time.Now(), header: true, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		timer := time.NewTimer(m.headerTimeout)
		defer timer.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			case <-w.wake:
			case <-timer.C:
			}
			w.mu.Lock()
			limit := m.idleTimeout
			if w.header {
				limit = m.headerTimeout
			}
			remaining := limit - time.Since(w.lastByte)
			header := w.header
			w.mu.Unlock()
			if remaining <= 0 {
				kind := "no model bytes received"
				if header {
					kind = "response headers timed out"
				}
				cancel(fmt.Errorf("%s; check the network and run setup again", kind))
				return
			}
			timer.Reset(remaining)
		}
	}()
	// Join the watchdog before reading its close result or releasing the lock.
	defer func() { close(w.stop); <-w.done; err = errors.Join(err, w.closeBody()) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.model.url, nil)
	if err != nil {
		return err
	}
	if req.URL.Scheme != "https" {
		return errors.New("model download must use HTTPS")
	}
	response, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return err
	}
	w.activity(response.Body)
	// A body closer watches the request context as well as the watchdog stop.
	// It is joined here, including callbacks and write failures.
	bodyDone := make(chan struct{})
	go func() {
		defer close(bodyDone)
		select {
		case <-ctx.Done():
			_ = w.closeBody()
		case <-w.stop:
		}
	}()
	defer func() { cancel(nil); <-bodyDone }()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	headerBytes := 0
	for name, values := range response.Header {
		headerBytes += len(name)
		for _, value := range values {
			headerBytes += len(value) + 4
		}
		if headerBytes > maxManifestBytes {
			return errors.New("download headers exceed 64 KiB")
		}
	}
	if response.ContentLength >= 0 && response.ContentLength != m.model.size {
		return errors.New("download size differs from the pinned size")
	}
	h := sha256.New()
	buffer := make([]byte, 32*1024)
	completed := int64(0)
	emptyReads := 0
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		limit := min(int64(len(buffer)), m.model.size+1-completed)
		n, readErr := response.Body.Read(buffer[:int(limit)])
		if n == 0 && readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return io.ErrNoProgress
			}
		}
		if n > 0 {
			emptyReads = 0
			w.activity(nil)
			completed += int64(n)
			if completed > m.model.size {
				return errors.New("download exceeds the pinned size")
			}
			if _, err := file.Write(buffer[:n]); err != nil {
				return err
			}
			_, _ = h.Write(buffer[:n])
			if progress != nil {
				if err := progress(Progress{Downloading, completed, m.model.size}); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return readErr
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if completed != m.model.size {
		return errors.New("download size differs from the pinned size")
	}
	if hex.EncodeToString(h.Sum(nil)) != m.model.digest {
		return errors.New("download SHA-256 differs from the pinned digest")
	}
	return nil
}
