# Clipse configuration guide

This guide covers the complete operator setup for a Clipse dispatcher, including Daytona-backed agent execution, Linear workflow-state versus label-backed state, and multiple independent dispatcher instances.

Clipse uses one YAML configuration and one dispatcher process for each Linear team and GitHub repository pair. Secrets stay in the process environment; they do not belong in the YAML file.

## Configuration model

### Interactive wizard

Build Clipse and launch the recommended terminal setup flow:

```sh
make build
./bin/clipse configure
```

The Bubble Tea wizard guides the operator through every current YAML field and
uses Daytona as the new-config preset. Its Review screen shows the exact YAML,
an imported-file diff when `--from` is used, and a read-only readiness report
covering host tools, repository/base-branch access, GitHub, Linear team/state
ownership, candidate discovery, the worker command, model authentication, and
Daytona's lifecycle `list` preflight.

Useful controls:

- `Up`/`Down`, `Tab`, and `Enter` move through fields.
- `Esc` returns to the previous step; `Ctrl+C` exits without writing.
- `F2` toggles advanced fields.
- `F4` discovers teams visible to the current `LINEAR_API_KEY`.
- `F5` temporarily hands the terminal to `dcode` for `openai_codex` `/auth`.
- `F3` toggles an optional, procedurally generated techno loop.
- `R` reruns readiness checks; `W` writes from Review.

The soundtrack uses `afplay` on macOS or `pw-play`/`paplay`/`aplay` on Linux
when present. It is opt-in in the default `--music auto` mode, never blocks
configuration, and has a silent fallback. Use `--music on|off`,
`--no-animation`, or `--no-color` to pin the desired behavior.

Create or import a named instance with:

```sh
./bin/clipse configure --output configs/product-a.local.yaml
./bin/clipse configure --from configs/product-a.local.yaml --mode advanced
```

The wizard writes mode `0600`, backs up an existing destination only after a
second confirmation, reloads the final file with the production parser, and
creates the selected board/checkpoint directories. It does not persist secret
values or mutate Linear, GitHub, Daytona, or model-provider resources. A live
Daytona smoke remains the separate, explicit verification step described
below.

### Configuration ownership

Each dispatcher owns four distinct kinds of state:

| Concern | Configuration source | Isolation requirement |
| --- | --- | --- |
| GitHub repository | `repo.remote`, `repo.path`, `repo.base_branch` | One primary clone per managed repository |
| Linear workspace and team | Process `LINEAR_API_KEY`, plus `team_key` and `team_id` | The key selects the workspace; the YAML scopes the team |
| Runtime state | `board_dir` | Unique for every concurrently running dispatcher |
| LangGraph checkpoints | `checkpoints_dir` | Unique for every dispatcher |

The dispatcher reads its YAML from `--config`. `CLIPSE_CONFIG` can provide the default path, but an explicit flag is easier to audit when several instances run on the same host:

```sh
./bin/clipse dispatch --config /absolute/path/to/clipse.yaml
```

The `--board` flag overrides `board_dir` for one invocation. Prefer setting an absolute `board_dir` in each config so status commands, service definitions, and restarts all resolve the same state.

## Host prerequisites

Install the toolchain documented by this checkout:

- Go 1.25 or newer to build the dispatcher.
- Python 3.13 or newer.
- `uv` for the pinned worker environment.
- `git` for the host primary clone and deterministic Git-operator worktrees.
- `gh`, authenticated to GitHub, for PR, review, check, protection, and merge operations.
- Network access to Linear, GitHub, the selected model provider, and Daytona when it is enabled.

Build and verify Clipse from the repository root:

```sh
make build
make test
./bin/clipse --help
uv run --no-sync --project agent clipse-worker --help
```

The worker project pins `deepagents-code[daytona]` and its Daytona SDK dependencies through `agent/uv.lock`. Use the lockfile instead of upgrading the Daytona SDK independently.

## Credentials and process environment

### Linear

`LINEAR_API_KEY` is required by both `clipse dispatch` and `clipse board`. The key chooses the Linear workspace; `team_key` and `team_id` then restrict operations to one team in that workspace.

The dispatcher retains this credential in the kernel. It must never appear in `env_allowlist`, because issue-driven workers must not receive it.

