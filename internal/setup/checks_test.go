package setup_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/setup"
)

type fakeRunner struct {
	remote string
}

func (f fakeRunner) LookPath(file string) (string, error) {
	if file == "missing" {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + file, nil
}

func (f fakeRunner) Run(_ context.Context, command setup.Command) ([]byte, error) {
	joined := strings.Join(append([]string{command.Name}, command.Args...), " ")
	switch {
	case strings.Contains(joined, "rev-parse --is-inside-work-tree"):
		return []byte("true\n"), nil
	case strings.Contains(joined, "remote get-url origin"):
		return []byte(f.remote + "\n"), nil
	case strings.Contains(joined, "ls-remote"):
		return []byte("abc123\tHEAD\n"), nil
	case strings.Contains(joined, "gh auth status"):
		return []byte("ok"), nil
	case strings.Contains(joined, "gh repo view"):
		return []byte(`{"nameWithOwner":"acme/widget"}`), nil
	case strings.Contains(joined, "clipse-worker --help"):
		return []byte("usage"), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
}

type fakeLinearProbe struct {
	count int
	err   error
}

func (f fakeLinearProbe) Check(context.Context, config.Config) (int, error) {
	return f.count, f.err
}

type fakeBackendProbe struct{ err error }

func (f fakeBackendProbe) Check(context.Context, config.Config, []string) error { return f.err }

func TestRunChecksReadyAndZeroCandidatesWarning(t *testing.T) {
	cfg := validConfig(t)
	env := []string{
		"PATH=/usr/bin",
		"HOME=" + t.TempDir(),
		"LINEAR_API_KEY=linear-secret",
		"DAYTONA_API_KEY=daytona-secret",
		"ANTHROPIC_API_KEY=model-secret",
	}
	opts := setup.CheckOptions{
		Runner:  fakeRunner{remote: cfg.Repo.Remote},
		Linear:  fakeLinearProbe{count: 2},
		Backend: fakeBackendProbe{},
		Environ: env,
	}
	report := setup.RunChecks(context.Background(), cfg, opts)
	if report.Outcome != setup.OutcomeReady {
		t.Fatalf("Outcome = %q, want ready; checks=%#v", report.Outcome, report.Results)
	}

	opts.Linear = fakeLinearProbe{count: 0}
	report = setup.RunChecks(context.Background(), cfg, opts)
	if report.Outcome != setup.OutcomeWarning {
		t.Fatalf("zero-candidate Outcome = %q, want warning; checks=%#v", report.Outcome, report.Results)
	}
}

func TestRunChecksMissingCredentialsBlocksWithoutLeakingValues(t *testing.T) {
	cfg := validConfig(t)
	report := setup.RunChecks(context.Background(), cfg, setup.CheckOptions{
		Runner:  fakeRunner{remote: cfg.Repo.Remote},
		Linear:  fakeLinearProbe{},
		Backend: fakeBackendProbe{},
		Environ: []string{"PATH=/usr/bin", "HOME=" + t.TempDir()},
	})
	if report.Outcome != setup.OutcomeBlocked {
		t.Fatalf("Outcome = %q, want blocked", report.Outcome)
	}
	joined := fmt.Sprintf("%#v", report.Results)
	for _, secret := range []string{"linear-secret", "daytona-secret", "model-secret"} {
		if strings.Contains(joined, secret) {
			t.Errorf("readiness report leaked %q", secret)
		}
	}
}
