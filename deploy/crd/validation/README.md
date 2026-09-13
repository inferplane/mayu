# Native CRD validation

From the repository root:

```bash
go -C deploy/crd/validation run -mod=readonly . ../inferplane.dev_governancepolicies.yaml
```

This standalone module pins `k8s.io/apiextensions-apiserver v0.34.0`
(Kubernetes 1.34). It uses the native CRD schema and CEL compilation/cost
validator, including the aggregate cost limit. A failed check exits nonzero.
The first run may download module dependencies; validation itself runs locally
without a cluster, network calls, or credentials.

The main module's ordinary policy tests check collection bounds against runtime
conversion. Run this native check as well after any CRD validation change.
Its Kubernetes dependencies are isolated from the gateway/control-plane builds.
