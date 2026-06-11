---
description: Execute the full test suite and report results
allowed-tools: Read, Bash(go test:*), Bash(go vet:*), Bash(gofmt:*), Bash(bash tests/run-all.sh:*), Glob
---

# Test All

Execute the full test suite for inferplane.

## Step 1: Go Tests

Run the Go suite with the race detector, plus vet and format checks:

```bash
go test ./... -race
go vet ./...
gofmt -l .        # must print nothing
```

## Step 2: Harness Tests

Run the bash harness tests (hooks, secret patterns, project structure):

```bash
bash tests/run-all.sh
```

## Step 3: Report

Present:
- Go: packages passed/failed, failed test details with file paths and error messages
- gofmt: any files needing formatting (empty output = pass)
- Harness: total/passed/failed from the TAP-style runner
- Suggest fixes for failing tests if the cause is apparent

## Error Recovery

### If the harness runner itself fails
```bash
bash -n tests/run-all.sh          # check syntax
ls -la tests/**/*.sh              # check permissions
chmod +x tests/**/*.sh            # fix permissions
```

### Common Go failure categories

| Failure Pattern | Likely Cause | Fix |
|---|---|---|
| "data race detected" | Unsynchronized shared state | Add mutex / use atomics |
| "undefined:" | Missing import or symbol | Add import or define symbol |
| "cannot use ... as ..." | Type mismatch after refactor | Update call sites |
| build failure in one pkg | Broken interface change | Fix the implementers |

### If many tests fail at once
Likely a structural change broke multiple assumptions:
1. `git log -1` — what was the last change?
2. `git diff HEAD~1` — what specifically changed?
3. Fix the root cause, not individual tests
