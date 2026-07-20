package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xlyk/clipse/internal/backend"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/linear"
)

type Severity string

const (
	SeverityPass    Severity = "pass"
	SeverityWarning Severity = "warning"
	SeverityBlocked Severity = "blocked"
)

type Outcome string

const (
	OutcomeReady   Outcome = "ready"
	OutcomeWarning Outcome = "configured_with_warnings"
	OutcomeBlocked Outcome = "blocked"
)

// CheckResult is one sanitized, stable readiness result displayed by the
// wizard. Detail must never contain a credential value.
type CheckResult struct {
	ID       string
	Severity Severity
	Summary  string
	Detail   string
}

type Report struct {
	Outcome Outcome
	Results []CheckResult
}

type Command struct {
	Name string
	Args []string
	Dir  string
	Env  []string
}

type CommandRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, Command) ([]byte, error)
}

type LinearReadiness interface {
	Check(context.Context, config.Config) (candidateCount int, err error)
}

type BackendReadiness interface {
	Check(context.Context, config.Config, []string) error
}

type CheckOptions struct {
	Runner  CommandRunner
	Linear  LinearReadiness
	Backend BackendReadiness
	Environ []string
}

// RunChecks executes the wizard's non-mutating readiness suite. Individual
// failures are accumulated so the operator sees every blocker in one pass.
func RunChecks(ctx context.Context, cfg config.Config, opts CheckOptions) Report {
	runner := opts.Runner
	if runner == nil {
		runner = OSCommandRunner{}
	}
	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}
	linearProbe := opts.Linear
	if linearProbe == nil {
		linearProbe = realLinearReadiness{}
	}
	backendProbe := opts.Backend
	if backendProbe == nil {
		backendProbe = realBackendReadiness{}
	}

	results := make([]CheckResult, 0, 10)
	if _, err := Render(cfg); err != nil {
		results = append(results, blocked("config", "Configuration is invalid", err))
	} else {
		results = append(results, passed("config", "Configuration parses with production rules"))
	}

	results = append(results, checkTools(runner, cfg))
	results = append(results, checkRepository(ctx, runner, cfg, environ))
	results = append(results, checkGitHub(ctx, runner, cfg, environ))
	results = append(results, checkWorker(ctx, runner, cfg, environ))

	if envValue(environ, "LINEAR_API_KEY") == "" {
		results = append(results, CheckResult{ID: "linear", Severity: SeverityBlocked, Summary: "LINEAR_API_KEY is not available to this process", Detail: "Provide a repeatable launch-time credential source, then recheck."})
	} else {
		count, err := linearProbe.Check(ctx, cfg)
		if err != nil {
			results = append(results, blocked("linear", "Linear team/state preflight failed", err))
		} else if count == 0 {
			results = append(results, CheckResult{ID: "linear", Severity: SeverityWarning, Summary: "Linear is ready, but no issues are opted in", Detail: "Add an agent:<lane> label after setup when work is ready."})
		} else {
			results = append(results, CheckResult{ID: "linear", Severity: SeverityPass, Summary: fmt.Sprintf("Linear is ready with %d opted-in issue(s)", count)})
		}
	}

	if cfg.AgentBackend.Type == "daytona" {
		if envValue(environ, "DAYTONA_API_KEY") == "" {
			results = append(results, CheckResult{ID: "daytona", Severity: SeverityBlocked, Summary: "DAYTONA_API_KEY is not available to this process", Detail: "Export or inject it before launching the wizard and dispatcher."})
		} else if err := backendProbe.Check(ctx, cfg, environ); err != nil {
			results = append(results, blocked("daytona", "Daytona read-only preflight failed", err))
		} else {
			results = append(results, passed("daytona", "Daytona target and GitHub host auth are ready"))
			if cfg.AgentBackend.Daytona.Snapshot != "" {
				results = append(results, CheckResult{ID: "daytona_snapshot", Severity: SeverityWarning, Summary: "Daytona snapshot is configured but not live-tested", Detail: "Run the explicit smoke after writing to verify its toolchain."})
			}
		}
	} else {
		results = append(results, CheckResult{ID: "daytona", Severity: SeverityWarning, Summary: "Local compatibility backend selected", Detail: "Daytona is recommended for isolated agent tools."})
	}

	results = append(results, checkModels(cfg, environ))
	results = append(results, checkRuntimePaths(cfg))
	return Report{Outcome: outcome(results), Results: results}
}

