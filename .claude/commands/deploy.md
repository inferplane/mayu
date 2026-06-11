---
description: Build and deploy inferplane following project runbooks
allowed-tools: Read, Bash(go build:*), Bash(docker build:*), Bash(docker push:*), Bash(helm upgrade:*), Bash(helm install:*), Bash(helm lint:*), Glob
---

# Deploy

Build and deploy inferplane.

## Step 1: Pre-Deploy Checks

1. Verify working tree is clean: `git status`
2. Verify current branch (warn if not main)
3. Run tests: `go test ./... -race`
4. Check for a deployment runbook: `ls docs/runbooks/deploy-*.md`

## Step 2: Build the Image

```bash
# Static binary baked into a distroless image
docker build -t inferplane:$(git describe --tags --always) .
```

## Step 3: Deploy

If a deployment runbook exists in `docs/runbooks/`, follow its steps.

Otherwise, deploy via Helm:

```bash
helm lint charts/inferplane
helm upgrade --install inferplane charts/inferplane \
  --set image.tag=$(git describe --tags --always) \
  -f my-values.yaml
```

Note: the chart references (never creates) the Secret holding upstream provider
keys and the admin token. Create it out-of-band before deploying.

## Step 4: Verify

After deployment:
- Check rollout status: `kubectl rollout status deploy/inferplane`
- Probe readiness: the admin plane `/readyz` on port 9090
- Scrape `/metrics` and confirm `inferplane_requests_total` is present

## Error Recovery

### If pre-deploy checks fail (Step 1)
```bash
git stash                         # stash uncommitted changes
git checkout main                 # switch to main
```

### If the rollout fails (Step 3)
```bash
kubectl rollout undo deploy/inferplane    # roll back to the previous ReplicaSet
kubectl logs deploy/inferplane            # inspect startup errors
```

### If readiness fails after deploy (Step 4)
- Check logs for config load errors (bad provider/model refs, missing secrets)
- Confirm the referenced Secret exists and env vars resolve
- Confirm the ConfigMap mounted at /etc/inferplane/config.json is valid
- If unrecoverable, run the rollback above
