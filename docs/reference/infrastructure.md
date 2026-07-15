# Infrastructure / 인프라 구현 상세

[![English](https://img.shields.io/badge/Language-English-blue)](#english)
[![한국어](https://img.shields.io/badge/Language-한국어-red)](#korean)

<a id="english"></a>
## English

### 1. Overview
Packaging and deployment for the single static binary: a multi-stage Docker build
producing a distroless image, and a Helm chart that renders config into a ConfigMap and
wires an optional IRSA ServiceAccount for Bedrock.

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Dockerfile | `Dockerfile` | Multi-stage `CGO_ENABLED=0` build → `distroless/static:nonroot` |
| Docker ignore | `.dockerignore` | Excludes tests/docs/charts from the build context |
| Helm chart | `charts/inferplane/` | Deployment, Service (data+admin), ServiceAccount, ConfigMap, optional Ingress, optional PVC (ADR-023), NOTES.txt |
| Chart values | `charts/inferplane/values.yaml` | Image, replicaCount (1, SQLite), existingSecret, IRSA annotation, ingress (data/admin hosts), persistence (opt-in PVC for the key store) |
| Grafana dashboard | `deploy/grafana/inferplane.json` | 9-panel Prometheus dashboard |

### 3. Key Decisions
- `CGO_ENABLED=0` static binary so the image can be distroless/nonroot with no libc.
- The admin key console's static assets (`internal/server/adminui/static/`) ship inside the binary via `go:embed` — no image, chart, or build-pipeline change (ADR-001).
- **Config hot-reload (ADR-006):** edit config and `kill -HUP <pid>` (K8s: signal PID 1 or roll the pods) to apply provider/model/pricing changes with no restart — the topology is swapped atomically, governance counters/keystore/audit persist, and a bad config rolls back. Listen addrs, TLS, drain, and team policy limits are NOT hot (restart required).
- Single replica by default (SQLite key store + instance-local governance); multi-replica HA waits for the Postgres/Redis backends in v0.2.
- **Key-store persistence (ADR-023):** `persistence.enabled` (default `false`, breaking-change-free) mounts a PVC (or `existingClaim`) at `/var/lib/inferplane`; without it, that path is an `emptyDir` and the key store/audit WAL are wiped on every pod restart. Enabling it switches the Deployment to `strategy: Recreate` (an RWO volume cannot attach to two pods) and a template guard refuses to render if `replicaCount != 1`. `virtual_keys` in `config` can declare a virtual key from a secret ref (`secrets.existingSecret`) so a client's key survives a restart even without persistence — see ADR-023 for the trade-off between the two.
- The chart references an `existingSecret` and never creates secrets (design §7).
- `Ingress` is off by default (`ingress.enabled: false`); when on, the admin plane
  additionally requires `ingress.admin.enabled: true` to be routed — it carries
  key-issuance/governance actions, so exposing it is an explicit second opt-in, not
  a side effect of turning on the data-plane Ingress.
- `NOTES.txt` is the "easy deploy" surface: it prints the actual reachable
  address (Ingress host or a ready-to-paste `kubectl port-forward`), the first-key
  command, and the Claude Code env vars — so `helm install` alone gets an operator
  to working traffic without re-deriving them from `values.yaml`.

### 4. Code Pointers
- `Dockerfile` — build + runtime stages
- `charts/inferplane/templates/deployment.yaml` — pod spec, ports 8080/9090
- `charts/inferplane/templates/configmap.yaml` — rendered `config.json`
- `charts/inferplane/templates/ingress.yaml` — optional data/admin Ingress rules
- `charts/inferplane/templates/pvc.yaml` — optional PVC for the key store (ADR-023)
- `charts/inferplane/templates/NOTES.txt` — post-install quickstart

### 5. Cross-references
- Related modules: [docs/architecture.md](../architecture.md) (Infrastructure section)
- Related ADRs: docs/decisions/ (none yet)
- Related runbooks: docs/runbooks/ (create `deploy-production.md`)

<a id="korean"></a>
## 한국어

### 1. 개요
단일 정적 바이너리의 패키징·배포 계층입니다. distroless 이미지를 만드는 멀티스테이지
Docker 빌드와, config를 ConfigMap으로 렌더링하고 Bedrock용 선택 IRSA ServiceAccount를
연결하는 Helm 차트로 구성됩니다.

### 2. 구성요소
| 구성요소 | 경로 | 목적 |
|---|---|---|
| Dockerfile | `Dockerfile` | 멀티스테이지 `CGO_ENABLED=0` 빌드 → `distroless/static:nonroot` |
| Docker ignore | `.dockerignore` | 빌드 컨텍스트에서 tests/docs/charts 제외 |
| Helm 차트 | `charts/inferplane/` | Deployment, Service(data+admin), ServiceAccount, ConfigMap, 선택적 Ingress, 선택적 PVC(ADR-023), NOTES.txt |
| 차트 values | `charts/inferplane/values.yaml` | 이미지, replicaCount(1, SQLite), existingSecret, IRSA 어노테이션, ingress(data/admin 호스트), persistence(키 스토어용 옵트인 PVC) |
| Grafana 대시보드 | `deploy/grafana/inferplane.json` | 9패널 Prometheus 대시보드 |

### 3. 주요 결정
- `CGO_ENABLED=0` 정적 바이너리로 libc 없이 distroless/nonroot 이미지 구성.
- 관리 키 콘솔의 정적 자산(`internal/server/adminui/static/`)은 `go:embed`로 바이너리에 내장 — 이미지/차트/빌드 파이프라인 변경 없음(ADR-001).
- **Config hot-reload (ADR-006):** config 편집 후 `kill -HUP <pid>`(K8s: PID 1에 시그널 또는 파드 롤)로 프로바이더/모델/pricing 변경을 무중단 적용 — 토폴로지는 원자적으로 교체되고 거버넌스 카운터/키스토어/감사는 유지되며 잘못된 config는 롤백됩니다. listen 주소·TLS·drain·팀 정책 한도는 hot 아님(재시작 필요).
- 기본 단일 레플리카(SQLite 키 스토어 + 인스턴스 로컬 거버넌스); 다중 레플리카 HA는 v0.2 Postgres/Redis 백엔드 대기.
- **키 스토어 영속성(ADR-023):** `persistence.enabled`(기본 `false`, 기존 배포를 깨지 않는 opt-in)가 `/var/lib/inferplane`에 PVC(또는 `existingClaim`)를 마운트 — 켜지 않으면 이 경로는 `emptyDir`이라 파드 재시작마다 키 스토어/감사 WAL이 초기화됩니다. 활성화 시 Deployment는 `strategy: Recreate`로 전환(RWO 볼륨은 두 파드에 동시 부착 불가)되고, `replicaCount != 1`이면 템플릿 가드가 렌더링을 거부합니다. `config`의 `virtual_keys`로 시크릿 참조(`secrets.existingSecret`)에서 가상 키를 선언하면 영속성 없이도 클라이언트 키가 재시작을 견딥니다 — 두 방식의 트레이드오프는 ADR-023 참고.
- 차트는 `existingSecret`을 참조하며 시크릿을 생성하지 않음(설계 §7).
- `Ingress`는 기본 off(`ingress.enabled: false`) — 켜더라도 관리 플레인은
  `ingress.admin.enabled: true`를 별도로 설정해야 라우팅됩니다. 키 발급/거버넌스
  작업을 다루므로, 데이터 플레인 Ingress를 켠다고 부수적으로 노출되지 않도록 명시적
  두 번째 opt-in으로 분리했습니다.
- `NOTES.txt`가 "쉬운 배포"의 실제 지점입니다 — 실제로 접근 가능한 주소(Ingress
  호스트 또는 바로 붙여넣을 수 있는 `kubectl port-forward`), 첫 키 발급 명령,
  Claude Code 환경변수를 출력합니다. 즉 `helm install` 한 번으로 `values.yaml`을
  다시 해석할 필요 없이 바로 트래픽을 흘려볼 수 있습니다.

### 4. 코드 포인터
- `Dockerfile` — 빌드 + 런타임 스테이지
- `charts/inferplane/templates/deployment.yaml` — 파드 스펙, 포트 8080/9090
- `charts/inferplane/templates/configmap.yaml` — 렌더링된 `config.json`
- `charts/inferplane/templates/ingress.yaml` — 선택적 data/admin Ingress 규칙
- `charts/inferplane/templates/pvc.yaml` — 키 스토어용 선택적 PVC(ADR-023)
- `charts/inferplane/templates/NOTES.txt` — 설치 후 퀵스타트

### 5. 상호 참조
- 관련 모듈: [docs/architecture.md](../architecture.md) (인프라 섹션)
- 관련 ADR: docs/decisions/ (아직 없음)
- 관련 런북: docs/runbooks/ (`deploy-production.md` 작성)
