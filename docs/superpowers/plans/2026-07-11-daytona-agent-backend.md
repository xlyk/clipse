# Daytona Agent Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a production Daytona agent backend with persistent coder sandboxes, disposable reviewer sandboxes, controller-only GitHub credentials, durable cleanup, and an unchanged local execution option.

**Architecture:** The deterministic Go dispatcher provisions and records workspaces through a typed lifecycle command, then starts the existing host Python worker with provider metadata. Python keeps LangGraph and model calls on the host, injects `DaytonaSandbox` into DAC, and routes controller Git/GitHub operations through a session interface. The Go Git operator remains on the host and fetches branches created in Daytona only when a card reaches `merging`.

**Tech Stack:** Go 1.24, SQLite, Python 3.13, LangGraph, `deepagents-code[daytona]==0.1.22`, Daytona Python SDK, `langchain-daytona`, GitHub CLI, pytest, standard-library Go tests

## Global Constraints

- An absent `agent_backend` block means `local`; existing configurations and worker argv stay compatible.
- New example configuration explicitly recommends `daytona`.
- Coder and coder-docs share one repository-and-issue sandbox; every reviewer run receives a run-scoped disposable sandbox.
- `auto_stop_minutes` defaults to `60`; reviewer automatic deletion defaults to `60` minutes.
- The Daytona API key reaches only Daytona-mode host controller processes; local workers never inherit it.
- Host `gh auth token` is passed only to Daytona SDK Git method arguments and never enters sandbox env, Git config, commands, prompts, transcripts, events, or errors.
- A Daytona failure never falls back to local.
- The Go kernel imports no Daytona SDK and remains LLM-free.
- All production behavior follows test-first red-green-refactor cycles.
- Preserve unrelated untracked files; stage exact paths only.

---

## File structure

### New Go files

- `internal/backend/backend.go` — provider-neutral lifecycle types, manager interface, and typed errors.
- `internal/backend/command.go` — command-backed Daytona manager and sanitized JSON protocol.
- `internal/backend/command_test.go` — argv, environment, parsing, and secret-boundary tests.
- `internal/store/workspaces.go` — `agent_workspaces` persistence operations.
- `internal/store/workspaces_test.go` — lifecycle persistence and cleanup queue tests.
- `dispatcher/backend.go` — provisioning, cleanup drain, and startup reconciliation orchestration.
- `dispatcher/backend_test.go` — dispatcher lifecycle behavior with a fake manager.

### New Python files

- `agent/src/clipse_agent/backends/__init__.py` — public backend package exports.
- `agent/src/clipse_agent/backends/contracts.py` — Pydantic lifecycle request/result models and error kinds.
- `agent/src/clipse_agent/backends/github.py` — sanitized host `gh` authentication and repository operations.
- `agent/src/clipse_agent/backends/daytona.py` — labels, lifecycle actions, and `DaytonaSession`.
- `agent/src/clipse_agent/backends/local.py` — current subprocess behavior behind `LocalSession`.
- `agent/src/clipse_agent/backends/session.py` — the graph-facing `AgentSession` protocol.
- `agent/tests/test_backend_contracts.py` — lifecycle result serialization and sanitization.
- `agent/tests/test_daytona_backend.py` — fake-SDK lifecycle and session behavior.
- `agent/tests/test_local_backend.py` — local compatibility behavior.

### New smoke file

- `scripts/smoke_daytona_backend.py` — production-path live DAC, branch, draft-PR, reviewer, and cleanup smoke.

### Existing files changed

- `agent/pyproject.toml`, `agent/uv.lock` — enable the pinned Daytona extra.
- `agent/src/clipse_agent/worker.py` — lifecycle action mode, backend flags, and session construction.
- `agent/src/clipse_agent/dac.py` — optional DAC sandbox injection.
- `agent/src/clipse_agent/graphs/coder.py` — session-owned workspace, Git, and GitHub operations.
- `agent/src/clipse_agent/graphs/reviewer.py` — host PR diff/comments and remote file reads through a session.
- `agent/tests/test_worker.py`, `test_dac.py`, `test_coder_graph.py`, `test_reviewer_graph.py` — provider wiring and regression coverage.
- `internal/config/config.go`, `config_test.go` — backend configuration and defaults.
- `internal/spawn/spawn.go`, `local.go`, `argv_test.go`, `env_test.go` — worker backend metadata and environment policy.
- `internal/spawn/worktree.go`, `worktree_test.go` — fetch a Daytona-created remote branch.
- `internal/store/migrations.go`, `migrations_test.go`, `crud.go`, `types.go` — workspace table and snapshot joins.
- `dispatcher/dispatcher.go`, `spawn.go`, `reconcile.go`, `run.go`, tests — lifecycle manager integration.
- `cli/dispatch.go`, `status.go`, `tui/model.go`, `tui/view.go`, tests — preflight, manager construction, and visibility.
- `configs/clipse.example.yaml`, `configs/clipse.yaml`, `README.md`, `AGENTS.md` — recommended setup and behavior.
- `Makefile` — opt-in live smoke target.

---

### Task 1: Backend configuration and Python dependency

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `agent/pyproject.toml`
- Modify: `agent/uv.lock`
- Modify: `configs/clipse.example.yaml`

**Interfaces:**
- Produces: `config.AgentBackend{Type string, Daytona config.DaytonaBackend}`.
- Produces: Daytona defaults of `60` minutes for idle stop and reviewer auto-delete.
- Preserves: absent backend block resolves to `local`.

