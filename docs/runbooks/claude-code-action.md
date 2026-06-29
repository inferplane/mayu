# Runbook: Claude Code GitHub Action

Registers Claude as an automated reviewer (`@claude` responder + automatic PR
review) via [anthropics/claude-code-action](https://github.com/anthropics/claude-code-action).

## What this provides

- **`.github/workflows/claude.yml`** — responds when someone writes `@claude` in
  an issue, PR comment, PR review, or new issue.
- **`.github/workflows/claude-code-review.yml`** — reviews every PR automatically
  (on open + new commits), grounded in the CLAUDE.md invariants.

## Prerequisites (must be done by a repo admin — code alone is not enough)

1. **Install the Claude GitHub App** on the repository:
   https://github.com/apps/claude → Configure → select `inferplane/mayu`.
   (Or run `/install-github-app` inside Claude Code, which guides this.)

2. **Add the API credential as a repo secret** (Settings → Secrets and variables
   → Actions → New repository secret):
   - `ANTHROPIC_API_KEY` — an Anthropic API key, **or**
   - `CLAUDE_CODE_OAUTH_TOKEN` for Pro/Max (`claude setup-token`), then swap the
     workflow input `anthropic_api_key:` → `claude_code_oauth_token:`.

   For an AWS-native deployment, use Bedrock via Workload Identity Federation
   (`anthropic_federation_rule_id` / `anthropic_organization_id` /
   `anthropic_service_account_id`, with `id-token: write`) instead of a static key.

3. **Merge these workflows to the default branch (`main`)** — `pull_request` and
   comment triggers run from the workflow definition on `main`, so the action only
   takes effect once merged.

## Verify

- Open a test PR → the `Claude Code Review` workflow runs and posts a review comment.
- Comment `@claude summarize this PR` → the `Claude Code` workflow responds.
- If a run fails with an auth error, the secret is missing/incorrect (step 2).
- If nothing triggers, the GitHub App is not installed (step 1) or the workflow
  is not yet on `main` (step 3).

## Notes

- Workflows never touch the data plane; they run in GitHub-hosted CI only.
- The review workflow is read-only except for posting the PR comment
  (`--allowed-tools` is scoped to `gh pr view/diff/comment`, `gh search`, `gh issue view`).
- Cost scales with PR volume; narrow the `claude-code-review.yml` trigger (e.g.
  add `paths:` filters) if needed.
