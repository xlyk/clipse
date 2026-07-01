package dispatcher

import (
	"errors"
	"os"
	"path/filepath"
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