func checkTools(runner CommandRunner, cfg config.Config) CheckResult {
	tools := []string{"git", "gh"}
	if len(cfg.Worker.Command) > 0 {
		tools = append(tools, cfg.Worker.Command[0])
	}
	sort.Strings(tools)
	tools = compactStrings(tools)
	var missing []string
	for _, tool := range tools {
		if _, err := runner.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return CheckResult{ID: "tools", Severity: SeverityBlocked, Summary: "Required host tools are missing", Detail: strings.Join(missing, ", ")}
	}
	return passed("tools", "Required host tools are available")
}

func checkRepository(ctx context.Context, runner CommandRunner, cfg config.Config, env []string) CheckResult {
	commands := []Command{
		{Name: "git", Args: []string{"-C", cfg.Repo.Path, "rev-parse", "--is-inside-work-tree"}, Env: env},
		{Name: "git", Args: []string{"-C", cfg.Repo.Path, "remote", "get-url", "origin"}, Env: env},
		{Name: "git", Args: []string{"ls-remote", cfg.Repo.Remote, "refs/heads/" + cfg.Repo.BaseBranch}, Env: env},
	}
	inside, err := runShort(ctx, runner, commands[0])
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return blocked("repository", "Repository path is not a usable Git worktree", err)
	}
	remote, err := runShort(ctx, runner, commands[1])
	if err != nil {
		return blocked("repository", "Could not read repository origin", err)
	}
	if !sameGitHubRemote(strings.TrimSpace(string(remote)), cfg.Repo.Remote) {
		return CheckResult{ID: "repository", Severity: SeverityBlocked, Summary: "Configured remote does not match origin", Detail: "Update repo.remote or choose the matching primary clone."}
	}
	if _, err := runShort(ctx, runner, commands[2]); err != nil {
		return blocked("repository", "Base branch is not readable from the configured remote", err)
	}
	return passed("repository", "Repository, origin, and base branch are readable")
}

func sameGitHubRemote(left, right string) bool {
	_, leftSlug, leftErr := backend.CanonicalGitHubRemote(left)
	_, rightSlug, rightErr := backend.CanonicalGitHubRemote(right)
	if leftErr == nil && rightErr == nil {
		return leftSlug == rightSlug
	}
	return left == right
}

func checkGitHub(ctx context.Context, runner CommandRunner, cfg config.Config, env []string) CheckResult {
	_, slug, err := backend.CanonicalGitHubRemote(cfg.Repo.Remote)
	if err != nil {
		return blocked("github", "Repository remote is not a credential-free GitHub URL", err)
	}
	if _, err := runShort(ctx, runner, Command{Name: "gh", Args: []string{"auth", "status", "--hostname", "github.com"}, Env: env}); err != nil {
		return blocked("github", "GitHub CLI is not authenticated", err)
	}
	if _, err := runShort(ctx, runner, Command{Name: "gh", Args: []string{"repo", "view", slug, "--json", "nameWithOwner"}, Env: env}); err != nil {
		return blocked("github", "GitHub repository is not accessible", err)
	}
	return passed("github", "GitHub identity can access "+slug)
}

func checkWorker(ctx context.Context, runner CommandRunner, cfg config.Config, env []string) CheckResult {
	if len(cfg.Worker.Command) == 0 {
		return CheckResult{ID: "worker", Severity: SeverityBlocked, Summary: "Worker command is empty"}
	}
	command := Command{Name: cfg.Worker.Command[0], Args: append(append([]string(nil), cfg.Worker.Command[1:]...), "--help"), Dir: cfg.Repo.Path, Env: env}
	if _, err := runShort(ctx, runner, command); err != nil {
		return blocked("worker", "Clipse worker command failed its help probe", err)
	}
	return passed("worker", "Clipse worker command is runnable")
}