- [ ] **Step 1: Write failing configuration tests**

Add table-driven tests with these assertions:

```go
func TestLoad_AgentBackendDefaultsToLocal(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML)
	if cfg.AgentBackend.Type != "local" {
		t.Fatalf("AgentBackend.Type = %q, want local", cfg.AgentBackend.Type)
	}
}

func TestLoad_DaytonaBackendDefaults(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML+`
agent_backend:
  type: daytona
`)
	if cfg.AgentBackend.Daytona.AutoStopMinutes != 60 {
		t.Errorf("AutoStopMinutes = %d, want 60", cfg.AgentBackend.Daytona.AutoStopMinutes)
	}
	if cfg.AgentBackend.Daytona.ReviewerAutoDeleteMinutes != 60 {
		t.Errorf("ReviewerAutoDeleteMinutes = %d, want 60", cfg.AgentBackend.Daytona.ReviewerAutoDeleteMinutes)
	}
}
```

Add rejection cases for `type: docker`, zero/negative intervals, and explicitly empty `snapshot` or `target` keys.

- [ ] **Step 2: Run the tests and verify the expected failure**

Run:

```bash
go test ./internal/config -run 'TestLoad_(AgentBackend|DaytonaBackend)' -count=1
```

Expected: compile failure because `Config.AgentBackend` does not exist.

- [ ] **Step 3: Implement config types and defaults**

Add:

```go
const (
	defaultAgentBackendType              = "local"
	defaultDaytonaAutoStopMinutes        = 60
	defaultReviewerAutoDeleteMinutes     = 60
)

type DaytonaBackend struct {
	AutoStopMinutes            int    `yaml:"auto_stop_minutes"`
	ReviewerAutoDeleteMinutes  int    `yaml:"reviewer_auto_delete_minutes"`
	Snapshot                   string `yaml:"snapshot"`
	Target                     string `yaml:"target"`
}

type AgentBackend struct {
	Type    string          `yaml:"type"`
	Daytona DaytonaBackend `yaml:"daytona"`
}
```

Mirror defaultable fields with pointers in `rawConfig`, resolve them in `Load`, and validate only `local` or `daytona`. Validate positive Daytona intervals only when Daytona is selected. Treat a present empty optional scalar as invalid by pointer-wrapping `snapshot` and `target` in the raw type.

- [ ] **Step 4: Enable the pinned Daytona Python extra**

Change the dependency entry to:

```toml
"deepagents-code[daytona]==0.1.22",
```

Then run:

```bash
cd agent && uv lock
```

Expected: the lock records the Daytona SDK and `langchain-daytona` without changing the DAC version.

- [ ] **Step 5: Update the example configuration**

Add the approved block with `type: daytona`, both 60-minute defaults, and commented optional snapshot/target examples. State beside it that omitting the block preserves local mode.

- [ ] **Step 6: Verify and commit**

```bash
go test ./internal/config -count=1
cd agent && uv run pytest tests/test_worker.py -q
git add internal/config/config.go internal/config/config_test.go agent/pyproject.toml agent/uv.lock configs/clipse.example.yaml
git commit -m "feat: configure agent backends"
```

---

### Task 2: Typed Python Daytona lifecycle protocol

**Files:**
- Create: `agent/src/clipse_agent/backends/__init__.py`
- Create: `agent/src/clipse_agent/backends/contracts.py`
- Create: `agent/src/clipse_agent/backends/github.py`
- Create: `agent/src/clipse_agent/backends/daytona.py`
- Create: `agent/tests/test_backend_contracts.py`
- Create: `agent/tests/test_daytona_backend.py`
- Modify: `agent/src/clipse_agent/worker.py`
- Modify: `agent/tests/test_worker.py`

**Interfaces:**
- Produces: `BackendActionRequest`, `BackendActionResult`, `BackendWorkspace`, and `BackendActionError`.
- Produces: `DaytonaLifecycle.ensure`, `.delete`, and `.list`.
- Produces: `clipse-worker --backend-action=<ensure|delete|list>` one-line JSON mode.

- [ ] **Step 1: Write failing contract and label tests**

```python
def test_coder_owner_and_labels_are_stable() -> None:
    request = BackendActionRequest(
        action="ensure",
        provider="daytona",
        repo_url="https://github.com/xlyk/clipse.git",
        repo_slug="xlyk/clipse",
        base_branch="main",
        branch="feat/CLI-1",
        issue_id="issue-1",
        run_id="run-1",
        role="coder",
        auto_stop_minutes=60,
        reviewer_auto_delete_minutes=60,
    )
    assert owner_key(request) == "daytona:xlyk/clipse:coder:issue-1"
    assert labels_for(request) == {
        "created-by": "clipse",
        "repo": repo_label("xlyk/clipse"),
        "issue": "issue-1",
        "role": "coder",
    }
```

Add equivalent reviewer assertions that include `run=run-1`, plus serialization tests proving absent errors are omitted and token-looking input never appears in `safe_error("clone", exc)`.

- [ ] **Step 2: Verify RED**

```bash
cd agent && uv run pytest tests/test_backend_contracts.py tests/test_daytona_backend.py -q
```

Expected: import failure because `clipse_agent.backends` does not exist.

- [ ] **Step 3: Implement contracts and sanitized GitHub auth**

Define Pydantic models with literal provider/role/state/error values. Implement:

