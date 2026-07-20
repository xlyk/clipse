package setup_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/setup"
)

func TestWriteConfigCreatesPrivateRoundTrippableFile(t *testing.T) {
	cfg := validConfig(t)
	raw, err := setup.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join(t.TempDir(), "nested", "clipse.yaml")

	result, err := setup.WriteConfig(path, raw, setup.WriteOptions{})
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if result.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty", result.BackupPath)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("Load written config: %v", err)
	}
}

func TestWriteConfigRefusesOrBacksUpExistingFile(t *testing.T) {
	cfg := validConfig(t)
	raw, err := setup.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.yaml")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := setup.WriteConfig(path, raw, setup.WriteOptions{}); err == nil {
		t.Fatal("WriteConfig replaced an existing file without permission")
	}
	result, err := setup.WriteConfig(path, raw, setup.WriteOptions{
		Replace: true,
		Now:     func() time.Time { return time.Date(2026, 7, 19, 12, 34, 56, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("WriteConfig replace: %v", err)
	}
	wantBackup := path + ".bak.20260719T123456Z"
	if result.BackupPath != wantBackup {
		t.Errorf("BackupPath = %q, want %q", result.BackupPath, wantBackup)
	}
	backup, err := os.ReadFile(wantBackup)
	if err != nil {
		t.Fatalf("ReadFile backup: %v", err)
	}
	if string(backup) != "original\n" {
		t.Errorf("backup = %q, want original", backup)
	}
}
