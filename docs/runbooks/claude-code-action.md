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
  - `CLAUDE_BEDROCK_MODEL` (default `us.anthropic.claude-sonnet-4-6-v1:0`) — set to
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