```python
class BackendActionError(RuntimeError):
    def __init__(self, kind: ErrorKind, operation: str, message: str) -> None:
        super().__init__(message)
        self.kind = kind
        self.operation = operation


def github_token(run_host: HostRunner = subprocess_host_runner) -> str:
    run_host(["gh", "auth", "status", "--hostname", "github.com"])
    token = run_host(["gh", "auth", "token", "--hostname", "github.com"])
    if not token:
        raise BackendActionError("needs_input", "github_auth", "gh auth token returned empty")
    return token
```

`subprocess_host_runner` must discard stdout/stderr from exception messages and name only the operation and exit status.

- [ ] **Step 4: Implement fakeable Daytona lifecycle behavior**

`DaytonaLifecycle` accepts a client factory and token reader. Its `ensure` logic must be exactly:

```python
matches = self._matching(request)
if len(matches) > 1:
    ids = ", ".join(sorted(item.id for item in matches))
    raise BackendActionError("needs_input", "ensure", f"multiple matching sandboxes: {ids}")
if len(matches) == 1:
    sandbox = matches[0]
    if sandbox.state != "started":
        self._client.start(sandbox)
else:
    sandbox = self._create_and_clone(request)
return workspace_from(sandbox, request)
```

Create coders without auto-delete and reviewers with the configured auto-delete interval. Clone with `sandbox.git.clone(url=request.repo_url, path=REMOTE_REPO_REL, branch=clone_branch, username="x-access-token", password=token)` and assert the fake sandbox's environment and Git config remain token-free.

- [ ] **Step 5: Add backend-action mode to the worker**

Add mutually exclusive parser flags for action, provider, role, repository, branch, issue/run identity, sandbox ID, and lifecycle values. In `main`, dispatch backend actions before lane parsing and print `BackendActionResult.model_dump_json(exclude_none=True)` exactly once.

Map missing API key/auth to `needs_input`, provider timeouts to `transient`, and unsupported provider/action to `capability`. Return process exit `0` for a valid typed failure result so the Go caller can parse its category.

- [ ] **Step 6: Verify and commit**

```bash
cd agent && uv run pytest tests/test_backend_contracts.py tests/test_daytona_backend.py tests/test_worker.py -q
cd agent && uv run ruff check src tests
git add agent/src/clipse_agent/backends agent/src/clipse_agent/worker.py agent/tests/test_backend_contracts.py agent/tests/test_daytona_backend.py agent/tests/test_worker.py
git commit -m "feat: add daytona lifecycle protocol"
```

---

### Task 3: Durable agent workspace state

**Files:**
- Modify: `internal/store/migrations.go`
- Modify: `internal/store/migrations_test.go`
- Modify: `internal/store/types.go`
- Create: `internal/store/workspaces.go`
- Create: `internal/store/workspaces_test.go`

**Interfaces:**
- Produces: `store.AgentWorkspace` and lifecycle state constants.
- Produces: `UpsertAgentWorkspace`, `MarkWorkspaceCleanupPending`, `PendingWorkspaceCleanup`, `RecordWorkspaceCleanupError`, `MarkWorkspaceDeleted`, and `AgentWorkspacesByIssue`.

- [ ] **Step 1: Write failing migration and CRUD tests**

```go
func TestAgentWorkspace_CleanupRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := store.AgentWorkspace{
		OwnerKey: "daytona:xlyk/clipse:coder:issue-1",
		IssueID: "issue-1", Provider: "daytona", Role: "coder",
		ExternalID: "sb-1", WorkspacePath: "/home/daytona/workspace/clipse",
		State: store.WorkspaceActive, LastAction: "create", CreatedAt: 10, UpdatedAt: 10,
	}
	if err := s.UpsertAgentWorkspace(ctx, ws); err != nil { t.Fatal(err) }
	if err := s.MarkWorkspaceCleanupPending(ctx, ws.OwnerKey, 20); err != nil { t.Fatal(err) }
	rows, err := s.PendingWorkspaceCleanup(ctx)
	if err != nil { t.Fatal(err) }
	if len(rows) != 1 || rows[0].ExternalID != "sb-1" { t.Fatalf("rows = %#v", rows) }
}
```

