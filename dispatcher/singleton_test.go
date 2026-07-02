package dispatcher

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireSingleton_FreshPathSucceeds(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "clipse.lock")

	release, err := AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("AcquireSingleton() error = %v, want nil", err)
	}
	if release == nil {
		t.Fatal("AcquireSingleton() release = nil, want non-nil func")
	}
	defer release()
}

func TestAcquireSingleton_SecondAcquireFailsWhileHeld(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "clipse.lock")

	release, err := AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("first AcquireSingleton() error = %v, want nil", err)
	}
	defer release()

	_, err = AcquireSingleton(lockPath)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second AcquireSingleton() error = %v, want ErrAlreadyRunning", err)
	}
}

func TestAcquireSingleton_ReacquireAfterRelease(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "clipse.lock")

	release, err := AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("first AcquireSingleton() error = %v, want nil", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v, want nil", err)
	}

	release2, err := AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("AcquireSingleton() after release error = %v, want nil", err)
	}
	defer release2()
}

func TestAcquireSingleton_ReleaseIsIdempotent(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "clipse.lock")

	release, err := AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("AcquireSingleton() error = %v, want nil", err)
	}

	if err := release(); err != nil {
		t.Fatalf("first release() error = %v, want nil", err)
	}
	if err := release(); err != nil {
		t.Fatalf("second release() error = %v, want nil (idempotent)", err)
	}
}

// TestAcquireSingleton_RecordsPIDAndClearsOnRelease asserts the holder writes
// its PID into the lockfile (so a read-only observer can gauge liveness
// without taking the flock) and clears it on release.
func TestAcquireSingleton_RecordsPIDAndClearsOnRelease(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "clipse.lock")

	release, err := AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("AcquireSingleton() error = %v, want nil", err)
	}

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lockfile: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("lockfile PID = %q, want %q", got, want)
	}

	if err := release(); err != nil {
		t.Fatalf("release() error = %v, want nil", err)
	}
	data, err = os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lockfile after release: %v", err)
	}
	if strings.TrimSpace(string(data)) != "" {
		t.Errorf("lockfile after release = %q, want empty (PID cleared)", string(data))
	}
}

// sanity check that the lockfile is actually created on disk, since
// AcquireSingleton is documented to create it if missing.
func TestAcquireSingleton_CreatesLockfile(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "clipse.lock")

	release, err := AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("AcquireSingleton() error = %v, want nil", err)
	}
	defer release()

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lockfile to exist at %s: %v", lockPath, err)
	}
}
