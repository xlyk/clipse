# Daytona Agent Backend POC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and run one disposable script that proves a host-side Clipse DAC agent can route filesystem and shell tools through a reattachable Daytona sandbox without exposing host GitHub credentials.

**Architecture:** A standalone host process obtains `gh` credentials, creates a Daytona sandbox, and clones the current repository through Daytona's Git API. It passes `langchain_daytona.DaytonaSandbox` directly to DAC's `create_cli_agent`, independently verifies the remote file and shell result, rewraps the same sandbox, and deletes it in `finally`.

**Tech Stack:** Python 3.13, `deepagents-code==0.1.22`, `langchain-daytona`, Daytona Python SDK, GitHub CLI

## Global Constraints

- Add one script: `scripts/poc/daytona_agent_backend.py`.
- Install Daytona support only at runtime with `uv run --project agent --with "deepagents-code[daytona]==0.1.22"`.
- Do not modify production Python/Go code, dependency files, schemas, configuration, Makefile, or board state.
- Perform no GitHub write: no branch, commit, push, PR, or merge.
- Never print, persist, prompt with, or inject the GitHub token into the sandbox environment.
- Always attempt sandbox deletion after creation.
- Preserve unrelated untracked files in the shared worktree.

---

## File structure

- Create `scripts/poc/daytona_agent_backend.py`: complete POC controller and live executable.
- Create `docs/superpowers/plans/2026-07-11-daytona-agent-backend-poc.md`: this execution plan only.

No test module is added; the approved feasibility spec deliberately uses the live experiment as its test.

### Task 1: Build the standalone POC controller

**Files:**
- Create: `scripts/poc/daytona_agent_backend.py`

**Interfaces:**
- Consumes: `DAYTONA_API_KEY`, optional `DAYTONA_API_URL`, optional `CLIPSE_POC_MODEL`, host `gh auth`, and current Git `origin`.
- Produces: process exit `0` plus `PASS: Daytona agent backend POC` on success; nonzero exit plus a sanitized `FAIL:` line otherwise.

- [ ] **Step 1: Prove the executable does not exist yet**

Run:

```bash
uv run --project agent --with "deepagents-code[daytona]==0.1.22" \
  python scripts/poc/daytona_agent_backend.py --help
```

Expected: nonzero exit because `scripts/poc/daytona_agent_backend.py` does not exist.

- [ ] **Step 2: Add argument parsing and preflight**

Implement these exact helpers:

```python
def run_host(argv: list[str]) -> str:
    result = subprocess.run(argv, text=True, capture_output=True, check=False)
    if result.returncode != 0:
        raise PocError(f"host command failed: {argv[0]} {argv[1] if len(argv) > 1 else ''}".strip())
    return result.stdout.strip()


def github_repo() -> tuple[str, str]:
    remote = run_host(["git", "remote", "get-url", "origin"])
    match = re.fullmatch(r"git@github\.com:(?P<slug>[^/]+/[^/]+?)(?:\.git)?", remote)
    if match is None:
        match = re.fullmatch(r"https://github\.com/(?P<slug>[^/]+/[^/]+?)(?:\.git)?/?", remote)
    if match is None:
        raise PocError("origin must be a github.com SSH or HTTPS URL")
    slug = match.group("slug")
    branch = run_host(
        ["gh", "repo", "view", slug, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name"]
    )
    return f"https://github.com/{slug}.git", branch


def github_token() -> str:
    run_host(["gh", "auth", "status", "--hostname", "github.com"])
    token = run_host(["gh", "auth", "token", "--hostname", "github.com"])
    if not token:
        raise PocError("gh auth token returned an empty token")
    return token


def require_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise PocError(f"{name} is required")
    return value
```

`github_repo` must normalize SSH and HTTPS GitHub remotes and use `gh repo view --json defaultBranchRef` for the base branch. `github_token` must call `gh auth status` before `gh auth token`. No exception may contain the token.

- [ ] **Step 3: Add Daytona lifecycle and authenticated clone**

Create the client and disposable sandbox:

```python
client = Daytona(DaytonaConfig(api_key=api_key, api_url=os.getenv("DAYTONA_API_URL")))
sandbox = client.create(
    CreateSandboxFromSnapshotParams(
        labels={"created-by": "clipse-daytona-poc"},
        ephemeral=True,
        auto_stop_interval=10,
    )
)
```

Clone with SDK arguments, never a token-bearing shell command:

```python
sandbox.git.clone(
    url=clone_url,
    path="workspace/clipse",
    branch=base_branch,
    username="x-access-token",
    password=token,
)
```

