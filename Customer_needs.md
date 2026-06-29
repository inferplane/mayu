# LLM Gateway: 고객 불만, 필요성, 필수 항목 분석

> **작성일**: 2026-06-26 | **소스**: Slack 내부 대화, 웹 리서치, Knowledge Graph

---

## 1. 고객이 겪고 있는 불만 (Pain Points)

### 🔴 3P LLM Gateway (LiteLLM 등) 관련 불만

| 불만 사항 | 상세 내용 | 출처 |
|-----------|-----------|------|
| **프롬프트 캐싱 버그** | LiteLLM이 모델 ID를 잘못 인식하여 프롬프트 캐싱이 깨짐 → 비용이 예상치 못하게 급증. Sonnet 호출 시 Opus가 응답하는 버그 존재 | #claude-interest-korea (Woo Hyung Choi, 6/25) |
| **비용 급증 (비가시적)** | 캐싱 버그를 모르고 사용하면 비용이 계속 올라가는데 고객은 원인 파악 불가 | #claude-interest-korea (Woo Hyung Choi, 6/25) |
| **로깅 누락** | 기본 설정으로는 프롬프트 로깅이 안 됨. 별도 Config 설정 필요 (`store_prompts_in_spend_logs: true`) | #claude-interest-korea (Jinsung Huh, 6/11) |
| **가드레일 우회** | LiteLLM을 통하면 Bedrock Guardrails 등 보안 정책이 우회될 수 있는 취약점 | #claude-interest-korea (Jinsung Huh, 6/25) |
| **권한 제어 우회** | RBAC 등 접근 제어 기능이 불완전하여 보안 정책 적용에 한계 | #claude-interest-korea (Jinsung Huh, 6/25) |
| **지원 인력 부족** | LiteLLM(Berri AI): CEO가 PR리뷰, 커스터머 엔지니어가 **전 세계 2명**. 고객이 보고 "잘 안되겠다" 판단 | #claude-interest-korea (Woo Hyung Choi, 6/25) |
| **영업/계약 응답 없음** | 일본 SCSK 고객: LiteLLM Private Offer 요청 후 **10일간 무응답** (mik@berri.ai, varoon@berri.ai, sales@berri.ai 모두 미응답) → 7/1 런칭 계획 차질 | #jp-aws-marketplace-general (Yutaka Iwasaki, 6/24) |
| **C-Level 압박** | 한 고객사는 "이번 주 내로 LiteLLM 문제 해결 안 되면 통째로 빼버리겠다" — 부사장이 직접 호출, CEO가 직접 챙기기 시작 | #claude-code-workshop-tf (Myeongsu Jeon, 6/24) |
| **Bedrock 네이티브 기능 부재** | "Bedrock에 네이티브하게 LiteLLM 기능들이 없는 게 제일 큰 문제. 이런 비완성 서비스를 추천하면 어떡하냐고 하시긴 했지만..." | #claude-code-workshop-tf (Myeongsu Jeon, 6/24) |
| **Bifrost 등 대안도 미성숙** | Bifrost(Maxim AI) 테스트 → 관리적 기능 부족 → 결국 고객들도 LiteLLM 선택 (대안 없음) | #claude-interest-korea (Jinsung Huh, 6/25) |

### 🟡 일반적 LLM Gateway 부재 시 고객 불만 (업계 공통)

| 카테고리 | 불만 사항 |
|----------|-----------|
| **비용 불투명** | 팀/기능/고객별 비용 분배 불가. Multi-tenant SaaS에서 per-tenant 추적 없으면 평균 **340% 비용 초과** |
| **Shadow AI** | 75%+ 임직원이 승인 없이 AI 도구 사용. 보안팀 가시성 zero |
| **Provider Lock-in** | 인증 방식, API 포맷, Rate Limit 모두 달라 Provider 변경 시 코드 전면 수정 |
| **장애 대응 불가** | Provider 장애 시 자동 Failover 없음 → 데모 중 서비스 다운 |
| **API Key 난립** | 팀별 독립적 API Key 관리 → 비용 가시성 상실, 보안 사고 위험 |
| **중복 호출 비용** | 동일 프롬프트 반복 호출 → 캐싱 없이 매번 과금 |
| **관찰성(Observability) 부재** | 어떤 데이터가 어디로 흐르는지 모름 |
| **Rate Limit 충돌** | Provider TPM/RPM vs 내부 per-tenant 쿼터 → 이중 Throttling 문제 |

---

## 2. LLM Gateway가 왜 필요한가 (Why Needed)

### 한국 고객 특수 요구사항

