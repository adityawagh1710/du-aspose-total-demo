# deploy/

EKS dev-cluster deployment artifacts for `office-convert`. The Compose
workflow under the repo root (`compose.yaml`) is the canonical local
runtime; this directory is for the **shared dev cluster** install on
`DEV05-EKS-CLUSTER` (namespace `office-convert-dev`).

For the full topology, ingress decisions, and the in-flight ALB Ingress
migration plan, read
[`aidlc-docs/operations/dev-deployment-topology.md`](../aidlc-docs/operations/dev-deployment-topology.md).

## Layout

```
deploy/
├── README.md            ← this file
├── helm/
│   └── office-convert/  ← Helm chart (API + UI + Ingress; values.yaml is the entry point)
├── scripts/
│   ├── portforward.sh       ← idempotent kubectl port-forward wrapper for API + UI
│   ├── eks-vpn-routes.sh    ← adds /32 routes for the EKS API endpoint after VPN reconnect
│   ├── route53-upsert.sh    ← UPSERTs both A-aliases at UI/API hostnames → live ALB (called from `make deploy-dev`)
│   └── route53-delete.sh    ← DELETEs both A-aliases (called from `make undeploy-dev` BEFORE `helm uninstall`)
└── logs/                ← timestamped deploy / undeploy / port-forward logs (gitignored)
```

## Prerequisites

- `kubectl` configured against `DEV05-EKS-CLUSTER` (account `537462380503`, region `eu-west-1`, profile `opus2-dev`).
- `helm` v3.16+.
- `aws` CLI with `opus2-dev` SSO credentials.
- Corp VPN active. EKS API endpoint reaches via `/32` routes pushed by `eks-vpn-routes.sh`. NLB private IPs are NOT reachable from the laptop (corp does not peer with the VPC) — for browser access use **either** the ALB Ingress (live since 2026-05-19, see [`aidlc-docs/operations/dev-deployment-topology.md`](../aidlc-docs/operations/dev-deployment-topology.md) §11) **or** `portforward.sh` as fallback.

## Common workflows

All commands run from the repo root unless noted.

### Install / update

```bash
AWS_PROFILE=opus2-dev AWS_ACCOUNT_ID=537462380503 AWS_REGION=eu-west-1 \
    IMAGE_TAG=$(git rev-parse --short HEAD) \
    make deploy-dev
```

`make deploy-dev` runs an 8-step pipeline: ECR repo create-if-missing → ECR
login → build + push API + UI images → namespace + license `Secret` →
`helm upgrade --install` → **`route53-upsert.sh`** (waits for the ALB
hostname, then UPSERTs both A-aliases) → print pod/service/ingress state +
NLB + ALB hostnames + image digests + AWS console deep-links.

