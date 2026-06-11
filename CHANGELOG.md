# Changelog

<a href="#english"><img src="https://img.shields.io/badge/lang-English-blue.svg" alt="English"></a>
<a href="#korean"><img src="https://img.shields.io/badge/lang-한국어-red.svg" alt="Korean"></a>

---

<a id="english"></a>

# English

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Anthropic Messages ingress (`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`) with verbatim, cache-safe body forwarding.
- OpenAI Chat Completions ingress (`/v1/chat/completions`, `/v1/models`) with canonical-schema conversion.
- Virtual keys (`ik_...`) with team RBAC and per-key allowed-model lists; SHA-256 hashed at rest, shown once.
- Two-phase governance: per-team rate limits (TPM/RPM), daily token quotas, and monthly USD budgets with `block`/`warn` policies.
- Integer-microUSD pricing with round-half-even and TTL-tiered prompt-cache rates; `on_missing: allow` for self-hosted chargeback.
- Tamper-evident audit log: per-instance SHA-256 hash chain, disk WAL (`buffer_then_block`), and the `inferplane audit verify` command.
- Providers: Anthropic direct, Amazon Bedrock (Claude via InvokeModel, others via Converse), and any OpenAI-compatible endpoint, with priority fallback and per-provider circuit breakers.
- Prometheus `/metrics` on the admin plane using OpenTelemetry GenAI semantic conventions, plus a 9-panel Grafana dashboard.
- Optional self-terminated TLS on the data plane for non-Kubernetes deployments.
- Packaging: multi-stage `CGO_ENABLED=0` static Docker image (distroless/nonroot) and a Helm chart (ConfigMap config, IRSA ServiceAccount, `existingSecret` reference).

### Security
- Config rejects inline secrets; credentials are referenced only via `env:`/`file:`/`secret:`.
- The gateway never forwards the client key upstream and never exposes its upstream keys to clients.
- `/metrics` carries no secret or `key_id`, and bounds label cardinality with a `_rejected` sentinel on pre-resolution 403/404 paths.
- `count_tokens` always returns 200 to avoid crashing Claude Code.

[Unreleased]: https://github.com/inferplane/mayu/commits/main

---

<a id="korean"></a>

# 한국어

이 프로젝트의 모든 주요 변경 사항은 이 파일에 기록됩니다.
이 문서는 [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)를 기반으로 하며,
[Semantic Versioning](https://semver.org/spec/v2.0.0.html)을 따릅니다.

## [Unreleased]

### Added
- 캐시 안전 본문 verbatim 전달을 갖춘 Anthropic Messages 인그레스(`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`) 추가.
- canonical schema 변환을 갖춘 OpenAI Chat Completions 인그레스(`/v1/chat/completions`, `/v1/models`) 추가.
- 팀 RBAC와 키별 허용 모델 목록을 갖춘 가상 키(`ik_...`) 추가; 저장 시 SHA-256 해시, 1회 표시.
- 2단계 거버넌스 추가: 팀별 rate limit(TPM/RPM), 일일 토큰 쿼터, 월별 USD 예산과 `block`/`warn` 정책.
- round-half-even과 TTL 계층 프롬프트 캐시 단가를 갖춘 정수 microUSD pricing 추가; 자체 호스팅 차지백용 `on_missing: allow`.
- 변조 감지 감사 로그 추가: 인스턴스별 SHA-256 해시 체인, 디스크 WAL(`buffer_then_block`), `inferplane audit verify` 명령.
- 공급자 추가: Anthropic 직접, Amazon Bedrock(Claude는 InvokeModel, 그 외 Converse), 모든 OpenAI 호환 엔드포인트, 우선순위 폴백과 공급자별 서킷 브레이커.
- OpenTelemetry GenAI 시맨틱 컨벤션을 사용하는 관리 플레인 Prometheus `/metrics`와 9패널 Grafana 대시보드 추가.
- 비-Kubernetes 배포를 위한 데이터 플레인 자체 종단 TLS(선택) 추가.
- 패키징: 멀티스테이지 `CGO_ENABLED=0` 정적 Docker 이미지(distroless/nonroot)와 Helm 차트(ConfigMap config, IRSA ServiceAccount, `existingSecret` 참조) 추가.

### Security
- config가 인라인 시크릿을 거부; 자격 증명은 `env:`/`file:`/`secret:`로만 참조.
- 게이트웨이는 클라이언트 키를 상위로 전달하지 않고, 상위 키를 클라이언트에 노출하지 않음.
- `/metrics`는 시크릿·`key_id`를 담지 않으며, 사전 해석 403/404 경로에서 `_rejected` 센티넬로 레이블 카디널리티 제한.
- `count_tokens`는 Claude Code 크래시 방지를 위해 항상 200 반환.

[Unreleased]: https://github.com/inferplane/mayu/commits/main
