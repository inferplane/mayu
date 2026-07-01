# Runbook: Claude Code GitHub Action (Amazon Bedrock)

Registers Claude as an automated reviewer (`@claude` responder + automatic PR
review) via [anthropics/claude-code-action](https://github.com/anthropics/claude-code-action),
running on **Amazon Bedrock** through **GitHub OIDC** — no Anthropic API key, no
long-lived AWS keys.

## What this provides

- **`.github/workflows/claude.yml`** — replies to `@claude` mentions in issues,
  PR comments, PR reviews, and new issues.
- **`.github/workflows/claude-code-review.yml`** — reviews every PR (open + new
  commits), grounded in the CLAUDE.md invariants.

## Why GitHub-hosted runners (not self-hosted)

This repository is **public**. Self-hosted runners on a public repo are a known
RCE / credential-theft risk: a fork PR can run arbitrary code on your runner and
steal its AWS instance-role credentials (incl. `bedrock:InvokeModel`). GitHub
itself warns against this. Hosted runners + OIDC use **short-lived, scoped**
credentials and no standing infra; fork PRs do not receive an OIDC token, so they
are safely skipped. Only consider self-hosted if the repo becomes private OR you
need VPC-private Bedrock (PrivateLink) — and even then gate fork PRs + use
ephemeral runners.

## Prerequisites (repo admin — code alone won't activate it)

### 1. AWS: GitHub OIDC provider (once per AWS account)
If not already present, add GitHub as an OIDC identity provider:
- Provider URL: `https://token.actions.githubusercontent.com`
- Audience: `sts.amazonaws.com`

### 2. AWS: IAM role the workflow assumes
Create a role (its ARN goes in the `AWS_ROLE_TO_ASSUME` secret).

**Trust policy** — scope it to THIS repo so only its workflows can assume it:
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Federated": "arn:aws:iam::<ACCOUNT_ID>:oidc-provider/token.actions.githubusercontent.com" },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": { "token.actions.githubusercontent.com:aud": "sts.amazonaws.com" },
      "StringLike": { "token.actions.githubusercontent.com:sub": "repo:inferplane/mayu:*" }
    }
  }]
}
```

**Permission policy** — Bedrock invoke only (least privilege):
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
    "Resource": "arn:aws:bedrock:*::foundation-model/anthropic.claude-*"
  }]
}
```
(Cross-region inference profiles like `us.anthropic.*` invoke the regional
foundation models, so the `foundation-model/anthropic.claude-*` resource covers
them. Tighten the region/model if desired.)

### 3. AWS: enable Bedrock model access
In the Bedrock console (in your `AWS_REGION`), enable access to the Claude model
you'll use (and the regions the cross-region profile spans).

### 4. GitHub: repo secret + (optional) variables
- **Secret** `AWS_ROLE_TO_ASSUME` = the role ARN from step 2.
- **Variables** (optional overrides; the workflows have safe defaults):
  - `AWS_REGION` (default `us-west-2`)
  - `CLAUDE_BEDROCK_MODEL` (default `us.anthropic.claude-sonnet-4-6`) — set to
    a model id whose access you enabled in step 3 / matches your region.

### 5. Merge these workflows to `main`
`pull_request` and comment triggers run from the workflow definition on the
default branch — they only take effect once merged.

## Verify

- Open a test PR → `Claude Code Review` runs, assumes the role, calls Bedrock, and
  posts a review comment.
- Comment `@claude summarize this PR` → `Claude Code` responds.
- Failure modes:
  - `Could not assume role` → trust policy `sub`/`aud` or `AWS_ROLE_TO_ASSUME` wrong.
  - `AccessDeniedException` from Bedrock → model access not enabled (step 3) or the
    permission policy / region is too narrow.
  - Fork PR shows no review → expected (no OIDC token for forks); safe by design.

## Notes

- GitHub token: the workflows use the built-in `${{ github.token }}` with a scoped
  `permissions:` block (pull-requests: write to post the comment). No Claude GitHub
  App or custom app is required for the Bedrock path. Upgrade to a custom GitHub
  App (`actions/create-github-app-token`) only if you want Claude's pushes to
  re-trigger CI.
- Cost scales with PR volume; add `paths:` filters to `claude-code-review.yml` to
  narrow it. The `@claude` job is gated on an actual mention.

## Hardening applied (follow-up to the auto-review of PR #3)

The self-review flagged several items; resolved here:
- **`@claude` abuse gate (HIGH):** `claude.yml` now requires
  `author_association ∈ {OWNER, MEMBER, COLLABORATOR}` in addition to the `@claude`
  mention — otherwise any GitHub user could trigger a Bedrock call on this public repo.
- **SHA-pinned actions (MEDIUM):** `actions/checkout`,
  `aws-actions/configure-aws-credentials`, and `anthropics/claude-code-action` are pinned
  to commit SHAs (resolved from their `v4`/`v1` tags via the GitHub API), not mutable tags.
  Re-pin on upgrade: `gh api repos/<owner>/<action>/commits/<tag> --jq .sha`.
- **Single-line `claude_args` (MEDIUM):** avoids any ambiguity in how the action tokenizes
  a multi-line block, so `--allowed-tools` is never silently dropped. (Verified: the CLI
  accepts both `--allowed-tools` and `--allowedTools`.)
- **Least privilege (LOW):** the review workflow dropped `actions: read`; `claude.yml`
  `issues:` trigger is `[opened]` only (no re-trigger on assignment).
- **Trust `sub` is `repo:inferplane/mayu:*` (LOW) — kept intentionally:** the PR-review
  workflow runs on `pull_request` events whose OIDC `sub` is `repo:inferplane/mayu:pull_request`,
  so tightening the trust to `…:ref:refs/heads/main` only would BREAK PR review. Keep `:*`
  (or explicitly allow both `main` and `pull_request`).
- Deferred/INFO: prompt-injection via diff content is mitigated by the read+comment-only
  `--allowed-tools` scope (no code change needed).

## PR #4 auto-review follow-up (verified in-repo, no workflow changes)

Second review pass on the hardening PR found **no HIGH** and marked it **"Safe to
merge."** Two MEDIUMs were investigated:

- **`claude_args` quoting** — verified against the pinned action source
  (`anthropics/claude-code-action@fad22eb3`): `src/modes/agent/parse-tools.ts` tokenizes
  `claude_args` with the `shell-quote` npm package (real shell-quoting rules), so
  `--allowed-tools "Bash(gh pr view:*),Bash(gh pr diff:*),..."` parses as a single
  quoted value, not word-split on the commas/parens inside it. **Refuted** — the
  quoting is correct as written; no change needed.
- **SHA pins with no re-pin automation** — resolved: `.github/dependabot.yml`
  (`github-actions` ecosystem, weekly) opens a version-bump PR whenever
  `actions/checkout`, `aws-actions/configure-aws-credentials`, or
  `anthropics/claude-code-action` cut a release; each such PR gets the same
  Bedrock auto-review as any other change before merge.

LOW findings resolved in a follow-up cleanup: `actions: read` removed from `claude.yml`
(no allowed tool exercised it); the fork-guard comment in `claude-code-review.yml`
restores the OIDC-token security rationale alongside the red-check rationale.

Still open (tracked, not yet actioned): SHA re-pin automation via Dependabot.
