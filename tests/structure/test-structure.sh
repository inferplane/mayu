#!/bin/bash
# Validates inferplane project structure integrity:
# manifests, file existence, command frontmatter, CLAUDE.md sections.

# --- Manifest / config validation ---
assert_json_valid "settings.json is valid JSON" ".claude/settings.json"
assert_json_valid ".mcp.json is valid JSON" ".mcp.json"
assert_json_valid "examples/config.json is valid JSON" "examples/config.json"

# --- File existence ---
assert_file_exists "go.mod" "go.mod"
assert_file_exists "Root CLAUDE.md" "CLAUDE.md"
assert_file_exists "docs/architecture.md" "docs/architecture.md"
assert_file_exists "docs/onboarding.md" "docs/onboarding.md"
assert_file_exists "docs/api-reference.md" "docs/api-reference.md"
assert_file_exists "docs/reference/INDEX.md" "docs/reference/INDEX.md"

# --- Module CLAUDE.md coverage (top-level Go source dirs) ---
for dir in cmd internal providers pkg; do
    assert_file_exists "$dir/CLAUDE.md exists" "$dir/CLAUDE.md"
done

# --- Reference layer docs ---
for layer in infrastructure api data security agent-llm; do
    assert_file_exists "docs/reference/$layer.md exists" "docs/reference/$layer.md"
done

# --- Script validation ---
assert_file_executable "setup.sh is executable" "scripts/setup.sh"
assert_bash_syntax "setup.sh valid bash" "scripts/setup.sh"

# --- Command frontmatter ---
for cmd in review test-all deploy; do
    CMD_CONTENT=$(cat ".claude/commands/$cmd.md")
    assert_contains "Command $cmd: has frontmatter" "$CMD_CONTENT" "description:"
    assert_contains "Command $cmd: has allowed-tools" "$CMD_CONTENT" "allowed-tools:"
done

# --- Agent definitions ---
for agent in code-reviewer security-auditor; do
    assert_file_exists "agent $agent.yml exists" ".claude/agents/$agent.yml"
done

# --- Root CLAUDE.md required sections ---
SECTIONS=("Overview" "Tech Stack" "Project Structure" "Conventions" "Key Commands" "Auto-Sync Rules")
for section in "${SECTIONS[@]}"; do
    grep -qF "## $section" CLAUDE.md && pass "CLAUDE.md: has $section" || fail "CLAUDE.md: has $section" "not found"
done

# --- No commit-msg Co-Authored-By stripper (project keeps DCO + Co-Authored-By) ---
if [ -f ".git/hooks/commit-msg" ] && grep -qi "co-authored-by" ".git/hooks/commit-msg" 2>/dev/null; then
    fail "no Co-Authored-By stripper hook" "found .git/hooks/commit-msg that strips Co-Authored-By"
else
    pass "no Co-Authored-By stripper hook"
fi
