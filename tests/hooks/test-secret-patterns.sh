#!/bin/bash
# Tests for the secret-scan.sh detection patterns.
# Sensitive-looking tokens are constructed at runtime to avoid Push Protection.

# --- True positives — patterns that MUST match ---
assert_grep_match "TP: AWS Access Key ID" 'AKIA[0-9A-Z]{16}' "AKIAIOSFODNN7EXAMPLE"

ANT_PREFIX="sk-ant-"
ANT_BODY=$(printf 'a%.0s' {1..95})
assert_grep_match "TP: Anthropic API Key" 'sk-ant-[A-Za-z0-9-]{90,}' "${ANT_PREFIX}${ANT_BODY}"

SLACK_PREFIX="xoxb-"
SLACK_BODY="123456789012-1234567890123-abcdef"
assert_grep_match "TP: Slack Bot Token" 'xoxb-[0-9]+-[A-Za-z0-9]+' "${SLACK_PREFIX}${SLACK_BODY}"

GH_PREFIX="ghp_"
GH_BODY=$(printf 'b%.0s' {1..36})
assert_grep_match "TP: GitHub PAT" 'ghp_[A-Za-z0-9]{36}' "${GH_PREFIX}${GH_BODY}"

assert_grep_match "TP: inline api_key assignment" 'api[_-]?key\s*[:=]\s*["\x27][^"\x27]{8,}' 'api_key = "supersecret123"'

# --- False positives — patterns that must NOT match ---
assert_grep_no_match "FP: normal base64" 'AKIA[0-9A-Z]{16}' "dGhpcyBpcyBhIHRlc3Q="
assert_grep_no_match "FP: empty password" 'password\s*[:=]\s*["\x27][^"\x27]{8,}' 'password = ""'
assert_grep_no_match "FP: env-ref api_key (no inline value)" 'api[_-]?key\s*[:=]\s*["\x27][^"\x27]{8,}' '"api_key_ref": { "env": "ANTHROPIC_API_KEY" }'