Print the sandbox ID before clone/agent execution. Put `client.delete(sandbox)` in `finally` and report deletion failure without printing provider request details.

- [ ] **Step 4: Build and drive the host-side DAC agent**

Resolve the model on the host with `deepagents_code.config.create_model`. Construct DAC with:

```python
agent, _ = create_cli_agent(
    model,
    "clipse-daytona-poc",
    sandbox=backend,
    sandbox_type="daytona",
    system_prompt=POC_SYSTEM_PROMPT,
    interactive=False,
    auto_approve=True,
    enable_ask_user=False,
    enable_memory=False,
    enable_skills=False,
    enable_shell=True,
    checkpointer=None,
    cwd=REMOTE_REPO,
)
```

Call `clipse_agent.dac.drive_turn` with one task containing a random nonce and the absolute remote file path. Do not include the GitHub token in any state passed to the agent.

- [ ] **Step 5: Add independent verification and reattachment**

Implement:

```python
def read_text(backend: DaytonaSandbox, path: str) -> str:
    result = backend.read(path)
    if result.error or result.file_data is None:
        raise PocError(f"cannot read remote file: {path}")
    return result.file_data["content"]


def assert_remote_state(backend: DaytonaSandbox, nonce: str, token: str) -> None:
    if read_text(backend, REMOTE_FILE) != nonce:
        raise PocError("remote nonce file does not match")
    check = backend.execute(f"test \"$(cat {shlex.quote(REMOTE_FILE)})\" = {shlex.quote(nonce)}")
    if check.exit_code != 0:
        raise PocError("independent remote shell assertion failed")
    env_check = backend.execute("env | grep -E '^(GH_TOKEN|GITHUB_TOKEN)='")
    if env_check.exit_code == 0:
        raise PocError("GitHub token variable reached sandbox environment")
    if token in read_text(backend, f"{REMOTE_REPO}/.git/config"):
        raise PocError("GitHub token reached sandbox Git config")
```

The verification must require the exact nonce, run an independent shell equality test, reject `GH_TOKEN`/`GITHUB_TOKEN` in `env`, reject the token in `.git/config`, and reject the token in DAC output. Then call `client.get(sandbox.id)`, create a second `DaytonaSandbox`, and repeat the file and shell checks.

- [ ] **Step 6: Verify the local CLI behavior**

Run:

```bash
uv run --project agent --with "deepagents-code[daytona]==0.1.22" \
  python scripts/poc/daytona_agent_backend.py --help
```

Expected: exit `0` and usage containing `CLIPSE_POC_MODEL`.

Run without `DAYTONA_API_KEY`:

```bash
env -u DAYTONA_API_KEY uv run --project agent \
  --with "deepagents-code[daytona]==0.1.22" \
  python scripts/poc/daytona_agent_backend.py
```

Expected: nonzero exit and a sanitized setup message; no sandbox is created.

### Task 2: Run the live proof and repository gates

**Files:**
- Verify: `scripts/poc/daytona_agent_backend.py`

**Interfaces:**
- Consumes: a real Daytona account/API key, existing host `gh` auth, and a live model credential.
- Produces: live evidence for the six acceptance criteria in the approved spec.

- [ ] **Step 1: Run the live POC**

```bash
DAYTONA_API_KEY="$DAYTONA_API_KEY" \
CLIPSE_POC_MODEL="anthropic:claude-sonnet-4-6" \
uv run --project agent --with "deepagents-code[daytona]==0.1.22" \
  python scripts/poc/daytona_agent_backend.py
```

Expected: the script prints the sandbox ID, completes the DAC turn, verifies first and second wrappers, reports sandbox deletion, and ends with `PASS: Daytona agent backend POC`.

- [ ] **Step 2: Confirm no local or remote Git mutation**

Run:

```bash
git status --short
gh api repos/xlyk/clipse/branches --paginate --jq '.[].name'
```

Expected: only the planned script/plan plus pre-existing untracked files locally; no POC branch remotely.

- [ ] **Step 3: Run repository verification**

```bash
make test
make lint
```

Expected: Go race tests, Python tests, `go vet`, formatting check, and Ruff all pass.

- [ ] **Step 4: Commit exact POC artifacts**

```bash
git add scripts/poc/daytona_agent_backend.py \
  docs/superpowers/plans/2026-07-11-daytona-agent-backend-poc.md
git commit -m "test: add daytona agent backend poc"
```

Do not stage `.claude/`, `docs/reviews/`, local configs, or unrelated drafts.
