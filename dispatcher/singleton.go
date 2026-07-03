package dispatcher

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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
// On success, it returns a release func that clears the recorded PID, unlocks,
// and closes the lockfile. release is idempotent and safe to call more than
// once.
//
// After taking the lock, AcquireSingleton records the holder's PID in the
// lockfile so a read-only observer (e.g. `clipse tui`) can report dispatcher
// liveness by reading and probing that PID, without taking the flock itself —
// which would race a concurrently-starting dispatcher. The flock, not this
// content, remains the singleton guarantee; the PID is advisory metadata.
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

	if err := writePID(f); err != nil {
		// The lock itself is held; back it out so we don't leak a locked fd on
		// a metadata-write failure (disk full / IO error).
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		err = fmt.Errorf("recording pid in lockfile %s: %w", lockPath, err)
		if unlockErr != nil {
			err = fmt.Errorf("%w (also failed to unlock: %v)", err, unlockErr)
		}
		if closeErr != nil {
			err = fmt.Errorf("%w (also failed to close: %v)", err, closeErr)
		}
		return nil, err
	}

	var once sync.Once
	var releaseErr error
	release = func() error {
		once.Do(func() {
			// Clear the advisory PID so a cleanly-stopped dispatcher reads as
			// not-live immediately, rather than depending on the OS not
			// reusing its (now-free) PID. A truncate failure is recorded but
			// must not stop us from releasing the lock.
			if err := f.Truncate(0); err != nil {
				releaseErr = fmt.Errorf("clearing pid in lockfile %s: %w", lockPath, err)
			}
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

// writePID (over)writes the current process id as the entire lockfile content.
// WriteAt at offset 0 after truncating avoids leaving a stale tail from a
// longer previous PID.
func writePID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncating: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	return nil
}