Internally the target uses `helm upgrade --install` (idempotent), but the
operator convention is to **always run `make undeploy-dev` first** before
re-deploying — see [`reference_corp_vpn_constraints`](#known-constraints)
linked memory `feedback_deploy_workflow`. Logs land in
`deploy/logs/deploy-<timestamp>.log`; the rendered manifest lands in
`deploy/logs/manifest-<timestamp>.yaml`.

### Tear down

```bash
make undeploy-dev
```

4-step pipeline: **`route53-delete.sh`** (runs FIRST, while the Ingress
still holds the ALB DNS name needed for the Route 53 DELETE payload) →
`helm uninstall` → delete license `Secret` → delete namespace. ECR images
are retained by design; cleanup commands for them are printed (not run).
AWS LBC deprovisions the ALB + NLBs in ~60 s after `helm uninstall`.
Re-running `make deploy-dev` later allocates fresh ALB + NLB hostnames
(AWS doesn't recycle ELB names), and `route53-upsert.sh` repoints DNS
automatically — so the bookmarkable hostnames in
[`aidlc-docs/operations/dev-deployment-topology.md`](../aidlc-docs/operations/dev-deployment-topology.md)
§11 survive any undeploy/redeploy cycle.

### Browser access (port-forward)

```bash
./deploy/scripts/portforward.sh start
# → API on http://localhost:18080  (FastAPI + Swagger at /docs)
# → UI  on http://localhost:8501   (Next.js dashboard)

./deploy/scripts/portforward.sh status     # show PIDs + endpoint health
./deploy/scripts/portforward.sh restart
./deploy/scripts/portforward.sh stop
```

The script auto-picks free local ports (API base 18080, UI base 8501,
walks 10 consecutive ports if needed), kills its own previous instances,
re-runs `eks-vpn-routes.sh add` if kubectl can't reach the cluster, and
health-probes both endpoints. Logs land in
`deploy/logs/portforward-{api,ui}.log`. Tracked state lives in
`/tmp/officeconvert-portforward-{api,ui}.{pid,port}`.

### VPN route maintenance

```bash
./deploy/scripts/eks-vpn-routes.sh add      # restore /32 routes after VPN reconnect
./deploy/scripts/eks-vpn-routes.sh remove   # roll back
```

`portforward.sh start` calls this automatically when `kubectl` can't reach
the cluster, so direct invocation is only needed for ad-hoc kubectl
sessions outside the port-forward flow.

## What's in the chart

`helm/office-convert/` ships:

- **API Deployment** — FastAPI orchestrator (`uvicorn office_convert.server:app`), `replicaCount: 1`, `max_jobs: 1`, `parallel: 2`. Resources: `1–4 CPU`, `2–4 GiB memory`. **No swap on K8s** (vs. compose's 6 GiB cushion); big PPTX/XLSX inputs > ~250 MB will OOM the pod.
- **UI Deployment** — Next.js dashboard (`ui/`, container port 3000, Service port 8501). Talks to the API via in-cluster DNS.
- **Two LoadBalancer Services** — both internal NLBs. Currently coexist alongside the ALB Ingress (commit A "alongside NLBs"); local commit `33ba4c6` flips them to `ClusterIP` to drop the dormant NLBs.
- **Two Ingresses sharing one ALB** (`templates/ingress.yaml`) — internet-facing, corp-CIDR allowlisted, wildcard ACM TLS terminated at the ALB. `group.name: office-convert` merges UI + API Ingresses onto the same ALB; subdomain routing by Host header. See [`aidlc-docs/operations/dev-deployment-topology.md`](../aidlc-docs/operations/dev-deployment-topology.md) §6/§11 for the as-built spec and live state.
- **ConfigMap** — `OFFICE_CONVERT_*` env vars; consumed by Pydantic's `env_prefix` in `office_convert/config.py`.
- **License via existing Secret** — `aspose-license` Secret must exist before `helm install`. Not chart-managed; created automatically by `make deploy-dev` step [5/8] from `$(LICENSE_FILE)`, or manually with:
  ```bash
  kubectl create secret generic aspose-license \
      --from-file=license.lic=./Aspose.TotalforC++.lic \
      --namespace office-convert-dev
  ```

## Known constraints

- **NLB reachability** (unchanged from 2026-05-18): corp FortiClient VPN gives `/32` routes for the EKS API endpoint (kubectl works) but does NOT tunnel VPC private CIDRs. The internal NLB private IPs in `10.35.0.0/16` are unreachable from the laptop, and laptop-side static routes don't help (corp HQ has no server-side path to the VPC). The ALB Ingress (live since 2026-05-19) is the production access path; `portforward.sh` is the fallback. The dormant internal NLBs survive only because commit B hasn't been deployed yet.
- **Browser-to-ALB reachability**: requires the operator's egress IP to be in `values.yaml` `ingress.inboundCidrs`. Aditya's split-tunnel laptop hits the ALB via local ISP (not via FortiClient tunnel), so the per-operator ISP IP must be allowlisted — either committed to the chart (for stable office IPs) or added live via `kubectl annotate` (for ephemeral home IPs). See [`aidlc-docs/operations/dev-deployment-topology.md`](../aidlc-docs/operations/dev-deployment-topology.md) §11 for the live `kubectl annotate` recipe.
- **UI runs as root**: `Dockerfile.ui` doesn't set `USER`. Explicit TODO in `values.yaml`. Pod's `securityContext` is empty (the shared `podSecurityContext.runAsNonRoot: true` would block the UI from starting otherwise).
- **No application-layer auth in v1**: Q6 = X in `aidlc-docs/aidlc-state.md`. Whatever sits in front of the LB is the only gate. Currently that's "corp CIDR allowlist + TLS" at the ALB; the dormant NLBs add an extra "VPC-internal only" layer for any caller that finds them.

## Cross-references

- Current state + open decisions: `aidlc-docs/operations/dev-deployment-topology.md`.
- Cluster topology + AWS prerequisites: `~/.claude` operator memory (`reference_eks_cluster_topology`, `reference_alb_ingress_plan`).
- Hard constraints, SKU pivots, fork-after-load history: `aidlc-docs/aidlc-state.md`.
- Session decision log: `aidlc-docs/audit.md`.
