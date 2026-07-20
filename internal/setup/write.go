package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/xlyk/clipse/internal/config"
)

// WriteOptions controls the only destructive edge of the wizard. Replace
// must be explicit when the destination already exists.
type WriteOptions struct {
	Replace bool
	Now     func() time.Time
}

type WriteResult struct {
	Path       string
	BackupPath string
}

// WriteConfig validates raw, optionally backs up an existing destination,
// and atomically installs a private config file.
func WriteConfig(path string, raw []byte, opts WriteOptions) (WriteResult, error) {
	if _, err := config.Parse(raw, path); err != nil {
		return WriteResult{}, fmt.Errorf("validating config before write: %w", err)
	}
	if path == "" {
		return WriteResult{}, errors.New("config output path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("resolving config output path: %w", err)
	}
	path = abs
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return WriteResult{}, fmt.Errorf("creating config directory %s: %w", parent, err)
	}

	result := WriteResult{Path: path}
	if _, err := os.Stat(path); err == nil {
		if !opts.Replace {
			return WriteResult{}, fmt.Errorf("config %s already exists; backup-and-replace confirmation is required", path)
		}
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		result.BackupPath = path + ".bak." + now().UTC().Format("20060102T150405Z")
		if err := copyExclusive(path, result.BackupPath); err != nil {
			return WriteResult{}, fmt.Errorf("backing up existing config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return WriteResult{}, fmt.Errorf("checking config destination %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(parent, ".clipse-config-*.tmp")
	if err != nil {
		return WriteResult{}, fmt.Errorf("creating temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return WriteResult{}, fmt.Errorf("setting temporary config permissions: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return WriteResult{}, fmt.Errorf("writing temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return WriteResult{}, fmt.Errorf("syncing temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return WriteResult{}, fmt.Errorf("closing temporary config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return WriteResult{}, fmt.Errorf("installing config %s: %w", path, err)
	}
	if err := syncDir(parent); err != nil {
		return WriteResult{}, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("round-tripping written config: %w", err)
	}
	for _, dir := range []string{cfg.BoardDir, cfg.CheckpointsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return WriteResult{}, fmt.Errorf("creating runtime directory %s: %w", dir, err)
		}
	}
	return result, nil
}

func copyExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening config directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("syncing config directory: %w", err)
	}
	return nil
}