On macOS, a Keychain-backed launch can look like:

```sh
LINEAR_API_KEY="$(security find-generic-password \
  -a "$USER" \
  -s your-clipse-linear-key \
  -w)" \
./bin/clipse dispatch --config /absolute/path/to/clipse.yaml
```

### GitHub

Authenticate the host user that will run the dispatcher:

```sh
gh auth login --hostname github.com
gh auth status --hostname github.com
```

That identity needs access to clone and push the repository, create and comment on pull requests, read checks and branch protection, mark draft PRs ready, and merge according to repository policy. The local primary clone must also be able to fetch its `origin` using its configured SSH key or credential helper.

In Daytona mode, Clipse does not place `GH_TOKEN` or `GITHUB_TOKEN` in the sandbox or worker environment. Trusted host code asks the authenticated `gh` CLI for a token only when a Daytona SDK Git operation needs one.

### Model provider

The default models are:

```yaml
models:
  coder: "anthropic:claude-sonnet-4-6"
  coder_docs: "anthropic:claude-sonnet-4-6"
  reviewer: "anthropic:claude-opus-4-6"
```

Those defaults require `ANTHROPIC_API_KEY` in the launch environment and in `env_allowlist`.

For an `openai_codex:*` model, authenticate once as the same OS user that runs the dispatcher:

```sh
uv --project /absolute/path/to/clipse/agent run dcode
```

Inside `dcode`, run `/auth`, select `openai_codex`, and complete the sign-in. The credential is stored at `~/.deepagents/.state/chatgpt-auth.json` under the inherited `HOME`. It auto-refreshes, but a revoked or expired refresh token requires another sign-in.

For another provider, add only that provider's required environment variables to `env_allowlist`. Model calls run in the host worker process; Daytona runs the agent's filesystem and shell tools, not the model client.

### Daytona

Create a Daytona API key that can list, create/start, and delete sandboxes. In Daytona's current scope names, that requires sandbox write and delete permissions. Store it securely and inject it as `DAYTONA_API_KEY` when the dispatcher starts.

Clipse recognizes these controller variables:

- `DAYTONA_API_KEY` — required.
- `DAYTONA_API_URL` — optional SDK endpoint override.
- `DAYTONA_TARGET` — optional SDK target/region override.

Do not add them to `env_allowlist`; configuration validation rejects Daytona controller variables there. An explicit `agent_backend.daytona.target` value wins over `DAYTONA_TARGET`.

The dispatcher does not automatically load a `.env` file. Use the shell, a service manager, or a secret manager to construct its environment.

## Complete Daytona configuration

Start from `configs/clipse.example.yaml`. This reduced example shows every field that establishes identity, isolation, or credentials for a Daytona-backed dispatcher:

```yaml
repo:
  remote: "git@github.com:yourorg/yourrepo.git"
  path: "/absolute/path/to/yourrepo"
  base_branch: "main"
  require_checks: true

team_key: "TEAM"
team_id: "linear-team-uuid"

poll_interval_s: 30

caps:
  global: 2
  per_lane:
    coder: 1
    reviewer: 1
    git_operator: 1

turn_cap: 5
max_runtime_s: 3600
max_tokens_per_run: 400000
max_attempts: 3
rework_cap: 3
recover_cap: 5
recover_backoff_s: 30

lane_label_prefix: "agent:"

# Optional. Omit this field to use Linear workflow states.
state_label_prefix: "clipse:"

agent_backend:
  type: daytona
  daytona:
    auto_stop_minutes: 60
    reviewer_auto_delete_minutes: 60
    # snapshot: "your-snapshot-name"
    # target: "us"

worker:
  command:
    - uv
    - --project
    - "/absolute/path/to/clipse/agent"
    - run
    - clipse-worker

models:
  coder: "anthropic:claude-sonnet-4-6"
  coder_docs: "anthropic:claude-sonnet-4-6"
  reviewer: "anthropic:claude-opus-4-6"

shell_allow_list:
  coder: all
  coder_docs: all
  reviewer: all

board_dir: "/absolute/path/to/runtime/yourrepo"
checkpoints_dir: "/absolute/path/to/runtime/yourrepo/checkpoints"

env_allowlist:
  - ANTHROPIC_API_KEY
  - PATH
  - HOME
```

