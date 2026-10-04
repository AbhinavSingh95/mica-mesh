package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

const maxReceiptBytes = 64 * 1024

type receiptModel struct {
	ID            string `json:"id"`
	SizeBytes     int64  `json:"size_bytes"`
	SHA256        string `json:"sha256"`
	ContextTokens int    `json:"context_tokens"`
}
type receipt struct {
	Schema         int           `json:"schema"`
	Version        string        `json:"version"`
	Architecture   string        `json:"architecture"`
	Backend        string        `json:"backend"`
	RuntimeVersion string        `json:"runtime_version"`
	RuntimeCommit  string        `json:"runtime_commit"`
	Files          []ReleaseFile `json:"files"`
	Model          receiptModel  `json:"model"`
	VerifiedAt     time.Time     `json:"verified_at"`
}

func modelRecord(model modelDescriptor) receiptModel {
	return receiptModel{model.id, model.size, model.digest, 2048}
}
func receiptName(release Release) string {
	return release.Version + "-" + release.Architecture + ".json"
}

// LoadInstallation resolves prepared paths without network access or writes.
// The caller supplies a release previously verified by LoadRelease. This reader
// checks strict receipt identities and regular-file sizes. Agent startup still
// hashes the model bytes and checks runtime readiness before serving requests.
// Receipt-supplied paths or model addresses are never accepted.
func LoadInstallation(ctx context.Context, layout Layout, release Release) (result Installation, err error) {
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}
	if err := validateRelease(release); err != nil {
		return Installation{}, fmt.Errorf("invalid release metadata: %w", err)
	}
	if !filepath.IsAbs(layout.ReleaseRoot) || filepath.Clean(layout.ReleaseRoot) != layout.ReleaseRoot {
		return Installation{}, errors.New("release root must be an absolute directory path")
	}
	root, err := openDataRoot(layout.DataRoot, false)
	if err != nil {
		return Installation{}, fmt.Errorf("open setup data directory: %w; run setup to prepare managed files", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	file, err := openRegular(int(root.Fd()), "installations/"+receiptName(release))
	if err != nil {
		return Installation{}, fmt.Errorf("open setup receipt: %w; run setup to prepare managed files", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxReceiptBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return Installation{}, fmt.Errorf("read setup receipt: %w", err)
	}
	if len(data) > maxReceiptBytes {
		return Installation{}, errors.New("setup receipt exceeds 64 KiB; run setup again")
	}
	if err := strictReceiptJSON(data); err != nil {
		return Installation{}, fmt.Errorf("invalid setup receipt: %w; run setup again", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record receipt
	if err := decoder.Decode(&record); err != nil {
		return Installation{}, fmt.Errorf("decode setup receipt: %w; run setup again", err)
	}
	model := pinnedModel()
	if record.Schema != 1 || record.Version != release.Version || record.Architecture != release.Architecture || record.Backend != release.Backend || record.RuntimeVersion != release.RuntimeVersion || record.RuntimeCommit != release.RuntimeCommit || !slices.Equal(record.Files, release.Files) || record.Model != modelRecord(model) || record.VerifiedAt.IsZero() {
		return Installation{}, errors.New("setup receipt differs from the installed release or pinned model; run setup again")
	}
	modelFile, err := openRegular(int(root.Fd()), "models/"+model.digest+"/model.gguf")
	if err != nil {
		return Installation{}, fmt.Errorf("open prepared model: %w; run setup again", err)
	}
	if err := errors.Join(checkFileSize(ctx, modelFile, model.size, false), modelFile.Close()); err != nil {
		return Installation{}, fmt.Errorf("check prepared model: %w; run setup again", err)
	}
	releaseFD, err := unix.Open(layout.ReleaseRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Installation{}, fmt.Errorf("open installed release: %w; reinstall the native package", err)
	}
	releaseDir := os.NewFile(uintptr(releaseFD), layout.ReleaseRoot)
	defer func() { err = errors.Join(err, releaseDir.Close()) }()
	for _, entry := range release.Files {
		if entry.Purpose != "runtime" {
			continue
		}
		runtimeFile, err := openRegular(releaseFD, entry.Path)
		if err != nil {
			return Installation{}, fmt.Errorf("open prepared runtime: %w; reinstall the native package", err)
		}
		if err := errors.Join(checkFileSize(ctx, runtimeFile, entry.SizeBytes, true), runtimeFile.Close()); err != nil {
			return Installation{}, fmt.Errorf("check prepared runtime: %w; reinstall the native package", err)
		}
	}
	return installation(layout, release, model), nil
}
func checkFileSize(ctx context.Context, file *os.File, size int64, executable bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return errors.New("file size differs from its verified identity")
	}
	if executable && info.Mode()&0111 == 0 {
		return errors.New("runtime is not executable")
	}
	return nil
}
func saveReceipt(ctx context.Context, dir, staging *os.File, release Release, model modelDescriptor) (err error) {
	// Reject an unsafe destination before changing the active receipt.
	existing, openErr := openRegular(int(dir.Fd()), receiptName(release))
	if openErr == nil {
		if err := existing.Close(); err != nil {
			return err
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return fmt.Errorf("open active receipt: %w; move the unsafe entry aside, then run setup again", openErr)
	}
	record := receipt{Schema: 1, Version: release.Version, Architecture: release.Architecture, Backend: release.Backend, RuntimeVersion: release.RuntimeVersion, RuntimeCommit: release.RuntimeCommit, Files: release.Files, Model: modelRecord(model), VerifiedAt: time.Now().UTC()}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > maxReceiptBytes {
		return errors.New("setup receipt exceeds 64 KiB")
	}
	temp, name, err := privateTemp(staging)
	if err != nil {
		return fmt.Errorf("create setup receipt: %w; check free space and permissions", err)
	}
	defer func() { err = errors.Join(err, removeTemp(staging, name)) }()
	_, writeErr := temp.Write(data)
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	if err := errors.Join(writeErr, temp.Close()); err != nil {
		return fmt.Errorf("save setup receipt: %w; check free space and permissions", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat(int(staging.Fd()), name, int(dir.Fd()), receiptName(release)); err != nil {
		return fmt.Errorf("activate setup receipt: %w; check directory permissions", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync receipt directory: %w", err)
	}
	return nil
}

func strictReceiptJSON(data []byte) error {
	fields := map[string]bool{"schema": true, "version": true, "architecture": true, "backend": true, "runtime_version": true, "runtime_commit": true, "files": true, "path": true, "size_bytes": true, "sha256": true, "purpose": true, "model": true, "id": true, "context_tokens": true, "verified_at": true}
	return strictJSONFields(data, fields)
}
