# Release Skill

Automate the release process with validation checks.

## Procedure

### 1. Pre-release Checks
- Verify working tree is clean: `git status`
- Verify all tests pass: `go test ./... -race`
- Verify the static binary builds: `CGO_ENABLED=0 go build ./cmd/inferplane`

### 2. Determine Version
- Review changes since last tag: `git log $(git describe --tags --abbrev=0 2>/dev/null || echo HEAD)..HEAD --oneline`
- Apply semver rules (pre-1.0: minor = features/breaking, patch = fixes):
  - MAJOR: Breaking API changes (post-1.0)
  - MINOR: New features
  - PATCH: Bug fixes only

### 3. Update Changelog
- Move `[Unreleased]` items in `CHANGELOG.md` into a new version heading
- Group changes by type (Added, Changed, Fixed, Removed, Security)
- Sync both English and Korean sections; update reference links

### 4. Create Release
- Update the image tag in `charts/inferplane/values.yaml` and the README quickstart
- Create git tag: `git tag -a vX.Y.Z -m "Release vX.Y.Z"`
- Build and push the container image

### 5. Summary
- Display version bump
- List key changes
- Show next steps (push tag, publish image, update Helm chart appVersion)
