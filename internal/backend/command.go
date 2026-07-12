package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

var lifecycleEnvKeys = map[string]struct{}{
	"PATH":            {},
	"HOME":            {},
	"DAYTONA_API_KEY": {},
	"DAYTONA_API_URL": {},
	"DAYTONA_TARGET":  {},
}

// RunFunc executes a complete argv with an explicit environment and returns
// stdout. CommandManager accepts it as a seam for deterministic tests.
type RunFunc func(context.Context, []string, []string, string) ([]byte, error)

// CommandManager invokes clipse-worker's lifecycle command protocol.
type CommandManager struct {
	command    []string
	run        RunFunc
	env        []string
	secrets    []string
	projectDir string
}

// NewCommandManager builds a lifecycle manager from the same configured
// worker-command prefix used for agent turns. A nil runner uses os/exec.
func NewCommandManager(command []string, runner RunFunc, environ []string, projectDir string) *CommandManager {
	if runner == nil {
		runner = runCommand
	}
	env := lifecycleEnv(environ)
	var secrets []string
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "DAYTONA_API_KEY" && value != "" {
			secrets = append(secrets, value)
		}
	}
	return &CommandManager{
		command:    append([]string(nil), command...),
		run:        runner,
		env:        env,
		secrets:    secrets,
		projectDir: projectDir,
	}
}

func (m *CommandManager) Ensure(ctx context.Context, request EnsureRequest) (Workspace, error) {
	args := []string{"--backend-action=ensure"}
	args = appendStringFlag(args, "--backend-provider", request.Provider)
	args = appendStringFlag(args, "--backend-role", request.Role)
	args = appendStringFlag(args, "--issue", request.IssueID)
	args = appendStringFlag(args, "--run", request.RunID)
	args = appendStringFlag(args, "--repo-url", request.RepoURL)
	args = appendStringFlag(args, "--repo-slug", request.RepoSlug)
	args = appendStringFlag(args, "--base-branch", request.BaseBranch)
	args = appendStringFlag(args, "--branch", request.Branch)
	args = appendIntFlag(args, "--auto-stop-minutes", request.AutoStopMinutes)
	args = appendIntFlag(args, "--reviewer-auto-delete-minutes", request.ReviewerAutoDeleteMinutes)
	args = appendStringFlag(args, "--snapshot", request.Snapshot)
	args = appendStringFlag(args, "--target", request.Target)

	result, err := m.action(ctx, "ensure", args)
	if err != nil {
		return Workspace{}, err
	}
	workspace := result.workspace()
	workspace.Role = request.Role
	workspace.IssueID = request.IssueID
	workspace.RunID = request.RunID
	workspace.RepoSlug = request.RepoSlug
	workspace.Target = request.Target
	return workspace, nil
}

func (m *CommandManager) Delete(ctx context.Context, workspace Workspace) error {
	args := []string{"--backend-action=delete"}
	args = appendStringFlag(args, "--backend-provider", workspace.Provider)
	args = appendStringFlag(args, "--backend-role", workspace.Role)
	args = appendStringFlag(args, "--issue", workspace.IssueID)
	args = appendStringFlag(args, "--run", workspace.RunID)
	args = appendStringFlag(args, "--repo-slug", workspace.RepoSlug)
	args = appendStringFlag(args, "--sandbox-id", workspace.ExternalID)
	args = appendStringFlag(args, "--target", workspace.Target)
	_, err := m.action(ctx, "delete", args)
	return err
}

func (m *CommandManager) List(ctx context.Context, request ListRequest) ([]Workspace, error) {
	args := []string{"--backend-action=list"}
	args = appendStringFlag(args, "--backend-provider", request.Provider)
	args = appendStringFlag(args, "--repo-slug", request.RepoSlug)
	args = appendStringFlag(args, "--target", request.Target)
	result, err := m.action(ctx, "list", args)
	if err != nil {
		return nil, err
	}
	workspaces := make([]Workspace, len(result.Workspaces))
	for i, item := range result.Workspaces {
		workspaces[i] = item.workspace()
		workspaces[i].Provider = result.Provider
		workspaces[i].RepoSlug = request.RepoSlug
		workspaces[i].Target = request.Target
	}
	return workspaces, nil
}

