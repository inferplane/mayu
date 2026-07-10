# 고객 이슈 접수 현황 분석 — LiteLLM 운영 실태와 inferplane의 해결 범위

> **소스**: `[Claude Code] 문의_이슈 접수 현황.xlsx` (2026-06 ~ 2026-07, 카카오 사내 Claude
> Code on Bedrock 채널, **59건** 실접수 티켓). LiteLLM 게이트웨이 + AWS SSO 프로필 +
> 별도 토큰 서비스(get-gateway-token.sh)로 구성된 실제 운영 환경에서 나온 문의/이슈다.
> [`Customer_needs.md`](../Customer_needs.md)(Slack 대화·웹 리서치 기반)와 상호 보완
> 관계이며, 이 문서는 **실제 접수 티켓** 기준이라 근거가 더 구체적이다.

이 문서는 59건 전수를 카테고리별로 분류하고, 각 카테고리를 inferplane이 해결하는지
(✅ 직접 해결 / 🔶 부분 해결 / ❌ 게이트웨이 무관)를 판정한다. "해결"의 기준은 실제
코드·ADR에 구현된 기능이며, 근거 없는 주장은 배제했다.

## 요약

| 판정 | 건수 | 비율 |
|------|------|------|
| ✅ 직접 해결 | 약 35건 | 59% |
| 🔶 부분 해결 (게이트웨이가 가시성/완화는 주지만 상위 원인 자체는 제거 못함) | 약 10건 | 17% |
| ❌ 게이트웨이 무관 (Anthropic 플랜 정책, 클라이언트 버그, 스코프 외) | 약 14건 | 24% |

## 카테고리별 분석

### 1. 인증 체인 붕괴 — ✅ 직접 해결 (~15건, 가장 큰 카테고리)

**티켓 근거** (row 3, 6, 7, 11, 14, 33, 40, 47, 49, 53, 54, 55 등):
- `401 Malformed API Key passed in. Ensure Key has Bearer prefix`
- `InvalidClientTokenId` / `NoCredentials: Unable to locate credentials`
- `Invalid proxy server token passed... Unable to find token in cache or LiteLLM_VerificationTokenTable` (row 54) — LiteLLM의 토큰 캐시와 실제 DB가 불일치
- 토큰 폐기/재발급을 위해 "LiteLLM UI에서 토큰 삭제 + 캐싱된 DynamoDB에서 삭제"가 필요 (row 47)
- SSO device-code 인증 실패, 8시간마다 재인증 (row 39, 49)

현재 구조는 **클라이언트 → AWS SSO → IAM Role → 별도 토큰 서비스 → LiteLLM 검증 테이블**
이라는 4단 체인이다. 체인의 어느 한 단계라도 상태가 어긋나면(SSO 세션 만료, 토큰 캐시
불일치, IAM 프로필 손상) 사용자가 원인을 특정할 수 없는 401/403이 발생한다.

inferplane은 이 체인 자체를 없앤다. `inferplane keys create`로 발급한 가상 키
(`ik_...`)는 SHA-256으로 해시되어 키 스토어에 1회 저장되고, 평문은 발급 시 한 번만
노출된다(복구 불가). 클라이언트는 이 키를 그대로 `Authorization` 헤더에 담아 계속
사용하며, AWS SSO·IAM Role·별도 토큰 서비스가 존재하지 않는다. 키 폐기는
`inferplane keys revoke --id <key_id>` 한 줄이다(`cmd/inferplane/keys.go:25`) — LiteLLM처럼
UI와 DynamoDB 두 곳을 손으로 지울 필요가 없다.

### 2. CI/헤드리스 환경 인증 불가 — ✅ 직접 해결 (row 39)

기존 CI에서는 `CLAUDE_CODE_OAUTH_TOKEN`을 주입해 인증했으나, 브라우저 기반 AWS SSO(8시간
만료)로 전환되며 CI 자동화가 막혔다. 가상 키는 브라우저 상호작용이 필요 없는 정적 문자열이므로
CI 환경변수로 그대로 주입 가능하다.

### 3. Strict 스키마 400 에러 — ✅ 직접 해결, 구조적 (row 16, 17)

**티켓**: `API Error: 400 {"detail":{"error":"diagnostics: Extra inputs are not permitted"}}`.
Claude Code가 보내는 `anthropic-beta` 헤더의 신규 베타 필드(diagnostics)를 LiteLLM이 모르는
필드로 판단해 거부한다. 티켓 원문이 스스로 진단한 근본 원인이 정확하다: *"게이트웨이가 strict인
한 클라이언트 설정만으로는 계속 두더지 잡기가 된다"*(row 16) — Claude Code가 버전업하며
필드를 추가할 때마다 게이트웨이가 매번 막힌다.