func checkModels(cfg config.Config, env []string) CheckResult {
	providers := make(map[string]struct{})
	for _, spec := range []string{cfg.Models.Coder, cfg.Models.CoderDocs, cfg.Models.Reviewer} {
		provider, _, ok := strings.Cut(spec, ":")
		if ok {
			providers[provider] = struct{}{}
		}
	}
	var missing []string
	var unknown []string
	for provider := range providers {
		switch provider {
		case "anthropic":
			if envValue(env, "ANTHROPIC_API_KEY") == "" {
				missing = append(missing, "ANTHROPIC_API_KEY")
			}
			if !containsString(cfg.EnvAllowlist, "ANTHROPIC_API_KEY") {
				missing = append(missing, "ANTHROPIC_API_KEY in env_allowlist")
			}
		case "openai_codex":
			home := envValue(env, "HOME")
			if home == "" {
				missing = append(missing, "HOME for openai_codex OAuth")
				continue
			}
			if _, err := os.Stat(filepath.Join(home, ".deepagents", ".state", "chatgpt-auth.json")); err != nil {
				missing = append(missing, "openai_codex OAuth")
			}
			if !containsString(cfg.EnvAllowlist, "HOME") {
				missing = append(missing, "HOME in env_allowlist")
			}
		default:
			unknown = append(unknown, provider)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return CheckResult{ID: "models", Severity: SeverityBlocked, Summary: "Selected model authentication is incomplete", Detail: strings.Join(missing, ", ")}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return CheckResult{ID: "models", Severity: SeverityWarning, Summary: "Custom model provider auth was not tested", Detail: strings.Join(unknown, ", ")}
	}
	return passed("models", "Selected model authentication is present")
}

func checkRuntimePaths(cfg config.Config) CheckResult {
	if !filepath.IsAbs(cfg.BoardDir) || !filepath.IsAbs(cfg.CheckpointsDir) {
		return CheckResult{ID: "runtime", Severity: SeverityWarning, Summary: "Runtime paths are relative", Detail: "Absolute, unique paths are recommended for multiple instances."}
	}
	if cfg.BoardDir == cfg.CheckpointsDir {
		return CheckResult{ID: "runtime", Severity: SeverityBlocked, Summary: "Board and checkpoint directories must be distinct"}
	}
	lockPath := filepath.Join(cfg.BoardDir, "clipse.lock")
	if raw, err := os.ReadFile(lockPath); err == nil {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if parseErr == nil && pid > 0 {
			if signalErr := syscall.Kill(pid, 0); signalErr == nil || errors.Is(signalErr, syscall.EPERM) {
				return CheckResult{ID: "runtime", Severity: SeverityBlocked, Summary: "Board directory is already owned by a live dispatcher", Detail: lockPath}
			}
		}
	}
	return passed("runtime", "Runtime paths are absolute and isolated")
}

type realLinearReadiness struct{}

func (realLinearReadiness) Check(ctx context.Context, cfg config.Config) (int, error) {
	opts := make([]linear.HTTPClientOption, 0, 1)
	if cfg.StateLabelPrefix != "" {
		opts = append(opts, linear.WithStateLabelPrefix(cfg.StateLabelPrefix))
	}
	client, err := linear.NewHTTPClient(cfg.TeamKey, cfg.TeamID, cfg.LaneLabelPrefix, opts...)
	if err != nil {
		return 0, err
	}
	if cfg.StateLabelPrefix == "" {
		if err := client.ValidateWorkflowStates(ctx); err != nil {
			return 0, err
		}
	} else if err := client.ValidateStateLabels(ctx); err != nil {
		return 0, err
	}
	issues, err := client.CandidateIssues(ctx)
	if err != nil {
		return 0, err
	}
	return len(issues), nil
}

type realBackendReadiness struct{}

func (realBackendReadiness) Check(ctx context.Context, cfg config.Config, env []string) error {
	_, slug, err := backend.CanonicalGitHubRemote(cfg.Repo.Remote)
	if err != nil {
		return err
	}
	manager := backend.NewCommandManager(cfg.Worker.Command, nil, env, cfg.Repo.Path)
	_, err = manager.List(ctx, backend.ListRequest{Provider: "daytona", RepoSlug: slug, Target: cfg.AgentBackend.Daytona.Target})
	return err
}

// OSCommandRunner is the production subprocess boundary. It never uses a
// shell and redacts credential-looking environment values from bounded error
// output before returning it to the UI.
type OSCommandRunner struct{}

func (OSCommandRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (OSCommandRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if len(detail) > 800 {
			detail = detail[:800] + "…"
		}
		detail = redact(detail, command.Env)
		if detail == "" {
			return nil, fmt.Errorf("%s failed: %w", command.Name, err)
		}
		return nil, fmt.Errorf("%s failed: %s", command.Name, detail)
	}
	return out, nil
}

func runShort(ctx context.Context, runner CommandRunner, command Command) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return runner.Run(probeCtx, command)
}

func passed(id, summary string) CheckResult {
	return CheckResult{ID: id, Severity: SeverityPass, Summary: summary}
}

func blocked(id, summary string, err error) CheckResult {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return CheckResult{ID: id, Severity: SeverityBlocked, Summary: summary, Detail: detail}
}

func outcome(results []CheckResult) Outcome {
	result := OutcomeReady
	for _, check := range results {
		switch check.Severity {
		case SeverityBlocked:
			return OutcomeBlocked
		case SeverityWarning:
			result = OutcomeWarning
		}
	}
	return result
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func redact(text string, env []string) string {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || !(strings.Contains(key, "KEY") || strings.Contains(key, "TOKEN") || strings.Contains(key, "PASSWORD")) {
			continue
		}
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	return text
}
