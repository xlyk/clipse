package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// TestDispatcherLive covers the passive PID-based liveness read: a live PID
// (our own) reads as live; a missing/empty/garbled lockfile or a dead PID all
// read as not-live. The probe never takes the flock, so it can't race a
// starting dispatcher.
func TestDispatcherLive(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return p
	}

	// A reaped child's PID is reliably dead.
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting throwaway process: %v", err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Wait() // reap, so deadPID no longer names a live process

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"missing lockfile", filepath.Join(dir, "missing.lock"), false},
		{"live pid (self)", write("self.lock", strconv.Itoa(os.Getpid())), true},
		{"empty", write("empty.lock", "   "), false},
		{"garbage", write("garbage.lock", "not-a-pid"), false},
		{"zero pid", write("zero.lock", "0"), false},
		{"dead pid", write("dead.lock", strconv.Itoa(deadPID)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dispatcherLive(tt.path); got != tt.want {
				t.Errorf("dispatcherLive(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