inferplane은 설계 불변식으로 이 문제 자체가 발생할 수 없다:
- **캐노니컬 스키마 불변식**(`CLAUDE.md` §Canonical schema invariant) — 파이프라인이 해석하는
  필드만 타입으로 정의하고, 나머지는 `Extra map[string]json.RawMessage`로 원문 그대로 보존한다.
- **캐시 불변식**(§Cache invariant) — 인그레스와 상위 프로토콜이 일치하면 요청 본문을
  재직렬화 없이 `RawBody`로 그대로 전달한다.

즉 클라이언트가 새 필드를 추가해도 게이트웨이가 그 필드를 알 필요가 없어 애초에 거부할 수
없는 구조다.

### 4. 모델명/라우팅 에러 — ✅ 직접 해결 (row 25, 41, 52)

**티켓**: `Invalid model name passed in model=apac.anthropic.claude-sonnet-4-6`,
`'model' keyword not found and unable to extract model from endpoint. Expected format:
/model/{modelId}/{action}. Got: v1/messages` — LiteLLM의 passthrough 라우팅 방식(URL 경로에
모델 ID를 박아넣는 방식)이 표준 Anthropic Messages API(`/v1/messages` + body의 `model` 필드)와
어긋나며 발생한 문제다. row 41은 클라이언트 자동 모델 탐색이 카카오가 등록하지 않은 `us.*`
모델 ID를 시도하다 실패하는 사례다.

inferplane은 표준 `/v1/messages`, `/v1/chat/completions` 인그레스를 그대로 노출하고
(`internal/server`), model→provider 라우팅은 설정된 모델 별칭/폴백 체인으로 내부에서 해석한다
(`internal/router`). 클라이언트가 URL 경로 규칙을 알아야 할 필요가 없다. ADR-014
(provider-registration-ux-litellm-parity)는 이 라우팅 등록 경험을 LiteLLM과 동등하게 만들면서도
동일한 구조적 이점을 유지한다.

### 5. 비용 불투명/한도 관리 — ✅ 직접 해결 (row 2, 4, 27, 37, 42, 45)

**티켓**: 사용량 조회 불가로 기존 구독 패턴 유지가 불안(row 2), 월 $100 정지 후 월 2회만
증액 가능(row 4), "한 것도 없는데 5일에 한도 소진"(row 37), 증액 자동화 이후 on/off 제어권을
개발자에게 달라는 제안(row 45).

inferplane의 거버넌스는 2단계다: `Governor.PreCheck`가 과금 **전**에 rate/quota/budget을
검사하고, `Governor.Settle`이 과금 **후**에 실제 토큰·microUSD를 차감한다(`internal/governance`).
`on_exceeded: block|warn`으로 한도 초과 시 즉시 차단할지 경고만 하고 통과시킬지 팀 단위로
선택 가능하다. `inferplane report --by team,model`(ADR-007)은 감사 로그에서 팀/모델별 정산
비용을 CSV로 뽑아내며, 비용은 float 누적 드리프트가 없는 정수 microUSD다. ADR-017(budget-alert
webhooks)은 80%/100% 같은 임계값에서 Slack/SNS 웹훅으로 사전 경고를 보내— "모르고 쓰다가
정지" 상황 자체를 없앤다.

### 6. 타임아웃/빈 응답/원인 불명 장애 — 🔶 부분 해결 (row 19, 20, 28, 43, 46)

**티켓**: `Unable to connect to API (ConnectionRefused)`, `API returned an empty or malformed
response (HTTP 200)`, `The operation timed out`가 반복 발생. 운영자 답변도 *"'추측'으로는
Fable5 모델 런칭과 관련하여 내부적으로 가용성이 부족해서 발생하는 현상으로 보여집니다"*
(row 46)처럼 확신 없는 추측에 머문다.

inferplane은 상위 Provider(Anthropic/Bedrock) 자체의 가용성 장애를 막을 수는 없다. 다만
`internal/router`의 서킷 브레이커(연속 실패 → open → 백오프 → half-open)가 **TTFT(첫 토큰)
이전에만** 자동으로 대체 모델/프로바이더로 폴백하며, GenAI 세만틱 컨벤션 메트릭
(`internal/metrics`)과 옵트인 OTel 트레이싱(ADR-011)으로 어느 요청이 어느 단계에서 실패했는지
추적할 수 있다. "추측"이 아니라 관측 데이터로 원인을 특정할 수 있게 된다는 점에서 부분 해결로
분류한다 — 상위 장애 자체를 없애는 것은 어떤 게이트웨이도 할 수 없다.