`repo.remote` must be a credential-free `github.com` HTTPS or SCP-style SSH remote. Daytona canonicalizes either accepted form to a credential-free HTTPS URL, then supplies the host GitHub token to SDK Git calls without persisting it in the sandbox.

`repo.path` is still required in Daytona mode. The Git-operator is deterministic Go running on the host: it fetches the remote feature branch into a local worktree to inspect checks, protection, mergeability, and conflicts before merging.

## Daytona snapshot and sandbox behavior

Clipse clones the repository into `/home/daytona/workspace/clipse`, but it does not install the target repository's toolchain after cloning. The selected snapshot—or the Daytona account's default image when `snapshot` is omitted—must support the repository's real build, test, lint, and code-generation commands.

Before production use, confirm the snapshot provides the required language runtimes, package managers, system libraries, `git`, and common shell tools. A named snapshot must exist in the selected target and be accessible to the API-key identity.

Lifecycle behavior is deliberate:

- Coder and coder-docs turns for one issue reuse an issue-scoped sandbox.
- Every reviewer run receives a fresh disposable sandbox.
- All sandboxes stop after `auto_stop_minutes` of inactivity.
- Reviewer sandboxes also request provider-side deletion after `reviewer_auto_delete_minutes` as a fallback.
- Terminal coder workspaces and all reviewer workspaces enter Clipse's durable cleanup queue.
- Startup lists Clipse-owned sandboxes and reconciles them against SQLite before polling Linear.
- Multiple coder sandboxes with the same Clipse ownership identity require manual reconciliation; Clipse will not guess which one to delete.
- A Daytona preflight or lifecycle failure never silently falls back to local execution.

## Linear state modes

Clipse always uses `agent:<lane>` labels as the opt-in and lane-selection mechanism. An issue without a matching lane label is invisible to the dispatcher even if it is assigned to the dispatcher operator.

Board state has two modes.

### Workflow-state mode: the default

Omit `state_label_prefix`. Clipse maps its internal columns to the configured team's Linear workflow states and writes workflow-state transitions through the outbox.

### Label-backed state mode: opt-in

Set:

```yaml
state_label_prefix: "clipse:"
```

Before startup, create these eight labels on the configured Linear team:

- `clipse:todo`
- `clipse:ready`
- `clipse:running`
- `clipse:review`
- `clipse:merging`
- `clipse:done`
- `clipse:rework`
- `clipse:blocked`

Clipse validates the complete set but does not create it. No state label means `todo`; an unknown or conflicting `clipse:` label fails safe to `blocked`. Linear workflow types `completed` and `canceled` remain terminal overrides.

When an issue reaches `done`, Clipse removes its `agent:<lane>` label so it leaves the candidate queue. To requeue a completed item safely, remove or replace `clipse:done`, reopen a completed/canceled Linear workflow into a nonterminal state when necessary, then restore `agent:<lane>` last.

The lane and state prefixes must not overlap.

## Preparing work in Linear

A dispatchable project needs:

- Issues on the configured team.
- `blocked-by` relations representing the dependency DAG.
- An `agent:coder` label on agent-owned implementation issues.
- The intended starting workflow state, or the intended `clipse:<state>` label in label-backed mode.

For a new project, describe the board in a `board.yaml`, preview it, then apply it:

```sh
LINEAR_API_KEY="..." ./bin/clipse board plan board.yaml
LINEAR_API_KEY="..." ./bin/clipse board apply board.yaml
```

The board applier is idempotent and non-destructive: it creates new refs, updates changed refs, skips unchanged refs, and reports—but does not delete—orphans.

## Running multiple independent instances

Create one YAML file and one dispatcher process per Linear workspace/team and GitHub repository pair:

```text
configs/
  product-a.yaml
  product-b.yaml

runtime/
  product-a/
  product-b/
```

At minimum, every instance needs distinct values for:

- `repo.remote`
- `repo.path`
- `team_key`
- `team_id`
- `board_dir`
- `checkpoints_dir`
- Process-scoped `LINEAR_API_KEY`

