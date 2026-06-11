# Sync Docs Skill

Synchronize project documentation with current code state.

## Actions

### 1. Quality Assessment
Score each CLAUDE.md file (0-100) across:
- Commands/workflows (20 pts)
- Architecture clarity (20 pts)
- Non-obvious patterns (15 pts)
- Conciseness (15 pts)
- Currency (15 pts)
- Actionability (15 pts)

Apply anti-pattern deductions:
- Over 500 lines (-15)
- Vague instructions (-10)
- Duplicated docs (-10)
- No test guidance (-10)
- Contains secrets (-20)

Output quality report with grades (A-F) before making changes.

### 2. Root CLAUDE.md Sync
- Update Overview, Tech Stack, Conventions, Key Commands
- Verify commands are copy-paste ready against the actual Makefile/go commands

### 3. Architecture Doc Sync
- Update docs/architecture.md to reflect current system structure
- Add new components, update data flows, reflect infrastructure changes

### 4. Module CLAUDE.md Audit
- Scan cmd/, internal/, providers/, pkg/
- Create or update CLAUDE.md for each top-level source dir
- Score each module CLAUDE.md

### 5. Reference Doc Audit
- Verify docs/reference/*.md code pointers still resolve to real paths
- Update component tables when packages are added or moved

### 6. ADR and Runbook Audit
- Check recent commits for undocumented architectural decisions
- Verify runbook coverage; flag stale ADRs and outdated runbooks

### 7. README + CHANGELOG Sync
- Update project structure section to match actual directory layout
- Keep both language sections identical

### 8. Report
Output before/after quality scores, anti-patterns detected, and list of all changes.
