package dispatcher

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

// ErrAlreadyRunning is returned by AcquireSingleton when another clipse
// dispatcher already holds the lock on the machine.
var ErrAlreadyRunning = errors.New("another clipse dispatcher is already running")

// AcquireSingleton takes a machine-global exclusive lock on lockPath so that
// only one clipse dispatcher runs at a time. The lockfile is created if it
// does not already exist.
//
// On success, it returns a release func that unlocks and closes the
// lockfile. release is idempotent and safe to call more than once.
//
// If another process already holds the lock, AcquireSingleton returns
// ErrAlreadyRunning.
func AcquireSingleton(lockPath string) (release func() error, err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lockfile %s: %w", lockPath, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		if closeErr != nil {
			return nil, fmt.Errorf("flocking lockfile %s: %w (also failed to close: %v)", lockPath, err, closeErr)
		}
		return nil, fmt.Errorf("flocking lockfile %s: %w", lockPath, err)
	}

	var once sync.Once
	var releaseErr error
	release = func() error {
		once.Do(func() {
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
				releaseErr = fmt.Errorf("unlocking lockfile %s: %w", lockPath, err)
				return
			}
			if err := f.Close(); err != nil {
				releaseErr = fmt.Errorf("closing lockfile %s: %w", lockPath, err)
			}
		})
		return releaseErr
	}

	return release, nil
}