Use absolute directories. Each `board_dir` contains that instance's `clipse.db`, `clipse.lock`, logs, transcripts, workspace records, and Git-operator worktrees. Because the dispatcher locks `<board_dir>/clipse.lock`, distinct board directories permit the processes to run concurrently; sharing one causes the second process to fail its lock check.

Launch each process with its own Linear key:

```sh
LINEAR_API_KEY="$(security find-generic-password -a "$USER" -s clipse-linear-product-a -w)" \
./bin/clipse dispatch --config /absolute/path/to/configs/product-a.yaml
```

```sh
LINEAR_API_KEY="$(security find-generic-password -a "$USER" -s clipse-linear-product-b -w)" \
./bin/clipse dispatch --config /absolute/path/to/configs/product-b.yaml
```

The instances may share a Daytona account and model credential. Daytona ownership labels include the repository identity, preventing Clipse workspaces for different repositories from colliding.

### Different GitHub accounts

If one host GitHub identity can access both repositories, no extra isolation is necessary. Each config's `repo.remote` scopes host `gh` commands to the intended repository.

If concurrent instances require different GitHub accounts in Daytona mode, give each process a separate `HOME` containing its own `gh` authentication state:

```sh
HOME=/absolute/path/to/clipse-homes/product-a gh auth login --hostname github.com
HOME=/absolute/path/to/clipse-homes/product-b gh auth login --hostname github.com
```

Launch each dispatcher with the matching `HOME`. Do not rely on switching a shared `gh` profile after both dispatchers start: later worker invocations reread the shared authentication state. Also do not rely on `GH_CONFIG_DIR` alone for Daytona lifecycle isolation; the lifecycle environment intentionally forwards `HOME`, not `GH_CONFIG_DIR`.

An isolated `HOME` also isolates `openai_codex` OAuth and Deep Agents configuration. Authenticate the selected model separately in every such home.

## Launching and observing a dispatcher

For the default Anthropic models and Daytona backend:

```sh
export DAYTONA_API_KEY
export ANTHROPIC_API_KEY

LINEAR_API_KEY="$(security find-generic-password -a "$USER" -s your-clipse-linear-key -w)" \
./bin/clipse dispatch --config /absolute/path/to/clipse.yaml
```

The process must run as the user whose `HOME` contains the intended `gh` and model authentication, with a `PATH` that resolves `uv`, `gh`, and `git`.

Inspect the corresponding board explicitly:

```sh
./bin/clipse status --board /absolute/path/to/runtime/yourrepo
./bin/clipse tui --board /absolute/path/to/runtime/yourrepo
```

`status` and `tui` read a board directory rather than a config file. When several instances exist, always pass `--board` instead of accepting the `./.clipse` default.

## Startup preflights

Before it can claim work, a Daytona-backed dispatcher:

1. Loads and validates the YAML.
2. Canonicalizes the configured GitHub remote.
3. Invokes the worker's Daytona `list` lifecycle action.
4. Verifies Daytona authentication and target access.
5. Runs `gh auth status --hostname github.com` through the host worker environment.
6. Acquires the board-directory lock.
7. Creates or opens the board SQLite database and checkpoint directory.
8. Builds the Linear client from that process's `LINEAR_API_KEY`.
9. Validates the eight state labels when label-backed mode is enabled.
10. Reconciles Clipse-owned Daytona sandboxes with durable SQLite state.

A failure stops startup before new work is claimed.

## Verification sequence

Use this rollout order for a new host, repository, snapshot, or credential set:

1. Run `make build` and `make test`.
2. Confirm `gh auth status --hostname github.com` and a read-only fetch against the configured repository.
3. Confirm the selected model credential with a focused worker or eval invocation.
4. Run `make smoke-daytona-backend` from the Clipse repository.
5. Verify that the smoke closes its draft PR, deletes its branch, deletes both sandboxes, and reports zero leftovers.
6. Opt one low-risk issue into the target board with `agent:coder`.
7. Keep `coder`, `reviewer`, and `git_operator` concurrency at one until that issue reaches `done` and cleanup completes.
8. Inspect `status` or `tui`, then increase concurrency deliberately.