### 7. 폐쇄망(Braincloud) 접근 불가 — 🔶 부분 해결 (row 24, 30, 56)

**티켓**: LiteLLM 대시보드/게이트웨이가 카카오 사내 Private IP 대역에만 열려 있어
Braincloud(KAP) 등 별도 네트워크 존에서 접근 불가. 운영자 답변도 *"보안팀과 검토해보겠다"*로
매 건마다 반복.

inferplane은 외부 SaaS 의존 없는 단일 정적 바이너리이자 Kubernetes-native하므로, 네트워크
존마다 별도 인스턴스를 배치하는 것이 아키텍처적으로 자연스럽다(중앙 집중형 SaaS가 아니라 배포
가능한 바이너리이기 때문). 하지만 이건 배포 유연성의 문제이고, 특정 IP 대역 허용 여부 자체는
조직의 방화벽/보안 정책 결정이라 게이트웨이 소프트웨어가 대신 해줄 수 없다. 부분 해결로 분류.

### 8. 온보딩/설정 복잡도 — ✅ 직접 해결 (row 1, 5, 12, 31, 48, 59)

**티켓**: 스크립트 배포 URL 변경(row 5), AWS CLI 설치/버전 문제(row 48), 반복되는 재설정
요청(row 55, 59), MCP 연동 가이드 요청(row 31).

현재 온보딩은 AWS CLI 설치 → SSO 프로필 구성 → 로그인 → 게이트웨이 토큰 스크립트 실행이라는
다단계 흐름이며, 각 단계가 개별 실패 지점이다. inferplane 클라이언트 설정은 base URL과 가상
키 두 줄이 전부다 — AWS CLI, SSO 프로필, 리전 설정이 클라이언트에 전혀 필요 없다.

### 9. 신모델 활성화 지연 — 🔶 부분 해결 (row 44, 46)

**티켓**: 신규 모델(`claude-fable-5`) 활성화 요청, 운영자 답변은 *"향후 다시 업데이트
드리겠습니다"*로 재배포를 기다려야 하는 구조.

inferplane은 `SIGHUP` 기반 핫 리로드(ADR-006)로 재시작 없이 config를 원자적으로 교체할 수
있고, `provider_store`가 활성화된 경우 `PUT/DELETE /admin/providers|models` API(ADR-008,
ADR-014)로 콘솔에서 모델을 추가/제거하면 즉시 반영된다. 다만 모델 자체가 해당 리전/계정에서
Anthropic/Bedrock 측에 활성화되어 있어야 하므로(row 44는 "해외 사용 제한" 케이스), 게이트웨이가
상위 프로바이더의 모델 출시 정책까지 우회할 수는 없다.

### 10. 구독제 전용 기능 — ❌ 게이트웨이 무관 (row 9, 13, 32, 34, 35, 36, 38)

**티켓**: Google Drive 커넥터, Claude for Office(Excel/PowerPoint/Word), Chrome 확장 —
모두 *"Pro, Max, Team, Enterprise 플랜에서만 사용 가능"*이라는 Anthropic 공식 정책 제약이다.
Bedrock/게이트웨이 경유 여부와 무관하게 발생하며, 어떤 LLM 게이트웨이도 해결할 수 없다.
정직하게 스코프 외로 명시한다.

### 11. 기타 클라이언트/스코프 외 이슈 — ❌ 게이트웨이 무관 (row 26, 50, 57)

Auto mode 버전 문제(클라이언트 자체 버그, 이후 버전에서 해결됨), 툴콜 텍스트가 선택지 없이
그대로 노출되는 UI 버그(row 57), embedding 모델 요청(row 50 — 문의자 스스로 "claude code 제공
목적의 게이트웨이에는 적합하지 않은 요청"이라고 정정). inferplane의 v1 스코프(LLM 소비
거버넌스)와 무관.

### 12. 커스텀 스크립트/SDK 인증 실패 — ✅ 직접 해결 (row 3, 8, 15, 22)

