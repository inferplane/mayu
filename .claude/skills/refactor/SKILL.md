# Refactor Skill

Refactor existing code to improve quality without changing behavior.

## Principles
- Improve structure without changing behavior
- Single Responsibility Principle (SRP)
- Remove duplicate code (DRY)
- Small, incremental steps with verification

## Process

### 1. Analysis
- Identify the target code and its tests
- Map all callers and dependencies
- Confirm test coverage exists (suggest adding tests first if not)

### 2. Plan
Present the refactoring plan to the user:
- What will change
- What will NOT change (behavior preservation)
- Risk assessment (low/medium/high)

### 3. Execute
- Make changes in small, verifiable steps
- Run `go test ./... -race` after each step
- Keep commits atomic, each DCO signed off (`git commit -s`)

### 4. Verify
- Confirm all existing tests pass
- Verify no behavior changes (gofmt + go vet clean)
- Check that the refactoring achieved its goal