The Daytona smoke opens a real draft PR and creates live sandboxes, but it never merges. It derives its GitHub repository from the Clipse checkout, so it proves the integration path rather than the target repository's snapshot compatibility. The first low-risk target issue remains the final production-path verification.

The standalone smoke accepts these optional overrides:

- `CLIPSE_DAYTONA_SNAPSHOT`
- `CLIPSE_DAYTONA_TARGET`
- `CLIPSE_SMOKE_CODER_MODEL`
- `CLIPSE_SMOKE_REVIEWER_MODEL`

To load a repo-local smoke environment explicitly:

```sh
uv run --env-file .env --project agent python scripts/smoke_daytona_backend.py
```

## Common failures

### `LINEAR_API_KEY is not set`

The key was not present in the dispatcher process. Source the correct workspace credential at launch. Do not put it in `env_allowlist`.

### `preflighting daytona backend`

Check, in order:

1. `DAYTONA_API_KEY` is present in the dispatcher environment.
2. `DAYTONA_API_URL` and the target are correct when overridden.
3. The API key can list, create/start, and delete sandboxes.
4. A configured snapshot exists in the selected target.
5. The worker command and absolute `uv --project` path are valid.
6. `gh auth status --hostname github.com` succeeds under the dispatcher's `HOME`.

Clipse will not fall back to host execution after this failure.

### `another clipse dispatcher is already running`

The selected `board_dir` is already locked. Point the new instance at a different board directory, or stop the process that legitimately owns the current one. Never delete a live lockfile to bypass the check.

### Linear state-label preflight fails

Create all eight `<state_label_prefix><state>` labels on the configured team, and ensure `state_label_prefix` does not overlap `lane_label_prefix`. Remove the option if workflow-state mode is intended.

### Dispatcher starts but finds no work

Confirm the issue belongs to `team_key`, is nonterminal, and carries an `agent:<lane>` label. Assignee alone does not opt an issue into Clipse. In label-backed mode, also inspect reserved-state labels for conflicts or unknown values.

### Model authentication repeatedly blocks an issue

For Anthropic, verify `ANTHROPIC_API_KEY` is both present and allow-listed. For `openai_codex`, repeat `/auth` under the exact `HOME` and OS user used by the dispatcher. A missing or expired Codex OAuth credential currently follows the bounded transient-retry path before parking.

### Git-operator cannot fetch the Daytona branch

The remote sandbox may be healthy while the host primary clone is not. Verify `repo.path`, its `origin`, and host Git authentication. Daytona mode intentionally requires the authoritative remote feature branch; it does not fall back to the base branch for Git-operator worktrees.

## Security boundaries

- Linear credentials remain kernel-only.
- Daytona credentials remain controller-only.
- GitHub tokens are not written into sandbox environment variables, Git config, prompts, transcripts, or worker results.
- Daytona sandboxes receive no model credentials; model calls stay on the host.
- The default `shell_allow_list` policy is `all`, which gives issue-driven agent tools an unrestricted shell inside their selected backend. Use an explicit command list when the issue source is less trusted, accepting the restrictive mode's command-pattern limitations.
- Treat `~/.deepagents/.state/chatgpt-auth.json` as an account credential. Daytona prevents sandbox shell tools from reading that host file, but the host worker still needs the correct `HOME` to use it.

## Related documentation

- [`configs/clipse.example.yaml`](../configs/clipse.example.yaml) — annotated configuration reference.
- [`AGENTS.md`](../AGENTS.md) — contributor guide and kernel invariants.
- [`README.md`](../README.md) — quickstart and architecture overview.
- [`docs/design/2026-07-11-linear-board-bootstrap.md`](design/2026-07-11-linear-board-bootstrap.md) — declarative Linear board setup.
- [`docs/superpowers/specs/2026-07-11-daytona-agent-backend-design.md`](superpowers/specs/2026-07-11-daytona-agent-backend-design.md) — Daytona lifecycle and trust-boundary design.
- [Daytona Python SDK configuration](https://www.daytona.io/docs/en/python-sdk/)
- [Daytona API keys](https://www.daytona.io/docs/en/api-keys/)
- [Daytona snapshots](https://www.daytona.io/docs/en/snapshots/)