**티켓**: *"Claude Code 앱 자체는 잘 되는데 Python 코드에서는 같은 토큰으로 연결이 안 됩니다"*
(row 8), `bedrock:ListInferenceProfiles`/`bedrock:InvokeModel` IAM 권한 혼란(row 3, 15, 22).
근본 원인은 클라이언트가 AWS IAM 정책의 존재와 세부 권한 범위를 알아야 하는 구조다.

inferplane은 표준 Anthropic/OpenAI API 형태를 그대로 노출하므로, Claude Code든 임의의 Python
스크립트든 동일한 가상 키로 동일하게 동작한다. 클라이언트는 게이트웨이 뒤에 Bedrock이 있다는
사실도, IAM 권한도 알 필요가 없다(§5.2 클라이언트/상위 키 격리 — 클라이언트는 상위 프로바이더
키를 절대 보지 못하고, 게이트웨이는 클라이언트 키를 상위로 전달하지 않는다).

## inferplane 강점 요약 (실접수 티켓 근거)

1. **인증이 실제로 가장 큰 장애 유발 지점이었다** — 59건 중 약 1/4이 SSO/토큰 체인 문제.
   가상 키는 이 4단 체인 자체를 제거한다.
2. **"Extra inputs are not permitted"는 설계상 발생 불가능** — verbatim body 전달 +
   `Extra` 보존이 LiteLLM strict 스키마 문제의 구조적 해법이다. 클라이언트 버전업 때마다
   반복되는 "두더지 잡기"를 끝낸다.
3. **거버넌스가 사전(PreCheck)·사후(Settle) 양방향** — "모르고 쓰다 정지" 대신 팀별
   `warn`/`block` 정책과 실시간 리포트, 임계값 알림(ADR-017).
4. **키 라이프사이클이 명령 하나** — LiteLLM의 "UI 삭제 + DynamoDB 삭제" 대비
   `inferplane keys revoke`.
5. **`count_tokens`는 절대 non-200을 반환하지 않는다** — 비-200이 Claude Code를 크래시시키는
   것을 방지하는 명시적 보장(`docs/reference/api.md`)이며, LiteLLM에는 이런 보장이 없다.
6. **운영자의 "추측" 답변을 관측 데이터로 대체** — 서킷 브레이커 + GenAI 메트릭 + OTel
   트레이싱으로 타임아웃/빈 응답의 원인을 실제로 추적 가능.
7. **폐쇄망 친화적 배포 모델** — 외부 SaaS 의존 0, 단일 정적 바이너리, 네트워크 존별 배치
   가능. 방화벽 정책 자체는 대신할 수 없지만, 중앙 집중형 구조의 근본 제약은 없다.
8. **Bedrock Guardrails 우회 방지(ADR-019)와 팀별 리전 락킹(ADR-020)** — `Customer_needs.md`가
   지적한 "가드레일 우회" 문제와 "NCT 컴플라이언스(한국 내 리전 고정)" 요구를 이미 구현으로
   커버한다.

## 해결하지 못하는 것 (정직한 갭)

- Anthropic 구독제 전용 기능(Desktop 커넥터, Claude for Office, Chrome 확장)은 어떤 게이트웨이도
  우회할 수 없는 플랜 정책이다.
- 상위 프로바이더(Anthropic/Bedrock) 자체의 가용성 장애는 서킷 브레이커로 완화(자동 폴백)할
  수는 있으나 원인 자체를 제거하지는 못한다.
- 사내 방화벽/네트워크 존 허용 여부는 조직의 보안 정책 결정이며, 배포 유연성만 제공한다.
- Embedding 모델, 클라이언트 UI 버그는 v1 스코프(LLM 소비 거버넌스) 밖이다.

## 관련 문서

- [`Customer_needs.md`](../Customer_needs.md) — Slack 대화·웹 리서치 기반 LiteLLM 불만 분석 (이 문서의 상위 맥락)
- [ADR-014](decisions/ADR-014-provider-registration-ux-litellm-parity.md) — 모델 등록 UX의 LiteLLM 동등성
- [ADR-017](decisions/ADR-017-budget-alert-webhooks.md) — 예산 임계값 웹훅 알림
- [ADR-019](decisions/ADR-019-bedrock-guardrails-data-plane.md) — Bedrock Guardrails 우회 방지
- [ADR-020](decisions/ADR-020-per-team-region-locking.md) — 팀별 리전 락킹(NCT 컴플라이언스)
- [ADR-007](decisions/ADR-007-chargeback-report.md) — 감사 로그 기반 차지백 리포트
- [ADR-006](decisions/ADR-006-config-hot-reload.md) — SIGHUP 기반 config 핫 리로드
