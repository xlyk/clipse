# Interactive Configuration Wizard Plan

**Goal:** Add a polished `clipse configure` terminal wizard that takes an operator from an empty machine-specific configuration to a validated, reviewable `clipse.yaml`, one decision at a time. The primary path targets Daytona, supports independent Linear/GitHub instances, and makes operational blockers visible before the dispatcher can claim work.

**Decision:** Build this as a Bubble Tea TUI, not a separate GUI. The current Charm stack can provide the full-screen form, file picker, lists, progress, help, responsive layout, and interactive subprocess handoff without adding a dependency. The “old-school keygen” treatment belongs in presentation and optional audio; it must not obscure validation, delay real work, or weaken accessibility.

**Tech stack:** Go 1.25; the existing Cobra, Bubble Tea v1.3.10, Bubbles v1.0.0, Lip Gloss v1.1.0, and `yaml.v3` dependencies; standard library otherwise. Bubble Tea's alternate screen and `ExecProcess` cover full-screen rendering and temporary handoff to an interactive authentication command. Bubbles already contains the needed text input, text area, list, file picker, spinner, progress, help, and viewport components.

**Primary references:** [configuration guide](../configuration-guide.md), [example config](../../configs/clipse.example.yaml), [Bubble Tea](https://github.com/charmbracelet/bubbletea), [Bubbles](https://github.com/charmbracelet/bubbles), and Go's [`os/exec`](https://pkg.go.dev/os/exec).

---

## Product boundary

The wizard owns four outcomes:

1. Assemble every value needed by the current `config.Config` contract.
2. Explain and verify the credentials that remain process-level inputs rather than YAML fields.
3. Run read-only readiness checks against the selected repository, GitHub identity, Linear team, model provider, and Daytona target.
4. Preview and atomically write a configuration only after explicit confirmation.

The wizard does **not**:

- put `LINEAR_API_KEY`, `DAYTONA_API_KEY`, model keys, GitHub tokens, or OAuth tokens in YAML;
- silently create Linear labels, clone repositories, start a dispatcher, create Daytona sandboxes, open PRs, or run a token-spending model call;
- treat a syntactically valid YAML file as “ready” when live auth or issue discovery has not been checked;
- fall back from Daytona to local execution;
- introduce a GUI toolkit or embedded browser;
- require music, animation, color, Unicode, a mouse, or a wide terminal to finish setup.

The only v1 mutation is local: creating the chosen runtime directories and writing the selected config path. A final handoff may show commands for board bootstrap, smoke verification, and dispatch, but those commands run only after the user leaves the wizard and invokes them deliberately.

## CLI contract

```text
clipse configure
clipse configure --output /absolute/path/to/clipse.yaml
clipse configure --from /absolute/path/to/existing.yaml
clipse configure --mode quick|advanced
clipse configure --music auto|on|off
clipse configure --no-animation
clipse configure --no-color
```

- `--output` preselects the write target. The wizard still previews the document and asks before writing.
- `--from` loads an existing config as editable input. It never implies permission to overwrite it.
- `--mode quick` shows required choices and recommended presets while still rendering every resolved field on the review screen.
- `--mode advanced` exposes every field directly.
- `--music auto` is silent until the welcome screen offers the soundtrack; `on` requests it immediately and `off` disables all audio probing.
- If stdin or stdout is not a TTY, return a clear error and point to `configs/clipse.example.yaml`; do not attempt an inline pseudo-wizard.
- `Esc` moves back, `Ctrl+C` exits without writing, `?` opens contextual help, and `m` toggles the soundtrack anywhere.

Do not add a non-interactive “accept all defaults” write mode in v1. The required repository and Linear identity choices are too consequential for a blind generator.

## End-to-end user flow

The quick and advanced modes share one state machine. Advanced mode expands fields in place rather than following a separate code path.

| Step | Required decisions | Detection and validation | Advanced controls |
|---|---|---|---|
| 0. Instance | Instance name, output path, new vs import | Existing file, sibling configs, runtime-path collisions | Explicit board/checkpoint roots |
| 1. Host | Clipse checkout and worker command | `git`, `gh`, `uv`, worker `--help`, writable directories | Custom worker argv and isolated `HOME` |
| 2. Repository | Local primary clone, remote, base branch, CI policy | Git worktree, origin match, base ref, read-only fetch/access | Edit remote and `require_checks` |
| 3. Linear | Current credential source and team | Auth, list teams, confirm key/id pair | Custom lane prefix |
| 4. State model | Workflow states (default) or label-backed state | Exact workflow mapping or all eight prefixed labels | Custom non-overlapping state prefix |
| 5. Agent backend | Daytona (recommended) or local | Daytona key, target, provider `list`, GitHub host auth | Snapshot, lifecycle timers |
| 6. Models | Coder, docs, reviewer profiles | Provider auth presence and worker compatibility | Model specs, opaque params, isolated `HOME` |
| 7. Safety and limits | Unrestricted vs restrictive shell posture | Secret forwarding rules and numeric invariants | All caps, retries, timeouts, allowlists |
| 8. Review | Accept config and readiness report | Canonical parse using production config code | Full YAML/diff view |
| 9. Finish | Write or return to edit | Re-load written file, re-run final local checks | Board/smoke/dispatch handoff |

### Step 0: instance identity and multi-instance safety

Ask for a short instance name used only to derive safe suggestions. For a single default instance, suggest `configs/clipse.yaml`. For named instances, suggest `configs/clipse.<name>.local.yaml` and add that local pattern to `.gitignore` as part of implementation.

Suggest an absolute, host-local state root:

- macOS: `$HOME/Library/Application Support/clipse/<name>`
- Linux: `$XDG_STATE_HOME/clipse/<name>` or `$HOME/.local/state/clipse/<name>`

Set `board_dir` to that root and `checkpoints_dir` to `<board_dir>/checkpoints`. On import, retain explicit paths but warn if they are relative. Scan sibling config files best-effort and block an exact `board_dir` collision. A live `<board_dir>/clipse.lock` is a blocking conflict, not an overwrite prompt.

This makes “one config per Linear team and GitHub repo” the natural path. The finish screen always prints `status` and `tui` commands with that instance's explicit `--board` path.

### Steps 1–2: host and repository

Use the Bubbles file picker for the Clipse checkout and the target primary clone, with manual path input available. Detect the normal worker command from the Clipse checkout:

```yaml
worker:
  command: [uv, --project, /absolute/path/to/clipse/agent, run, clipse-worker]
```

Repository checks are read-only:

- selected path exists and is a Git worktree;
- `remote.origin.url` canonicalizes to the chosen credential-free GitHub remote;
- the base branch exists locally or at `origin/<base>`;
- `gh auth status --hostname github.com` succeeds under the selected `HOME`;
- a repository-scoped read operation succeeds;
- `git ls-remote origin HEAD` can authenticate without modifying the checkout.

Daytona still requires `repo.path`: the agent tools are remote, but deterministic Git operations remain on the host. Explain this in the step help instead of hiding the field.

### Steps 3–4: Linear and state ownership

`LINEAR_API_KEY` remains a kernel-only process credential. The wizard may read it from its own environment and use it in memory for discovery, but it never copies the value into the UI model's durable draft, YAML, logs, error text, command arguments, or clipboard. A masked, one-session paste may unblock discovery, but it cannot count as durable readiness because a child process cannot update its parent shell; the finish page must require a repeatable launch-time source such as an environment injector or existing OS credential-store lookup.

With a valid credential, query available teams and let the user choose one row. Persist both the selected `team_key` and `team_id`; never ask the operator to transcribe the UUID if discovery succeeds.

State choice must be explicit:

- **Workflow-state mode — default.** Fetch the selected team's workflow states and show how all eight Clipse columns resolve. Missing exact writable mappings are blocking.
- **Label-backed mode — opt-in.** Suggest `clipse:` and verify exactly the eight required team labels: `todo`, `ready`, `running`, `review`, `merging`, `done`, `rework`, and `blocked`. Missing, duplicate, ambiguous, or unknown reserved labels are blocking. Linear `completed` and `canceled` types remain terminal overrides.

Both modes separately show the lane opt-in contract: `agent:coder`, `agent:reviewer`, and `agent:git_operator`. A read-only candidate query reports the current opted-in issue count. Zero candidates is a warning with next-step guidance, not a config error.

The wizard must not create labels in v1. It links to the exact board-bootstrap flow and offers “recheck” after the user provisions them.

### Step 5: Daytona

Daytona is the recommended preset; local mode is labeled compatibility mode. The Daytona page covers:

- `DAYTONA_API_KEY` present in the wizard process;
- optional `DAYTONA_API_URL` and `DAYTONA_TARGET` process inputs;
- YAML `agent_backend.daytona.target`, which wins over the environment when set;
- optional snapshot name/id;
- `auto_stop_minutes` and `reviewer_auto_delete_minutes`;
- the current repository's GitHub host-auth preflight;
- the existing provider-neutral `List` action, which validates access without creating a sandbox.

Snapshot existence and toolchain completeness cannot be proven by the current read-only lifecycle protocol. Show snapshot as “configured, not live-tested” and reserve a follow-up protocol extension for a genuine read-only snapshot lookup. Do not fake a pass by creating and deleting a sandbox behind the user's back.

The final screen may recommend `make smoke-daytona-backend`, but it must label the consequences: live sandboxes, a real draft PR and branch, model spend, then cleanup. It never runs from the wizard.

### Step 6: models and authentication

Offer presets before custom strings:

- current Anthropic defaults;
- OpenAI Codex profiles;
- mixed provider/model profiles;
- custom `provider:model` values.

Keep coder, coder-docs, and reviewer identities visible as separate rows. The quick path may reuse the coder model for coder-docs but never silently reuse coder for reviewer.

Auth checks are provider-aware:

- Anthropic: required environment key is present and included in `env_allowlist`.
- OpenAI Codex: the selected `HOME` contains the DAC OAuth state. If missing, offer an interactive handoff using Bubble Tea `ExecProcess` to run `uv --project agent run dcode`; the TUI resumes and rechecks when it exits.
- Unknown providers: validate only the `provider:model` shape and mark auth as operator-verified.

No live model call runs automatically. A token-spending auth/eval check belongs in the finish instructions with a cost warning.

### Step 7: safety, limits, and full config coverage

The quick path uses resolved production defaults, but the review screen pins them explicitly so future default drift cannot silently change an unattended deployment.

The wizard must cover every current YAML field:

- `repo.remote`, `repo.path`, `repo.base_branch`, `repo.require_checks`;
- `team_key`, `team_id`, `lane_label_prefix`, `state_label_prefix`;
- `agent_backend.type` and all Daytona fields;
- `worker.command`;
- `models.coder`, `models.coder_docs`, `models.reviewer`;
- all three `model_params` maps;
- all three `shell_allow_list` policies;
- `env_allowlist`;
- `poll_interval_s`, global and per-lane caps, `turn_cap`, `max_runtime_s`, and `max_tokens_per_run`;
- `max_attempts`, `rework_cap`, `recover_cap`, and `recover_backoff_s`;
- `board_dir` and `checkpoints_dir`.

The security page must state plainly that `shell_allow_list: all` is unrestricted and is the current default. A restrictive preset is opt-in and remains subject to DAC's pattern checks. `LINEAR_API_KEY` and Daytona controller variables are never allowed in `env_allowlist`.

### Steps 8–9: review, write, and handoff

The review page has three tabs:

1. **Readiness:** blocking failures first, then warnings and passes.
2. **YAML:** the exact bytes that will be written, with secrets represented only as out-of-band status rows.
3. **Diff:** when importing, a redacted diff against the existing document.

Writing follows these rules:

- no writes before confirmation;
- create parent/runtime directories with ordinary user permissions;
- create the config in the destination directory, `fsync`, set mode `0600`, and atomically rename;
- if the target exists, require a second “backup and replace” confirmation and write a timestamped sibling backup first;
- re-load the final path through `config.Load` and fail without deleting the backup if it does not round-trip;
- cancellation or any pre-write failure leaves the filesystem unchanged.

The finish screen distinguishes three outcomes:

- **Ready:** config, auth, remote access, state mode, worker, and backend preflights passed.
- **Configured with warnings:** valid and runnable, but no candidate issue, snapshot/toolchain not live-tested, or optional model check skipped.
- **Blocked:** the YAML may be saved as a draft, but the dispatcher is not described as ready. Show the exact recheck action.

Always print copyable, instance-specific `dispatch`, `status`, and `tui` commands after leaving the alternate screen. Never interpolate a secret value; use an environment variable name or a credential-source command supplied by the operator.

---

## Visual and interaction direction

The visual language is “late-90s keygen/configurator,” not “fake hacker console”:

- near-black canvas with electric cyan, magenta, violet, and acid-green accents;
- an ASCII Clipse mark and compact “CONFIG OVERDRIVE” title;
- a step rail styled like tracker channels;
- a live resolved-config panel styled like a hex/serial readout;
- a multicolor `NEON BUS`, animated spectrum, cycling step glyphs, focus pulse,
  neon rules, and persistent `HYPERDRIVE` status chrome;
- real probe results as crisp `PASS`, `WARN`, and `BLOCK` chips;
- no fake loading pauses, fake checksums, or random serial numbers presented as meaningful data.

Illustrative wide layout:

```text
◢◤ CLIPSE // CONFIG OVERDRIVE ◥◣  04/09 HYPERDRIVE ♫174 BPM
NEON BUS ✦ ▁▆▄█▂▇▅▃▆▁▇▄▂█▅
│ 01 INSTANCE  ✓ │ [ 04 // LINEAR LINK ]                    │
│ 02 HOST      ✓ │                                           │
│ 03 REPO      ✓ │  Team   SPA — Spacelift                  │
│ 04 LINEAR    ◆ │  State  clipse: labels                   │
│ 05 BACKEND   · │                                           │
│ 06 MODELS    · │  [PASS] credential  [PASS] 8/8 labels    │
│ 07 SAFETY    · │  [WARN] 0 opted-in issues                 │
│ 08 REVIEW    · │                                           │
│ 09 FINISH    · │  ↑↓ select  enter continue  ? help       │
├────────────────┴───────────────────────────────────────────┤
│ HYPERDRIVE // SYSTEM ARMED // CONFIG CHANNEL OPEN         │
└────────────────────────────────────────────────────────────┘
```

Responsive behavior is part of correctness:

- 110+ columns: step rail, form, and preview/check panel;
- 76–109 columns: rail above a two-column form/check layout;
- below 76 columns: single-column form, abbreviated banner, no spectrum;
- below the minimum usable height: viewport scrolling, never clipped controls.

Accessibility and terminal compatibility:

- honor `NO_COLOR` and `--no-color`;
- `--no-animation` removes ticks, boot animation, spectrum motion, and pulsing focus;
- provide an ASCII-only fallback when Unicode width/rendering is unsafe;
- never encode meaning by color alone;
- keep animation at or below 10 FPS and avoid full-screen flashes;
- preserve complete keyboard operation; mouse support is optional;
- restore the terminal and stop audio on success, error, `Esc`, `Ctrl+C`, SIGINT, and SIGTERM.

## Music feasibility and design

A terminal emulator has no portable streaming-audio protocol. Music is still practical without a GUI by using an optional host audio player beside the TUI.

Current soundtrack design:

1. Generate an original, deterministic 174 BPM stereo PCM WAV in Go using only
   the standard library: gabber kick, rumble and reverse bass, side-chained
   acid and reese layers, breakbeat ghosts, snare rolls, rave stabs, glitch
   gates, shrieks, and risers across an eight-bar arrangement.
2. Write it to a private temporary file.
3. Detect a supported host player: `afplay` on macOS; `pw-play`, `paplay`, then `aplay` on Linux. Unsupported systems remain silent.
4. Start the player with `exec.CommandContext`, discard its output, and restart it at track end while music remains enabled.
5. Cancel the context, wait for the process, and delete the temporary file on every exit path.

Do not use Bubble Tea `ExecProcess` for the soundtrack: it intentionally pauses the TUI until the child exits. Use `ExecProcess` only for foreground interactive tools such as Codex OAuth. Background audio lives behind an injected `AudioController` and reports state changes back as typed Bubble Tea messages.

Audio rules:

- no raw asset with unclear licensing; the procedural loop is original and reproducible;
- silent until opt-in in `auto` mode;
- automatically silent for non-TTY, `TERM=dumb`, and SSH unless `--music=on` overrides it;
- one-key `m` toggle and a persistent visible audio indicator;
- playback failure becomes a small warning and never blocks setup;
- bounded amplitude and no attempt to override system volume;
- no terminal bell fallback.

A pure-Go audio library could improve portability later, but it would be a new dependency and needs separate approval. A GUI is not justified by audio playback.

## Architecture

Keep the reducer testable and keep I/O out of `Update`, following the existing dashboard TUI pattern.

```text
cli/configure.go
    Cobra flags, terminal guard, program construction, final plain-text handoff

cli/configureui/
    model.go       page/state machine and immutable draft updates
    update.go      pure Bubble Tea reducer
    view.go        responsive keygen-style renderer
    fields.go      Bubbles inputs/lists/file pickers and field validation
    keys.go        global/contextual key maps
    messages.go    typed results from probes, writer, auth, and audio

internal/setup/
    draft.go       answers, provenance, presets, and config resolution
    document.go    deterministic YAML node construction and redacted diff
    checks.go      readiness result model and orchestration
    host.go        filesystem/tool/repository checks
    linear.go      Linear discovery/state/candidate checks
    backend.go     Daytona and worker checks through existing seams
    models.go      provider-aware, non-spending auth checks
    write.go       atomic write/backup/round-trip logic
    audio/         procedural WAV and cancel-safe player controller

internal/config/
    parse.go       shared in-memory Parse path factored from Load
    defaults.go    one exported, copy-safe source of production defaults
```

### State and message contracts

`setup.Draft` contains configuration answers and field provenance (`default`, `detected`, `imported`, `user`). It contains no secret values. A separate ephemeral credential provider supplies the current process's auth material directly to probes.

Each asynchronous operation returns a typed message:

```go
type CheckResult struct {
    ID       string
    Severity Severity // pass, warning, blocking
    Summary  string
    Detail   string   // sanitized
    Recheck  bool
}
```

The model tracks checks by stable ID, ignores stale responses using a generation number, and never launches the same probe twice concurrently. `Update` performs no filesystem, network, subprocess, or wall-clock reads; every such action is a `tea.Cmd` backed by an injected interface.

The final readiness decision is deterministic:

- any required `blocking` result => blocked;
- no blockers plus one or more warnings => configured with warnings;
- all required checks pass => ready.

### Config source of truth

Do not duplicate defaults or validation rules in the TUI.

Refactor `internal/config.Load` into:

```go
func Defaults() Config
func Parse(data []byte, source string) (*Config, error)
func Load(path string) (*Config, error)
```

`Defaults` returns fresh slices/maps so callers cannot mutate global state. `Load` reads the file then calls `Parse`. The wizard resolves a draft, renders YAML, and calls `Parse` on the exact bytes before enabling Write. The existing validation remains authoritative.

`internal/setup/document.go` builds a `yaml.Node` rather than marshaling `config.Config` directly. This allows stable field order, comments for security-sensitive settings, correct scalar-vs-list encoding for `shell_allow_list`, and deliberate omission of empty `model_params` maps. Golden tests pin the output.

### Probe safety

- Commands receive explicit argv arrays; never build shell strings.
- Secrets are environment values or HTTP headers, never argv.
- Captured stdout/stderr is bounded and sanitized before it becomes a message.
- Linear/Daytona/provider errors use typed, redacted summaries.
- All probes have contexts and short timeouts and are cancelable when the user backs out.
- No readiness probe writes Git refs, Linear objects, GitHub objects, SQLite state, or Daytona resources.

---

## Phased TDD implementation plan

Every task lands green. Use standard-library `testing`, fake subprocess runners, `httptest` loopback servers, and injected Bubble Tea commands; no live network in `make test`.

### Task 1 — share config defaults and in-memory parsing

**Files:** modify `internal/config/config.go`; add `internal/config/parse_test.go` and `internal/config/defaults_test.go`.

- [ ] Write tests that `Parse` and `Load` return equal configs for the same bytes.
- [ ] Pin every current default, including a copy-safety test for slices/maps.
- [ ] Refactor without changing existing error wording or validation order.
- [ ] Run `go test -race ./internal/config` and the existing config suite.

### Task 2 — draft resolution and complete YAML rendering

**Files:** create `internal/setup/draft.go`, `document.go`, and tests under `internal/setup/testdata/`.

- [ ] Define required answers, advanced overrides, and provenance.
- [ ] Resolve quick presets from `config.Defaults`, never copied constants.
- [ ] Render all current fields in stable order using `yaml.Node`.
- [ ] Add golden configs for Daytona/Anthropic, Daytona/Codex, local, label mode, workflow mode, and two independent instances.
- [ ] Parse every golden result through `config.Parse` and assert semantic equality.
- [ ] Assert forbidden controller keys (`LINEAR_API_KEY` and Daytona controller variables) and every fixture secret value never appear in output; model credential variable names may appear in `env_allowlist`, but their values may not.

### Task 3 — safe file planning and atomic write

**Files:** create `internal/setup/write.go` and `write_test.go`; update `.gitignore` for `configs/clipse.*.local.yaml`.

- [ ] Separate `PlanWrite` from `ApplyWrite`; planning is read-only.
- [ ] Test new write, missing parent, existing target refusal, confirmed backup/replace, mode `0600`, round-trip failure, and cancellation.
- [ ] Use same-directory temporary files, sync, rename, and explicit cleanup.
- [ ] Add path-collision checks for imported sibling configs and a live board lock.

### Task 4 — host, worker, repository, and GitHub probes

**Files:** create `internal/setup/host.go`, `runner.go`, and tests.

- [ ] Introduce a narrow argv/env subprocess runner interface.
- [ ] Detect required tools and validate the worker command with no model call.
- [ ] Validate target clone, origin, base branch, and credential-free GitHub remote.
- [ ] Add read-only GitHub auth/repository access checks under the selected `HOME`.
- [ ] Prove in tests that a token in the environment never reaches output, error detail, or argv.

### Task 5 — Linear discovery and state readiness

**Files:** add read-only discovery/state APIs under `internal/linear/`; create `internal/setup/linear.go` and loopback tests.

- [ ] List teams for the current credential and return key/id/name without mutations.
- [ ] Expose read-only workflow-state validation for all required Clipse columns.
- [ ] Reuse `ValidateStateLabels` for label mode and preserve terminal overrides.
- [ ] Query opted-in candidates and classify zero results as a warning.
- [ ] Test auth failure, wrong team, incomplete workflow mapping, missing/duplicate labels, prefix overlap, and redaction.

### Task 6 — Daytona, model, and aggregate readiness checks

**Files:** create `internal/setup/backend.go`, `models.go`, `checks.go`, and tests.

- [ ] Reuse `backend.CommandManager.List` for Daytona access and GitHub host auth.
- [ ] Mark snapshot selection unverified rather than provisioning a sandbox.
- [ ] Add provider-aware, non-spending auth presence checks.
- [ ] Build deterministic aggregate `ready`, `warning`, and `blocked` outcomes.
- [ ] Pin check ordering so the review page remains stable.

### Task 7 — wizard reducer and navigation shell

**Files:** create `cli/configureui/model.go`, `update.go`, `messages.go`, `keys.go`, and unit tests.

- [ ] Start with failing state-transition tests for next/back/edit/recheck/cancel/write.
- [ ] Keep `Update` pure and inject every I/O command.
- [ ] Ignore stale async results after a field changes or the user leaves a page.
- [ ] Preserve edits when moving backward and recompute only dependent checks.
- [ ] Ensure `Ctrl+C` and `Esc` before confirmation cannot produce a write command.

### Task 8 — build all form pages

**Files:** add `cli/configureui/fields.go`, `pages.go`, and tests.

- [ ] Compose existing Bubbles inputs, lists, file picker, viewport, spinner, progress, and help components.
- [ ] Implement quick/advanced disclosure without separate answer models.
- [ ] Cover every config field in the matrix above.
- [ ] Add inline validation, contextual explanations, and keyboard-only focus order.
- [ ] Mask any ephemeral credential input and clear it after the probe completes; never copy it into `setup.Draft`.

### Task 9 — review, redacted diff, write, and terminal handoff

**Files:** add `cli/configureui/review.go`; modify `cli/configure.go`; add tests.

- [ ] Render readiness, exact YAML, and redacted diff tabs.
- [ ] Require explicit write and backup/replace confirmations.
- [ ] Exit alternate screen before printing final commands.
- [ ] Print commands with absolute config/board paths and secret placeholders only.
- [ ] Test ready, warning, blocked-draft, write failure, and import/replace paths.

### Task 10 — keygen visual system and responsive layouts

**Files:** add `cli/configureui/view.go`, `styles.go`, `layout.go`, and snapshot/golden tests.

- [ ] Implement palette, banner, step rail, status chips, config stream, and spectrum.
- [ ] Pin wide, medium, narrow, and minimum-height layouts after stripping ANSI.
- [ ] Add `NO_COLOR`, no-animation, ASCII-only, and light/dark behavior.
- [ ] Keep every prompt and status legible in monochrome.
- [ ] Verify no animation tick runs when disabled or on a static completion page.

### Task 11 — optional procedural soundtrack

**Files:** create `internal/setup/audio/synth.go`, `player.go`, platform detection files, and tests; wire typed messages into `cli/configureui`.

- [ ] Generate a deterministic, clipped-safe WAV and test its header, duration, RMS ceiling, and byte stability.
- [ ] Detect platform players without executing a shell.
- [ ] Test opt-in, toggle, loop restart, cancel, missing player, player failure, and temporary-file cleanup with a fake runner.
- [ ] Run the audio controller under `go test -race` and add goroutine-leak-sensitive cancellation coverage.
- [ ] Confirm music failure never changes readiness or write eligibility.

### Task 12 — Cobra wiring, docs, and end-to-end verification

**Files:** modify `cli/root.go`, add `cli/configure.go` and command tests; update `README.md` and `docs/configuration-guide.md`.

- [ ] Add `configure` to root help and document flags/key bindings.
- [ ] Add a deterministic end-to-end test using fake probes and a temporary destination.
- [ ] Run `go test -race ./cli/... ./internal/config ./internal/setup ./internal/linear ./internal/backend`.
- [ ] Run `make test`, `make lint`, `make build`, and `./bin/clipse configure --help`.
- [ ] Manually verify macOS Terminal/iTerm, a narrow terminal, `NO_COLOR`, animation off, music on/off, and interruption during a probe.
- [ ] Manually verify an SSH/Linux silent fallback and imported-config diff.
- [ ] Perform one read-only live pass for GitHub, Linear, and Daytona, then one explicit full rollout using the configuration guide's smoke sequence.

## Test and acceptance matrix

The feature is done when all of the following hold:

- A new Daytona/Anthropic config can be produced without manually editing YAML.
- A new Daytona/Codex config can hand off to interactive OAuth and resume.
- A local-backend config remains supported but is not the recommended preset.
- Workflow-state mode is visibly the default; label mode cannot pass with fewer than eight exact labels.
- Every field in `config.Config` is represented and round-trips through production parsing.
- Running the wizard twice can create two configs with separate repos, teams, credentials, board dirs, checkpoint dirs, and `HOME` guidance.
- Zero candidate issues is visible and actionable.
- No secret value appears in YAML, preview, diff, logs, error messages, argv, or final commands.
- Canceling anywhere before write produces no filesystem change.
- Existing files are never replaced without backup plus explicit confirmation.
- All probes are read-only and cancelable.
- The TUI completes at narrow width, without color, without animation, without audio, and without a mouse.
- Music is fun when supported and irrelevant when unsupported.
- `make test`, `make lint`, and `make build` pass.

## GUI reconsideration threshold

Do not build a GUI for v1. Reconsider a small local GUI or browser form only if a later requirement cannot be met cleanly through the terminal, such as OS-native secure-secret enrollment across all supported platforms, a mandatory embedded OAuth callback, or a visual repository/identity picker that becomes materially safer than terminal discovery. Music and visual polish are not such requirements.
