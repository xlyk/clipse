package spawn

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psLstartLayout is the format `ps -o lstart=` prints on both macOS/BSD and
// Linux/procps: e.g. "Wed Jul  1 14:13:48 2026". Go's time.Parse tolerates
// the single- vs double-space day-of-month difference between platforms
// (day 1-9 vs 10-31) with this same reference layout, so one layout string
// covers both.
const psLstartLayout = "Mon Jan 2 15:04:05 2006"

// processStartTime returns pid's process start time as a unix timestamp
// (second granularity), via `ps -p <pid> -o lstart=`. This is best-effort:
// it works on macOS and Linux without any new dependency (no golang.org/
// x/sys, no /proc parsing), but ps availability, exact column output, and
// timing (a pid that has already exited by the time ps runs) can all cause
// it to fail. Callers should treat a returned error (or a 0 result) as
// "unverifiable" rather than fatal — see RunHandle.ProcStartedAt.
func processStartTime(pid int) (int64, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return 0, fmt.Errorf("ps -p %d -o lstart=: %w", pid, err)
	}
	line := strings.TrimSpace(string(bytes.TrimSpace(out)))
	if line == "" {
		return 0, fmt.Errorf("ps -p %d -o lstart=: empty output", pid)
	}
	t, err := time.ParseInLocation(psLstartLayout, line, time.Local)
	if err != nil {
		return 0, fmt.Errorf("parsing ps lstart %q: %w", line, err)
	}
	return t.Unix(), nil
}
