// Package tui implements the bubbletea terminal dashboard for clipse: a
// live view over the kernel's SQLite snapshot that polls on a timer and
// redraws in place. Update is kept pure (no store/DB access); the actual
// snapshot fetch is injected via WithRefreshCmd and run as a tea.Cmd, so the
// Model is unit-testable with hand-built snapshotMsg values and needs no
// TTY.
package tui