type commandResult struct {
	Action         string             `json:"action"`
	Provider       string             `json:"provider"`
	OK             bool               `json:"ok"`
	OwnerKey       string             `json:"owner_key"`
	ExternalID     string             `json:"external_id"`
	WorkspacePath  string             `json:"workspace_path"`
	State          WorkspaceState     `json:"state"`
	Workspaces     []commandWorkspace `json:"workspaces"`
	ErrorKind      ErrorKind          `json:"error_kind"`
	ErrorOperation string             `json:"error_operation"`
	Error          string             `json:"error"`
}

type commandWorkspace struct {
	OwnerKey      string         `json:"owner_key"`
	ExternalID    string         `json:"external_id"`
	WorkspacePath string         `json:"workspace_path"`
	State         WorkspaceState `json:"state"`
}

func (w commandWorkspace) workspace() Workspace {
	return Workspace{OwnerKey: w.OwnerKey, ExternalID: w.ExternalID, WorkspacePath: w.WorkspacePath, State: w.State}
}

func (r commandResult) workspace() Workspace {
	return Workspace{
		Provider: r.Provider, OwnerKey: r.OwnerKey, ExternalID: r.ExternalID,
		WorkspacePath: r.WorkspacePath, State: r.State,
	}
}

func (m *CommandManager) action(ctx context.Context, action string, flags []string) (commandResult, error) {
	if len(m.command) == 0 {
		return commandResult{}, &ActionError{Kind: ErrorKindCapability, Op: action, Msg: "worker command is not configured"}
	}
	argv := append(append([]string(nil), m.command...), flags...)
	stdout, err := m.run(ctx, argv, append([]string(nil), m.env...), m.projectDir)
	if err != nil {
		return commandResult{}, &ActionError{Kind: ErrorKindTransient, Op: action, Msg: "lifecycle command failed"}
	}

	var result commandResult
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return commandResult{}, malformedActionError(action)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return commandResult{}, malformedActionError(action)
	}
	if result.Action != action || result.Provider != "daytona" {
		return commandResult{}, malformedActionError(action)
	}
	if !result.OK {
		if !validErrorKind(result.ErrorKind) || result.ErrorOperation == "" || result.Error == "" {
			return commandResult{}, malformedActionError(action)
		}
		return commandResult{}, &ActionError{
			Kind: result.ErrorKind,
			Op:   result.ErrorOperation,
			Msg:  m.redact(result.Error),
		}
	}
	if action == "list" {
		if result.Workspaces == nil {
			return commandResult{}, malformedActionError(action)
		}
		for _, workspace := range result.Workspaces {
			if !validWorkspaceForAction(action, workspace.OwnerKey, workspace.ExternalID, workspace.WorkspacePath, workspace.State) {
				return commandResult{}, malformedActionError(action)
			}
		}
		return result, nil
	}
	if !validWorkspaceForAction(action, result.OwnerKey, result.ExternalID, result.WorkspacePath, result.State) {
		return commandResult{}, malformedActionError(action)
	}
	return result, nil
}

func malformedActionError(action string) error {
	return &ActionError{Kind: ErrorKindCapability, Op: action, Msg: "lifecycle command returned malformed JSON"}
}

func validErrorKind(kind ErrorKind) bool {
	return kind == ErrorKindTransient || kind == ErrorKindCapability || kind == ErrorKindNeedsInput
}

func validWorkspaceForAction(action, ownerKey, externalID, workspacePath string, state WorkspaceState) bool {
	if ownerKey == "" || externalID == "" || workspacePath == "" {
		return false
	}
	switch action {
	case "ensure":
		return state == WorkspaceActive || state == WorkspaceStopped
	case "delete":
		return state == WorkspaceDeleted
	case "list":
		return state == WorkspaceActive || state == WorkspaceStopped || state == WorkspaceCleanupPending || state == WorkspaceError
	default:
		return false
	}
}

func (m *CommandManager) redact(message string) string {
	for _, secret := range m.secrets {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}

func appendStringFlag(args []string, flag, value string) []string {
	if value == "" {
		return args
	}
	return append(args, flag+"="+value)
}

func appendIntFlag(args []string, flag string, value int) []string {
	if value <= 0 {
		return args
	}
	return append(args, flag+"="+strconv.Itoa(value))
}

func lifecycleEnv(environ []string) []string {
	seen := make(map[string]struct{}, len(lifecycleEnvKeys))
	var env []string
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, allowed := lifecycleEnvKeys[key]; !allowed {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		env = append(env, entry)
	}
	return env
}

func runCommand(ctx context.Context, argv, env []string, cwd string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("backend command is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = cwd
	return cmd.Output()
}

var _ Manager = (*CommandManager)(nil)