1. **멀티 AI 코딩 도구 통합**: "개발자가 원하는 툴(Claude Code or Codex) 사용하게 하되, **비용 및 로깅만 중앙에서 통합**"하고 싶다 — 대부분의 Enterprise 고객 공통 니즈 (#claude-interest-korea, Jinsung Huh, 6/11)

2. **NCT(국가핵심기술) 컴플라이언스**: 한국에서 NCT를 다루는 고객(주로 제조업)은 모든 AI 추론을 **한국 안에서만** 처리해야 함. Region-locked Gateway 필수 (#claude-interest-korea, Byong-Wu Chong, 6/15)

3. **1P(Anthropic Direct) + Bedrock 혼합 환경의 보안**: Anthropic 1P로 Claude Code 사용 시에도 가드레일·프롬프트 로깅을 위해 앞단에 LLM Gateway 배치 (#claude-interest-korea, Jinsung Huh, 6/25)

4. **"Service Mesh" 패러다임의 재현**: 마이크로서비스 시대에 Service Mesh가 해결한 공통 관심사(auth, retries, observability, policy)를 LLM 시대에는 LLM Gateway가 담당 — **한 번 해결 vs 모든 팀이 재발견**

### 업계 공통 필요성

| 필요성 | 설명 |
|--------|------|
| **비용 제어** | 팀/프로젝트/기능별 토큰 사용량 추적 및 Budget Alert |
| **보안/컴플라이언스** | PII 필터링, 프롬프트 감사, 데이터 유출 방지 |
| **운영 안정성** | 자동 Failover, Rate Limit, Retry/Backoff |
| **멀티 Provider 전략** | Lock-in 방지, A/B 테스트, 최적 모델 라우팅 |
| **개발자 생산성** | 단일 API, 모델 변경 시 코드 수정 zero |
| **거버넌스** | 누가 어떤 모델을 얼마나 쓰는지 중앙 가시성 |

---

## 3. 꼭 있어야 하는 항목 (Must-Have Features)

### 🏗️ 핵심 아키텍처

```
┌─────────────────────────────────────────────────────────┐
│  Applications (Claude Code, Codex, Custom Apps, Agents) │
└──────────────────────────┬──────────────────────────────┘
                           │
                    ┌──────▼──────┐
                    │ LLM Gateway │
                    └──────┬──────┘
                           │
        ┌──────────────────┼──────────────────┐
        ▼                  ▼                  ▼
  ┌──────────┐     ┌──────────┐     ┌──────────┐
  │ Bedrock  │     │ 1P APIs  │     │ Self-    │
  │ (Claude, │     │(Anthropic│     │ hosted   │
  │  GPT)    │     │ OpenAI)  │     │ Models   │
  └──────────┘     └──────────┘     └──────────┘
```

### ✅ 필수 기능 목록 (Tier 1 — Must Have)

| # | 기능 | 상세 | 고객 근거 |
|---|------|------|-----------|
| 1 | **통합 API (Unified API)** | Anthropic Messages API + OpenAI Chat Completions API 모두 지원. 코드 수정 없이 Provider 전환 | 모든 고객 공통 |
| 2 | **비용 추적 (Cost Attribution)** | 팀/사용자/프로젝트별 토큰 사용량·비용 실시간 대시보드 | "비용 및 로깅만 중앙에서 통합" |
| 3 | **프롬프트 로깅** | 모든 입출력 프롬프트 기록. 감사/디버깅/컴플라이언스 용도 | Jinsung, Nambong — "프롬프트 로깅도 모두 가능하겠죠?" |
| 4 | **접근 제어 (RBAC)** | 사용자/팀별 모델 접근 권한, 일일/월별 사용량 한도 | Enterprise 보안 요구사항 |
| 5 | **Rate Limiting / Quota** | Per-user, per-team 토큰 쿼터 + Provider Rate Limit 조율 | "비용 폭발" 방지 |
| 6 | **자동 Failover** | Provider 장애 시 자동 대체 모델/리전 라우팅 | 운영 안정성 |
| 7 | **가드레일 (Guardrails)** | PII 필터링, 유해 콘텐츠 차단, Bedrock Guardrails 연동 | "가드레일 우회" 버그 해결 |
| 8 | **인증/SSO** | OIDC/JWT/SAML, IAM 통합, API Key 관리 | Enterprise 보안 |
| 9 | **Region Locking** | 특정 리전으로만 트래픽 제한 (NCT, GDPR 등) | "모든 AI 추론을 한국 안에서만" |
| 10 | **Observability** | 지연 시간, 에러율, 토큰 사용량 메트릭 + CloudWatch/Datadog 연동 | 운영 가시성 |

### 🟡 높은 우선순위 (Tier 2 — Should Have)

| # | 기능 | 상세 |
|---|------|------|
| 11 | **Semantic Caching** | 유사 프롬프트 캐싱으로 비용 절감. Provider별 TTL 올바르게 적용 (LiteLLM 캐싱 버그 방지) |
| 12 | **모델 라우팅** | 요청 유형/비용/품질에 따라 적절한 모델 자동 선택 |
| 13 | **Load Balancing** | 여러 API Key/엔드포인트 간 트래픽 분산 |
| 14 | **감사 로그 (Audit Trail)** | 누가 언제 어떤 모델을 호출했는지 변조 불가능한 기록 |
| 15 | **Prompt/Response 변환** | API 포맷 자동 변환 (Anthropic ↔ OpenAI 호환) |

### 🔵 차별화 요소 (Tier 3 — Nice to Have)

| # | 기능 | 상세 |
|---|------|------|
| 16 | **A/B Testing** | 모델 간 품질 비교 실험 지원 |
| 17 | **Budget Alert** | 일정 금액 초과 시 자동 알림/차단 |
| 18 | **Self-hosted 모델 통합** | vLLM, TGI 등 자체 호스팅 모델도 동일 인터페이스 |
| 19 | **MCP/Tool Use 지원** | AgentCore WebSearch 등 MCP 도구 연동 (SigV4, PrivateLink) |
| 20 | **Multi-Cloud 지원** | AWS + Azure + GCP 동시 라우팅 가능 |

---

## 4. 경쟁 환경 (Competitive Landscape)

| 솔루션 | 특징 | 한계 |
|--------|------|------|
| **LiteLLM** (Berri AI) | 가장 많이 사용. OSS 무료, Enterprise 2-3억/년 | 지원 인력 2명, 버그 많음, 캐싱 이슈, 연락 두절 |
| **Portkey.ai** | 쿠팡 등 사용 | 관리 기능 제한적 |
| **Kong Gateway** | AI 기능 추가. 페이(Toss 계열) 도입 시작 | 무거움, LLMGW 전문 아님 |
| **Cloudflare AI Gateway** | Anthropic과 대규모 계약 | 온프레미스 불가 |
| **Bifrost (Maxim AI)** | 경량 대안 | 스타트업, 관리 기능 부족 |
| **Azure APIM GenAI** | MS 네이티브 GenAI Gateway | Azure Lock-in |
| **Google Apigee AI** | GCP 네이티브 | GCP Lock-in |
| **AWS (Bedrock)** | 아직 전용 LLM Gateway 서비스 없음 | **"AWS가 매니지드 방식의 LLM GW 하나 나오면 좋겠다"** (Jinsung, 6/25) |

---

## 5. 핵심 인사이트 & SA 시사점

### 💡 Key Takeaways

1. **"AWS가 LLM GW 시장을 선점해야 한다"** — 타 CSP(Azure APIM, Google Apigee)는 이미 제공 중. AWS만 빈 상태 (#claude-interest-korea, Jinsung Huh)

2. **"이 GW가 CSP에서 만들 이유가 없는 거 같기도 해요"** — 반대 의견도 존재. 3사가 사실상 LLMGW를 제공하므로 외부 전문 업체가 해야 한다는 관점 (Woo Hyung Choi)

3. **"Bedrock이 다 커버 못 줘주니까 급하게 앞에 프록시를 두는 방식"** — 현재는 Bedrock의 기능 공백을 3P가 메우는 구조

4. **LiteLLM 의존 리스크가 현실화** — 지원 부재, 버그, C-Level 압박까지 이어지는 상황. 대안 부재가 가장 큰 문제

5. **오픈소스 LLM Gateway 제안** (Junseok Oh) — Anthropic Messages API + OpenAI Chat Completions API 모두 지원하는 Ingress adapter 아키텍처

---

## 6. 관련 리소스

- [단일 LLM Gateway 아키텍처 블로그](https://aws.amazon.com/ko/blogs/tech/single-llm-gw-arch/) — Jinsung Huh, dohkim (6/26 게시)
- [NCT GenAI Gateway 샘플](https://github.com/aws-samples/sample-nct-genai-gateway) — Byong-Wu Chong
- [LLM Gateway (Open Source) 프로젝트](kg://entity:06c2dc8905c0) — Junseok Oh 제안
- [Hana Bank LiteLLM Deployment](kg://entity:24a5f8471754) — 하나은행 LiteLLM 도입 프로젝트
