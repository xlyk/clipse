# Clipse — GitHub App bot identity

**Status:** Follow-up · not yet implemented · **Date:** 2026-07-02 · **Owner:** Kyle

## Overview

Give Clipse its own GitHub identity — a **GitHub App** installed on the target
repo(s) — instead of borrowing the owner's personal access token (PAT). This is
an **attribution and security** change, not a review-mechanics change. The
self-review problem it might seem to solve is already solved in code (see below).

## Problem

Today every git-touching lane — **Coder**, **Reviewer**, **Git-operator** —
acts as **one** GitHub identity: the owner's personal PAT, injected into the
worker env as `GH_TOKEN`/`GITHUB_TOKEN` (both already in the default
`env_allowlist`, `internal/config/config.go`). Two consequences follow.

- **Attribution.** Every commit, PR, and PR comment lands on the owner's
  personal account. The agent's autonomous work is indistinguishable from the
  human's own, and a full PAT carries the owner's complete account scope.
- **No GitHub-native two-party review.** GitHub forbids a *formal* review
  (approve / request-changes) on a PR you authored. A single identity therefore
  cannot perform a GitHub-native review of its own PR.

## What a GitHub App does and does not fix

A GitHub App is still a **single identity**. It does **not**, on its own, enable
self-review: an App approving a PR it authored is blocked exactly as a PAT is.

Clipse already solved self-review in code, and not with an App. The Reviewer
lane does **not** call `gh pr review` (the approve / request-changes API that
GitHub blocks on your own PR). It posts a plain `gh pr comment`, and its verdict
travels through the kernel's typed JSON result (`outcome: needs_review |
changes_requested`), which is what drives the board. The review decision is a
kernel transition off a comment, not a GitHub PR-review event — so one identity
reviewing its own PR is fine (commit `ebcf5c4`).

So the App is about **attribution and security posture**, not review mechanics.

## What the App buys

- **Bot-attributed work.** Commits, PRs, and comments show as `clipse[bot]`,
  cleanly separated from the human owner.
- **Fine-grained, per-repo permissions.** Grant only what the lanes need,
  scoped to the specific repo(s) — not the owner's whole account.
- **Short-lived installation tokens.** The credential the worker holds is an
  installation access token that expires in about an hour, is revocable, and is
  auditable, rather than a long-lived PAT.
- **Higher rate limits.** App installations get their own limits, above a
  personal token's, which matters when many workers run in parallel.

This is the right posture for an autonomous agent: a scoped, revocable bot
identity instead of the owner's personal PAT.

## Setup

Exact permission names, API endpoints, and token lifetimes below are stated at
the confidence level they warrant; **verify specifics against current GitHub
docs** before implementing.

1. **Create the App.** Owner is the user account or the org that owns the target
   repo(s). A private, single-tenant App is fine — no OAuth callback or webhook
   is required for Clipse's polling model.
2. **Grant repository permissions** (least privilege for the lanes):
   - **Contents:** read & write — clone, branch, commit, push.
   - **Pull requests:** read & write — open PRs, post `gh pr comment`, merge.
   - **Checks:** read — the Git-operator reads CI status to gate the merge.
   - **Metadata:** read — mandatory baseline for any App.

   No org-admin, no members, no secrets scopes. If a lane later needs status or
   commit-status writes, add that permission then, not now.
3. **Install on the target repo(s).** Install the App on `xlyk/clipse` (and any
   future managed repo), selecting individual repositories rather than "all".
   Installation yields an **installation id**, needed to mint tokens.
4. **Generate a private key.** In the App settings, generate a private key
   (PEM). The dispatcher signs a JWT with it. Store it like any other
   kernel-only secret (`op` / a file path in config) — never in the worker env.
5. **Mint an installation access token.** The canonical flow: sign a short-lived
   JWT with the App private key (issuer = App id), then exchange it at the
   installation-token endpoint (`POST
   /app/installations/{installation_id}/access_tokens`) for a token valid ~1
   hour. Wrap this in a helper — a small JWT-signing routine, `gh api` once you
   hold a JWT, or (in CI) an action such as `actions/create-github-app-token`.
   *There is no first-class one-shot `gh` command to mint an installation token
   from a private key — verify the current tooling and endpoint.*
6. **Attribute commits to the bot.** Two independent settings:
   - The **installation token** is the git credential for the HTTPS push, which
     authorizes the write as the App installation.
   - The **git author/committer identity** determines whose name appears on the
     commit. Set `user.name = "clipse[bot]"` and `user.email` to the bot's
     GitHub noreply address so the commit links to the bot account with its
     badge. The bot noreply email has the form
     `<bot-user-id>+clipse[bot]@users.noreply.github.com`, where the numeric id
     comes from the App's associated bot user (looked up via the API).
     *Verify the exact email format and how to resolve the bot user id against
     current GitHub docs.*

## How Clipse would consume it

The App fits the existing **kernel-only credential model**, the same shape as
`LINEAR_API_KEY`: the dispatcher holds the sensitive material and the worker
never sees it.

- The **dispatcher** holds the App **private key** (or a pre-minted installation
  token) and mints/refreshes a short-lived installation token as needed.
- It injects that token into the worker env as **`GH_TOKEN`** — already an
  allow-listed key, so no `env_allowlist` change is required. The private key
  stays in the kernel; only the short-lived token crosses into the worker,
  matching the threat model's rule that a worker carries only the secrets its
  lane needs.
- Git **author/committer** for worker commits is set to the `clipse[bot]`
  identity so PRs and commits attribute to the bot.

## Code changes required (future task)

This doc is the design; the implementation is a follow-up. Roughly:

- **Config:** a field for the App id + private-key location (or a pre-minted
  installation token) and the bot's git author/committer name+email.
- **Token minting + refresh:** the dispatcher signs the JWT, exchanges it for an
  installation token, caches it, and refreshes before the ~1-hour expiry. On
  failure, fall back to the current PAT path or surface a clear config error.
- **Git identity:** set `user.name`/`user.email` for worker commits (worktree
  git config or `GIT_AUTHOR_*`/`GIT_COMMITTER_*` in the worker env) to the bot.

Until this lands, Clipse keeps using the owner's PAT via `GH_TOKEN`.

## Two-identity note: enforced GitHub-native review

If Clipse ever wants **GitHub-native enforced review** — a formal
approve/request-changes event in the PR timeline, or a branch-protection rule
requiring N approvals — that needs **author ≠ reviewer**, i.e. a *second*
identity (a second App or a distinct account acting as reviewer).

Clipse's current design does not require it. The board is driven by the kernel's
typed verdict, not by a GitHub review event, and the authoritative merge gate is
**CI + branch protection**, which gate on required status checks and an
up-to-date branch — not on approvals (design doc, "Board & state machine" and
decision J). Revisit a second identity only if enforced PR-timeline approvals
become a goal.

## Status

Follow-up, **not yet implemented**. Clipse runs on the owner's PAT today. This
doc records the decision and the how-to so the bot identity can be stood up as a
discrete task.