Add an idempotent migration test against a pre-feature database and a test proving cleanup errors remain pending.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/store -run 'TestAgentWorkspace|TestOpen_.*AgentWorkspace' -count=1
```

Expected: compile failure because `AgentWorkspace` and its methods do not exist.

- [ ] **Step 3: Add the table and narrow store methods**

Create:

```sql
CREATE TABLE IF NOT EXISTS agent_workspaces (
    owner_key      TEXT PRIMARY KEY,
    issue_id       TEXT NOT NULL,
    run_id         TEXT NOT NULL DEFAULT '',
    provider       TEXT NOT NULL,
    role           TEXT NOT NULL,
    external_id    TEXT NOT NULL DEFAULT '',
    workspace_path TEXT NOT NULL,
    state           TEXT NOT NULL,
    last_action     TEXT NOT NULL DEFAULT '',
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_workspaces_issue ON agent_workspaces(issue_id);
CREATE INDEX IF NOT EXISTS idx_agent_workspaces_cleanup ON agent_workspaces(state);
```

Use explicit column lists for every query. `UpsertAgentWorkspace` updates mutable lifecycle fields but preserves the original `created_at`.

- [ ] **Step 4: Verify and commit**

```bash
go test ./internal/store -count=1
git add internal/store/migrations.go internal/store/migrations_test.go internal/store/types.go internal/store/workspaces.go internal/store/workspaces_test.go
git commit -m "feat: persist agent workspaces"
```

---

### Task 4: Go lifecycle command manager and provisioning

**Files:**
- Create: `internal/backend/backend.go`
- Create: `internal/backend/command.go`
- Create: `internal/backend/command_test.go`
- Modify: `internal/spawn/spawn.go`
- Modify: `internal/spawn/local.go`
- Modify: `internal/spawn/argv_test.go`
- Modify: `internal/spawn/env_test.go`
- Modify: `dispatcher/dispatcher.go`
- Modify: `dispatcher/spawn.go`
- Create: `dispatcher/backend_test.go`
- Modify: `cli/dispatch.go`

**Interfaces:**
- Produces: `backend.Manager` with `Ensure`, `Delete`, and `List`.
- Produces: `backend.CommandManager` using the configured worker command.
- Extends: `spawn.WorkerSpec` with provider, sandbox ID, repo URL/slug, and branch.

- [ ] **Step 1: Write failing command-manager tests**

```go
func TestCommandManagerEnsure_UsesSanitizedDaytonaEnv(t *testing.T) {
	runner := &recordingRunner{stdout: `{"ok":true,"provider":"daytona","owner_key":"k","external_id":"sb-1","workspace_path":"/home/daytona/workspace/clipse","state":"active"}`}
	m := backend.NewCommandManager([]string{"uv", "run", "clipse-worker"}, runner.Run, []string{
		"PATH=/bin", "HOME=/tmp/home", "DAYTONA_API_KEY=secret", "LINEAR_API_KEY=never",
	})
	_, err := m.Ensure(context.Background(), backend.EnsureRequest{Provider: "daytona", Role: "coder", IssueID: "i", RepoSlug: "x/y"})
	if err != nil { t.Fatal(err) }
	if slices.Contains(runner.env, "LINEAR_API_KEY=never") { t.Fatal("kernel secret forwarded") }
	if !slices.Contains(runner.env, "DAYTONA_API_KEY=secret") { t.Fatal("Daytona key missing") }
	if !slices.Contains(runner.argv, "--backend-action=ensure") { t.Fatalf("argv = %#v", runner.argv) }
}
```

Add tests for typed `needs_input`, malformed JSON, nonzero process exit, and ensuring no token appears in errors. Add a dispatcher spawn test proving a Daytona agent worker receives `DAYTONA_API_KEY`, optional `DAYTONA_API_URL`/`DAYTONA_TARGET`, `HOME`, and `PATH`, while the same host environment produces no Daytona variables in a local worker spec.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/backend ./dispatcher -run 'TestCommandManager|TestSpawnAttempt_Daytona' -count=1
```

Expected: package/import failure because `internal/backend` does not exist.

- [ ] **Step 3: Implement the provider-neutral manager**

Define:

```go
type Manager interface {
	Ensure(context.Context, EnsureRequest) (Workspace, error)
	Delete(context.Context, Workspace) error
	List(context.Context, ListRequest) ([]Workspace, error)
}

type ActionError struct {
	Kind ErrorKind
	Op   string
	Msg  string
}
```

`CommandManager` appends lifecycle flags to the configured worker prefix, passes only `PATH`, `HOME`, `DAYTONA_API_KEY`, `DAYTONA_API_URL`, and `DAYTONA_TARGET`, captures stdout, and parses one JSON object. It maps typed failure JSON to `ActionError` without including stderr or raw provider responses.

- [ ] **Step 4: Extend worker argv without changing local defaults**

Add optional `WorkerSpec` fields and append flags only when non-empty:

```go
Backend    string
SandboxID  string
RepoURL    string
RepoSlug   string
Branch     string
```

Expected Daytona argv includes `--backend=daytona`, `--sandbox-id`, `--repo-url`, `--repo-slug`, and `--branch`. A local `WorkerSpec` produces byte-for-byte the previous argv.

- [ ] **Step 5: Provision before spawn**

Add `backend.Manager` to `Dispatcher` through `WithBackendManager`. In local mode keep `d.ws.Ensure(issue)`. In Daytona mode call `Ensure`, upsert the returned workspace before `Spawner.Spawn`, and pass the remote path and metadata in `WorkerSpec`.

Build the Daytona worker environment by adding only `DAYTONA_API_KEY`, optional `DAYTONA_API_URL`/`DAYTONA_TARGET`, `HOME`, and `PATH` from the dispatcher process to the normal model/issue environment. Deduplicate by variable name. Keep the existing allow-listed environment byte-for-byte in local mode; never add Daytona variables there.

Map `backend.ActionError.Kind` to `parkOrRetry`: `transient` consumes bounded recovery; `needs_input` and `capability` park immediately. Do not classify every provisioning error as transient.

- [ ] **Step 6: Wire the composition root and preflight**

`runDispatch` builds `backend.CommandManager` from the resolved worker command only in Daytona mode. Before acquiring work, call a lightweight `List`/preflight that checks the API key and `gh auth` and returns an actionable startup error.

- [ ] **Step 7: Verify and commit**

```bash
go test ./internal/backend ./internal/spawn ./dispatcher ./cli -count=1
git add internal/backend internal/spawn/spawn.go internal/spawn/local.go internal/spawn/argv_test.go internal/spawn/env_test.go dispatcher/dispatcher.go dispatcher/spawn.go dispatcher/backend_test.go cli/dispatch.go
git commit -m "feat: provision daytona workspaces"
```

---

### Task 5: Runtime backend sessions and DAC injection

**Files:**
- Create: `agent/src/clipse_agent/backends/session.py`
- Create: `agent/src/clipse_agent/backends/local.py`
- Modify: `agent/src/clipse_agent/backends/daytona.py`
- Create: `agent/tests/test_local_backend.py`
- Modify: `agent/tests/test_daytona_backend.py`
- Modify: `agent/src/clipse_agent/dac.py`
- Modify: `agent/tests/test_dac.py`
- Modify: `agent/src/clipse_agent/worker.py`
- Modify: `agent/tests/test_worker.py`

**Interfaces:**
- Produces: graph-facing `AgentSession` protocol.
- Produces: `LocalSession` and `DaytonaSession`.
- Extends: `build_coder_agent(profile, checkpointer, cwd, *, sandbox=None, sandbox_type=None)`.
- Produces: shared `CommandResult` in `backends/session.py`; coder and reviewer import it instead of owning duplicate result types.

- [ ] **Step 1: Write failing session and DAC tests**

```python
def test_build_coder_agent_passes_daytona_sandbox(monkeypatch) -> None:
    calls: dict[str, object] = {}
    fake = object()
    monkeypatch.setattr(dac, "create_cli_agent", lambda *args, **kwargs: calls.update(kwargs) or (object(), object()))
    dac.build_coder_agent(profile(), None, "/home/daytona/workspace/clipse", sandbox=fake, sandbox_type="daytona")
    assert calls["sandbox"] is fake
    assert calls["sandbox_type"] == "daytona"
    assert calls["cwd"] == "/home/daytona/workspace/clipse"
```

Add `LocalSession.run` compatibility and `DaytonaSession.run` tests. The Daytona test must assert a command executes through `DaytonaSandbox.execute`, not `subprocess.run`.

- [ ] **Step 2: Verify RED**

```bash
cd agent && uv run pytest tests/test_local_backend.py tests/test_daytona_backend.py tests/test_dac.py -q
```

Expected: missing modules and unexpected `sandbox` keyword.

- [ ] **Step 3: Define the session protocol**

```python
@dataclass(frozen=True)
class CommandResult:
    returncode: int
    stdout: str = ""
    stderr: str = ""


class AgentSession(Protocol):
    provider: str
    cwd: str
    sandbox: Any | None
    sandbox_type: str | None

    def run(self, argv: Sequence[str]) -> CommandResult:
        raise NotImplementedError

    def sync_base(self, base_branch: str) -> CommandResult:
        raise NotImplementedError

    def commit(self, message: str) -> CommandResult:
        raise NotImplementedError

    def push(self, branch: str) -> CommandResult:
        raise NotImplementedError

    def github(self, argv: Sequence[str]) -> CommandResult:
        raise NotImplementedError
```

Implement `LocalSession` with the current subprocess semantics. Implement `DaytonaSession` with `DaytonaSandbox.execute` for ordinary commands, authenticated Daytona Git API calls for pull/commit/push, and host `gh --repo <slug>` for GitHub operations.

- [ ] **Step 4: Inject the backend into DAC**

Pass `sandbox` and `sandbox_type` to `create_cli_agent` only when present; leave the current call shape unchanged in local mode. Continue returning DAC's backend as the second tuple element.

- [ ] **Step 5: Construct the session in the worker**

Local flags build `LocalSession(args.workspace, repo_slug)`. Daytona flags fetch the sandbox by ID, create `DaytonaSandbox`, and build `DaytonaSession`. Pass the session into each graph builder and bind an agent factory that supplies its sandbox to `build_coder_agent`.

- [ ] **Step 6: Verify and commit**

```bash
cd agent && uv run pytest tests/test_local_backend.py tests/test_daytona_backend.py tests/test_dac.py tests/test_worker.py -q
git add agent/src/clipse_agent/backends agent/src/clipse_agent/dac.py agent/src/clipse_agent/worker.py agent/tests/test_local_backend.py agent/tests/test_daytona_backend.py agent/tests/test_dac.py agent/tests/test_worker.py
git commit -m "feat: route dac through backend sessions"
```

---

### Task 6: Route the coder and docs workflow through sessions

**Files:**
- Modify: `agent/src/clipse_agent/graphs/coder.py`
- Modify: `agent/tests/test_coder_graph.py`
- Modify: `agent/src/clipse_agent/backends/local.py`
- Modify: `agent/src/clipse_agent/backends/daytona.py`
- Modify: `agent/tests/test_daytona_backend.py`

**Interfaces:**
- Consumes: `AgentSession.run`, `.sync_base`, `.commit`, `.push`, and `.github`.
- Preserves: Coder graph node order and `WorkerResult` outcomes.

- [ ] **Step 1: Write failing remote-routing tests**

Build a `RecordingSession` and assert the happy path records:

```python
assert session.operations == [
    ("verify_repo",),
    ("sync_base", "main"),
    ("commit", "feat: implement issue"),
    ("push", "feat/CLI-1"),
    ("github", ("pr", "view", "feat/CLI-1", "--json", "url", "--jq", ".url")),
]
```

Add tests proving a remote workspace path is never checked with host `Path.exists`, merge conflicts still reach DAC, unresolved markers still block commit, and local graph tests retain their current call order.

- [ ] **Step 2: Verify RED**

```bash
cd agent && uv run pytest tests/test_coder_graph.py -k 'session or remote' -q
```

Expected: `build_coder_graph` rejects `session` or performs a host filesystem check.

- [ ] **Step 3: Replace direct subprocess ownership with session calls**

`build_coder_graph` accepts `session: AgentSession | None`. When absent, it constructs the legacy local adapter from state for existing direct tests. `ensure_worktree` calls `session.run(["git", "rev-parse", "--is-inside-work-tree"])`; it never calls host `Path.exists` for Daytona.

`sync_base`, `make_commit`, `make_push`, and `make_open_pr` delegate to session methods. Preserve all current conflict parsing, structured-tail handling, commit-title derivation, empty-branch checks, and PR-body text.

- [ ] **Step 4: Implement Daytona Git semantics**

`sync_base` uses `sandbox.git.pull(self.repo_path, username="x-access-token", password=token, branch=base_branch, remote="origin")`. `commit` stages the repository with `sandbox.git.add(self.repo_path, ["."])`, configures the existing Clipse author, and calls `sandbox.git.commit(path=self.repo_path, message=message, author=GIT_AUTHOR_NAME, email=GIT_AUTHOR_EMAIL)`. `push` calls `sandbox.git.push(path=self.repo_path, username="x-access-token", password=token, branch=branch, remote="origin", set_upstream=True)`.

After every authenticated SDK call, tests assert the token is absent from `env`, `.git/config`, the returned `CommandResult`, and the fake transcript sink.

- [ ] **Step 5: Verify and commit**

```bash
cd agent && uv run pytest tests/test_coder_graph.py tests/test_daytona_backend.py tests/test_local_backend.py -q
git add agent/src/clipse_agent/graphs/coder.py agent/src/clipse_agent/backends/local.py agent/src/clipse_agent/backends/daytona.py agent/tests/test_coder_graph.py agent/tests/test_daytona_backend.py
git commit -m "feat: run coder workflow in daytona"
```

---

### Task 7: Fresh reviewer workflow and host GitHub operations

**Files:**
- Modify: `agent/src/clipse_agent/graphs/reviewer.py`
- Modify: `agent/tests/test_reviewer_graph.py`
- Modify: `agent/src/clipse_agent/backends/github.py`
- Modify: `agent/src/clipse_agent/backends/daytona.py`
- Modify: `dispatcher/reconcile.go`
- Modify: `dispatcher/backend_test.go`

**Interfaces:**
- Consumes: run-scoped reviewer workspace from Task 4.
- Produces: authoritative `gh pr diff --repo` loading and host inline comments.
- Produces: reviewer cleanup-pending state after every result.

- [ ] **Step 1: Write failing reviewer isolation tests**

```python
def test_reviewer_diff_comes_from_host_github(recording_session) -> None:
    node = reviewer.make_load_diff(recording_session)
    state = {"cwd": "/home/daytona/workspace/clipse", "branch": "feat/CLI-1", "task_text": "review"}
    result = node(state)
    assert recording_session.github_calls[0] == (
        "pr", "diff", "feat/CLI-1", "--repo", "xlyk/clipse"
    )
    assert "provided for your review" in result["task_text"]
```

Add tests proving review comments call host `gh --repo`, no reviewer token enters the sandbox, and the dispatcher marks the run-scoped workspace cleanup pending for PASS, CHANGES_REQUESTED, blocked, malformed result, and worker crash.

- [ ] **Step 2: Verify RED**

```bash
cd agent && uv run pytest tests/test_reviewer_graph.py -k 'github or session' -q
go test ./dispatcher -run TestReviewerWorkspaceCleanup -count=1
```

Expected: the reviewer still shells out to local `git fetch/diff` and cleanup state is absent.

- [ ] **Step 3: Route reviewer GitHub operations through the session**

Replace local fetch/diff with `session.github(["pr", "diff", branch])`. Keep the 60,000-character cap and omitted-file instructions; obtain changed-file names with host `gh pr diff --name-only` instead of remote local-base assumptions.

Route PR lookup and inline/summary comments through `session.github`. The session appends `--repo <slug>` once and strips raw stderr from raised errors.

- [ ] **Step 4: Queue reviewer cleanup after process exit**

In dispatcher reconciliation, once `RunHandle.Wait` has returned, mark that run's reviewer owner key `cleanup_pending` before applying or retrying its worker result. This must happen for every exit shape and must not delete the persistent coder workspace.

- [ ] **Step 5: Verify and commit**

```bash
cd agent && uv run pytest tests/test_reviewer_graph.py tests/test_daytona_backend.py -q
go test ./dispatcher -run 'TestReviewerWorkspaceCleanup|TestApplyResult' -count=1
git add agent/src/clipse_agent/graphs/reviewer.py agent/src/clipse_agent/backends/github.py agent/src/clipse_agent/backends/daytona.py agent/tests/test_reviewer_graph.py dispatcher/reconcile.go dispatcher/backend_test.go
git commit -m "feat: isolate daytona reviewer runs"
```

---

### Task 8: Durable cleanup and startup reconciliation

**Files:**
- Create: `dispatcher/backend.go`
- Modify: `dispatcher/backend_test.go`
- Modify: `dispatcher/dispatcher.go`
- Modify: `dispatcher/run.go`
- Modify: `dispatcher/promote.go`
- Modify: `dispatcher/reconcile.go`
- Modify: `internal/store/outbox.go`
- Modify: `internal/store/outbox_test.go`
- Modify: `dispatcher/workspace.go`

**Interfaces:**
- Produces: `drainWorkspaceCleanup(ctx)` and `reconcileAgentWorkspaces(ctx)`.
- Extends: terminal transitions with atomic coder cleanup scheduling.
- Extends: `Workspacer` with idempotent `Remove(issue)` for local cancellation cleanup.

- [ ] **Step 1: Write failing cleanup and reconciliation tests**

Cover these exact cases:

```go
func TestWorkspaceCleanupFailureRemainsPending(t *testing.T) {
	d, st, mgr := newBackendDispatcher(t)
	seedPendingCoderWorkspace(t, st, "issue-1", "sb-1")
	mgr.deleteErr = &backend.ActionError{Kind: backend.Transient, Op: "delete", Msg: "provider unavailable"}
	if err := d.Tick(context.Background()); err != nil { t.Fatal(err) }
	ws := mustWorkspace(t, st, "daytona:repo:coder:issue-1")
	if ws.State != store.WorkspaceCleanupPending { t.Fatalf("state = %q", ws.State) }
	if ws.LastError != "provider unavailable" { t.Fatalf("error = %q", ws.LastError) }
}
```

Add tests for retry-success, terminal done/cancelled atomic pending state, unknown-issue orphan deletion, active-run preservation, missing-row restoration, duplicate-coder non-deletion, and local cancelled-worktree removal.

- [ ] **Step 2: Verify RED**

```bash
go test ./dispatcher ./internal/store -run 'TestWorkspace|TestTerminal.*Cleanup|TestReconcileAgent' -count=1
```

Expected: missing cleanup/reconciliation methods and terminal transition behavior.

- [ ] **Step 3: Make terminal cleanup atomic**

Add `CleanupCoderWorkspace bool` to `store.TransitionReq`. Inside the same transaction as a `done` or `cancelled` state change, update the matching coder row to `cleanup_pending`. Set this flag only when the configured backend has an owned workspace; reviewer cleanup remains run-driven.

- [ ] **Step 4: Drain pending cleanup every tick**

After worker reconciliation and before claiming new work, list pending rows, call the manager's idempotent `Delete`, and mark success or sanitized failure. A cleanup error is logged and retained but does not fail the whole tick or alter board state.

- [ ] **Step 5: Reconcile once at startup**

Before orphan-run recovery and the polling loop, list Clipse-labeled remote workspaces. Reconcile by the approved rules: restore a sole active coder row, mark terminal/unknown/reviewer-orphan rows pending, preserve open-run rows, mark missing remote rows deleted, and emit a needs-input event for duplicates without deleting them.

- [ ] **Step 6: Preserve local cleanup semantics**

Add `Remove(issue store.Issue) error` to `Workspacer`, backed by `spawn.RemoveWorktree`. Mark local rows deleted after successful Git-operator cleanup and remove the previously-unwired cancelled worktree through the same idempotent method.

- [ ] **Step 7: Verify and commit**

```bash
go test ./dispatcher ./internal/store ./internal/spawn -count=1
git add dispatcher/backend.go dispatcher/backend_test.go dispatcher/dispatcher.go dispatcher/run.go dispatcher/promote.go dispatcher/reconcile.go dispatcher/workspace.go internal/store/outbox.go internal/store/outbox_test.go
git commit -m "feat: reconcile agent workspace cleanup"
```

---

### Task 9: Fetch Daytona-created branches for the Git operator

**Files:**
- Modify: `internal/spawn/worktree.go`
- Modify: `internal/spawn/worktree_test.go`
- Modify: `dispatcher/gitops_test.go`

**Interfaces:**
- Extends: `EnsureWorktree` to prefer `origin/<feature>` when the feature exists remotely.
- Preserves: new local branch creation from the fetched remote base when no feature exists.

- [ ] **Step 1: Write the failing real-Git regression test**

```go
func TestEnsureWorktree_UsesRemoteFeatureBranchWhenLocalBranchMissing(t *testing.T) {
	origin, primary := newRemoteAndClone(t, "main")
	runGit(t, origin, "checkout", "-b", "feat/CLI-1")
	writeFile(t, origin, "daytona.txt", "remote agent commit\n")
	runGit(t, origin, "add", "daytona.txt")
	runGit(t, origin, "commit", "-m", "feat: remote change")
	want := runGit(t, origin, "rev-parse", "HEAD")

	path, err := spawn.EnsureWorktree(ctxWithTimeout(t), primary, "feat/CLI-1", "main", t.TempDir())
	if err != nil { t.Fatal(err) }
	if got := runGit(t, path, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD = %s, want remote feature %s", got, want)
	}
}
```

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/spawn -run TestEnsureWorktree_UsesRemoteFeatureBranchWhenLocalBranchMissing -count=1
```

Expected: worktree branches from `origin/main`, so its HEAD differs from the feature commit.

- [ ] **Step 3: Fetch and select the remote feature**

When no local branch exists, run `git fetch origin <branch>`. If `refs/remotes/origin/<branch>` now exists, create the worktree with:

```bash
git worktree add -b <branch> <path> origin/<branch>
```

Only fall back to the existing fetched-base behavior when the feature ref is absent. Preserve stale-registration prune-and-retry behavior.

- [ ] **Step 4: Verify and commit**

```bash
go test ./internal/spawn ./internal/gitops ./dispatcher -count=1
git add internal/spawn/worktree.go internal/spawn/worktree_test.go dispatcher/gitops_test.go
git commit -m "fix: fetch remote agent branches"
```

---

### Task 10: Status, TUI, documentation, and live production smoke

**Files:**
- Modify: `internal/store/crud.go`
- Modify: `internal/store/crud_test.go`
- Modify: `internal/store/types.go`
- Modify: `cli/status.go`
- Modify: `cli/status_test.go`
- Modify: `cli/tui/model.go`
- Modify: `cli/tui/view.go`
- Modify: `cli/tui/model_test.go`
- Modify: `cli/tui/view_test.go`
- Create: `scripts/smoke_daytona_backend.py`
- Modify: `Makefile`
- Modify: `README.md`
- Modify: `AGENTS.md`
- Modify: `configs/clipse.yaml`
- Modify: `scripts/poc/daytona_agent_backend.py`

**Interfaces:**
- Extends: status snapshots with the current workspace.
- Produces: `make smoke-daytona-backend` opt-in live verification.
- Produces: setup and operating documentation.

- [ ] **Step 1: Write failing status and TUI tests**

Seed one active Daytona coder row and assert:

```go
var out bytes.Buffer
if err := RenderStatus(&out, snapshot); err != nil { t.Fatal(err) }
if got := out.String(); !strings.Contains(got, "daytona  coder  active  sb-12345") {
	t.Fatalf("status missing backend columns:\n%s", got)
}
```

Add TUI detail assertions for full sandbox ID, last action, remote path, and sanitized error. Add local/no-workspace display cases.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/store ./cli ./cli/tui -run 'Test.*Workspace|Test.*Backend' -count=1
```

Expected: snapshots and views contain no workspace metadata.

- [ ] **Step 3: Join workspace state into snapshots and views**

Add `Workspace *AgentWorkspace` to each snapshot row, selecting the persistent coder row first and an active reviewer row when no coder row exists. Status prints provider, role, state, and the first eight sandbox-ID characters. TUI details print the full values and omit an empty error line.

- [ ] **Step 4: Add the live smoke with unconditional cleanup**

The smoke imports production `DaytonaLifecycle`, `DaytonaSession`, DAC construction, and host GitHub helpers. It creates unique `smoke/daytona-<nonce>` resources, prints IDs immediately, and puts all cleanup in `finally`:

```python
finally:
    if pr_url:
        run_host(["gh", "pr", "close", pr_url, "--delete-branch"])
    elif branch_pushed:
        run_host(["gh", "api", "-X", "DELETE", f"repos/{repo_slug}/git/refs/heads/{branch}"])
    for workspace in reversed(created_workspaces):
        lifecycle.delete(workspace)
```

The smoke runs one real coder DAC turn, commits/pushes through Daytona SDK, opens a draft PR, runs one real reviewer DAC turn from a fresh sandbox, verifies the authoritative PR diff, closes the PR, deletes the branch, deletes both sandboxes, and lists labels to require zero leftovers. It never merges.

Add:

```make
smoke-daytona-backend:
	cd agent && uv run python ../scripts/smoke_daytona_backend.py
```

- [ ] **Step 5: Update documentation and real config**

Document `gh auth login`, `DAYTONA_API_KEY`, explicit backend selection, the 60-minute idle stop, persistent coder/disposable reviewer split, no silent fallback, status fields, cleanup recovery, and local mode. Update `configs/clipse.yaml` to select Daytona for this installation without adding a secret value. Keep the POC as a focused diagnostic but import shared safe GitHub helpers instead of duplicating them.

- [ ] **Step 6: Run focused and full verification**

```bash
make test
make lint
make codegen
git diff --exit-code -- internal/contract/contract.go agent/src/clipse_agent/contract.py
```

Expected: all Go race tests, all Python tests, vet, formatting, Ruff, and generated-contract drift checks pass.

- [ ] **Step 7: Run the live production-path smoke**

```bash
uv run --env-file .env --project agent python scripts/smoke_daytona_backend.py
```

Expected: coder and reviewer production sessions pass, the draft PR closes, its branch is deleted, both sandboxes are deleted, and the final label query reports zero smoke/reviewer leftovers.

- [ ] **Step 8: Final security scan and commit**

```bash
rg -n 'DAYTONA_API_KEY|GH_TOKEN|GITHUB_TOKEN|gh auth token' . \
  --glob '!agent/uv.lock' --glob '!docs/superpowers/**'
git status --short
git add internal/store/crud.go internal/store/crud_test.go internal/store/types.go cli/status.go cli/status_test.go cli/tui/model.go cli/tui/view.go cli/tui/model_test.go cli/tui/view_test.go scripts/smoke_daytona_backend.py Makefile README.md AGENTS.md configs/clipse.yaml scripts/poc/daytona_agent_backend.py
git commit -m "feat: finish daytona agent backend"
```

Review every scan hit and confirm it is a variable name, allow-list rule, safe setup instruction, or negative security assertion—never a credential value.

---

## Final verification checklist

- [ ] Old configuration without `agent_backend` runs the unchanged local path.
- [ ] Explicit local configuration creates no Daytona process or network call.
- [ ] Explicit Daytona configuration preflights before claiming work.
- [ ] Coder and docs use the same persistent sandbox across rework and restart.
- [ ] Reviewer uses a fresh run-scoped sandbox and deletes it after every outcome.
- [ ] Missing API key/auth parks as `needs_input`; provider outages use bounded transient retry.
- [ ] No Daytona failure silently falls back to local.
- [ ] GitHub and Daytona credentials are absent from every agent-visible and durable surface.
- [ ] Terminal and cancelled issues eventually delete coder sandboxes.
- [ ] Startup reconciliation preserves active work and cleans terminal/unknown orphans.
- [ ] Git operator checks out the remote Daytona feature branch rather than a new base branch.
- [ ] Status and TUI show accurate lifecycle metadata.
- [ ] `make test`, `make lint`, codegen drift, and the live production smoke pass.
- [ ] GitHub and Daytona final queries show no smoke artifacts.
