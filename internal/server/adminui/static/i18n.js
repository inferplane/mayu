// inferplane control plane — i18n (ADR-027). Self-contained dictionary + DOM
// application, no dependency (CSP default-src 'self' forbids a CDN i18n lib).
// English is the source-of-truth fallback baked into every key; ko/zh/ja are
// applied at runtime only — the served HTML text stays English (some of it is
// pinned verbatim by adminui_test.go). Language choice is NOT persisted (the
// data-free console invariant, ADR-001, permits sessionStorage only for the
// three named PKCE keys) — every load re-detects from navigator.language.
"use strict";

const MSG = {
  // nav (sidebar + view-title h1 reuse the same word, no glyph)
  "nav.overview": { en: "Overview", ko: "개요", zh: "概览", ja: "概要" },
  "nav.usage": { en: "Usage", ko: "사용량", zh: "用量", ja: "使用状況" },
  "nav.logs": { en: "Logs", ko: "로그", zh: "日志", ja: "ログ" },
  "nav.keys": { en: "Virtual keys", ko: "가상 키", zh: "虚拟密钥", ja: "仮想キー" },
  "nav.teamsUsers": { en: "Teams & Users", ko: "팀 & 사용자", zh: "团队与用户", ja: "チーム & ユーザー" },
  "nav.providersModels": { en: "Providers & Models", ko: "프로바이더 & 모델", zh: "提供方与模型", ja: "プロバイダー & モデル" },
  "nav.governance": { en: "Governance", ko: "거버넌스", zh: "治理", ja: "ガバナンス" },
  "nav.settings": { en: "Settings", ko: "설정", zh: "设置", ja: "設定" },

  "footer.sessionActive": { en: "session active", ko: "세션 활성", zh: "会话已连接", ja: "セッション有効" },
  "footer.lock": { en: "LOCK", ko: "잠금", zh: "锁定", ja: "ロック" },

  "health.healthy": { en: "healthy", ko: "정상", zh: "健康", ja: "正常" },
  "health.unhealthy": { en: "unhealthy", ko: "비정상", zh: "异常", ja: "異常" },
  "health.unreachable": { en: "unreachable", ko: "연결 불가", zh: "无法连接", ja: "接続不可" },

  // lock screen
  "lock.tagline": { en: "LLM consumption governance — control plane", ko: "LLM 사용 거버넌스 — 컨트롤 플레인", zh: "LLM 消耗治理 — 控制平面", ja: "LLM 利用ガバナンス — コントロールプレーン" },
  "lock.sso": { en: "Sign in with SSO", ko: "SSO로 로그인", zh: "使用 SSO 登录", ja: "SSO でログイン" },
  "lock.divider": { en: "or paste a token", ko: "또는 토큰 붙여넣기", zh: "或粘贴令牌", ja: "またはトークンを貼り付け" },
  "lock.label": { en: "admin token · or OIDC ID token", ko: "관리자 토큰 · 또는 OIDC ID 토큰", zh: "管理员令牌 · 或 OIDC ID 令牌", ja: "管理者トークン · または OIDC ID トークン" },
  "lock.placeholder": { en: "paste admin token, or an ID token from your IdP CLI", ko: "관리자 토큰 또는 IdP CLI의 ID 토큰을 붙여넣으세요", zh: "粘贴管理员令牌，或来自 IdP CLI 的 ID 令牌", ja: "管理者トークン、または IdP CLI の ID トークンを貼り付けてください" },
  "lock.unlock": { en: "UNLOCK CONSOLE", ko: "콘솔 잠금 해제", zh: "解锁控制台", ja: "コンソールをアンロック" },
  "lock.note": {
    en: "Sign in with SSO through your IdP, or paste a break-glass admin token or OIDC ID token. The token is held in page memory only and is never stored.",
    ko: "IdP를 통해 SSO로 로그인하거나, break-glass 관리자 토큰 또는 OIDC ID 토큰을 붙여넣으세요. 토큰은 페이지 메모리에만 보관되며 저장되지 않습니다.",
    zh: "通过您的 IdP 使用 SSO 登录，或粘贴紧急管理员令牌或 OIDC ID 令牌。令牌仅保存在页面内存中，绝不会被存储。",
    ja: "IdP を通じて SSO でログインするか、break-glass 管理者トークンまたは OIDC ID トークンを貼り付けてください。トークンはページメモリにのみ保持され、保存されることはありません。",
  },

  // overview
  "ov.reqTotal": { en: "requests · total", ko: "요청 · 전체", zh: "请求 · 总计", ja: "リクエスト · 合計" },
  "ov.activeKeys": { en: "active keys", ko: "활성 키", zh: "有效密钥", ja: "有効なキー" },
  "ov.issuedNotRevoked": { en: "issued & not revoked", ko: "발급됨 & 미폐기", zh: "已签发且未撤销", ja: "発行済み & 未失効" },
  "ov.teamsSeen": { en: "teams seen", ko: "관측된 팀", zh: "已观察到的团队", ja: "確認済みチーム" },
  "ov.acrossTraffic": { en: "across traffic", ko: "트래픽 전반", zh: "跨全部流量", ja: "全トラフィック" },
  "ov.budgetSpend": { en: "budget spend", ko: "예산 지출", zh: "预算支出", ja: "予算支出" },
  "ov.budgetSub": { en: "µUSD settled this window", ko: "이번 윈도우에 정산된 µUSD", zh: "本窗口已结算的 µUSD", ja: "このウィンドウで確定した µUSD" },
  "ov.trafficTitle": { en: "traffic by model · provider", ko: "모델 · 프로바이더별 트래픽", zh: "按模型 · 提供方的流量", ja: "モデル · プロバイダー別トラフィック" },
  "ov.noTraffic": { en: "no traffic yet — issue a key and send a request", ko: "아직 트래픽 없음 — 키를 발급하고 요청을 보내보세요", zh: "暂无流量 — 请签发密钥并发送请求", ja: "トラフィックはまだありません — キーを発行してリクエストを送信してください" },
  "ov.metricsUnreachable": { en: "/metrics unreachable", ko: "/metrics 연결 불가", zh: "/metrics 无法连接", ja: "/metrics に接続できません" },
  "ov.recentKeys": { en: "recent keys", ko: "최근 키", zh: "最近的密钥", ja: "最近のキー" },
  "ov.manageKeys": { en: "manage keys →", ko: "키 관리 →", zh: "管理密钥 →", ja: "キーを管理 →" },

  "common.noneYet": { en: "none yet", ko: "아직 없음", zh: "暂无", ja: "まだありません" },
  "common.connectToLoad": { en: "connect to load…", ko: "연결하여 불러오는 중…", zh: "连接以加载…", ja: "接続して読み込み中…" },
  "common.edit": { en: "edit", ko: "수정", zh: "编辑", ja: "編集" },
  "common.savedPrefix": { en: "saved ✓ ", ko: "저장됨 ✓ ", zh: "已保存 ✓ ", ja: "保存済み ✓ " },
  "common.testing": { en: "testing…", ko: "테스트 중…", zh: "测试中…", ja: "テスト中…" },
  "common.verifying": { en: "verifying…", ko: "검증 중…", zh: "验证中…", ja: "検証中…" },
  "common.loading": { en: "loading…", ko: "불러오는 중…", zh: "加载中…", ja: "読み込み中…" },
  "common.confirmSuffix": { en: "?", ko: "?", zh: "？", ja: "？" },
  "common.yes": { en: "yes", ko: "예", zh: "是", ja: "はい" },
  "common.no": { en: "no", ko: "아니오", zh: "否", ja: "いいえ" },
  "common.failedToLoad": { en: "failed to load", ko: "불러오기 실패", zh: "加载失败", ja: "読み込みに失敗しました" },
  "common.budgetPlaceholder": { en: "budget USD (optional)", ko: "예산 USD (선택)", zh: "预算 USD（可选）", ja: "予算 USD（任意）" },
  "common.tpmPlaceholder": { en: "TPM (optional)", ko: "TPM (선택)", zh: "TPM（可选）", ja: "TPM（任意）" },
  "common.rpmPlaceholder": { en: "RPM (optional)", ko: "RPM (선택)", zh: "RPM（可选）", ja: "RPM（任意）" },
  "common.expiresPlaceholder": { en: "expires (optional)", ko: "만료일 (선택)", zh: "过期日期（可选）", ja: "有効期限（任意）" },
  "common.ownerPlaceholder": { en: "owner (optional)", ko: "소유자 (선택)", zh: "所有者（可选）", ja: "所有者（任意）" },
  "common.guardrailIdPlaceholder": { en: "guardrail ID override (optional, bedrock)", ko: "가드레일 ID 오버라이드 (선택, bedrock)", zh: "护栏 ID 覆盖（可选，bedrock）", ja: "ガードレール ID オーバーライド（任意、bedrock）" },
  "common.guardrailVersionPlaceholder": { en: "guardrail version (optional, default DRAFT)", ko: "가드레일 버전 (선택, 기본값 DRAFT)", zh: "护栏版本（可选，默认 DRAFT）", ja: "ガードレールバージョン（任意、デフォルト DRAFT）" },
  "common.copy": { en: "COPY", ko: "복사", zh: "复制", ja: "コピー" },

  // table headers (shared across views)
  "th.model": { en: "model", ko: "모델", zh: "模型", ja: "モデル" },
  "th.provider": { en: "provider", ko: "프로바이더", zh: "提供方", ja: "プロバイダー" },
  "th.status": { en: "status", ko: "상태", zh: "状态", ja: "ステータス" },
  "th.req": { en: "req", ko: "요청", zh: "请求", ja: "リクエスト" },
  "th.keyId": { en: "key id", ko: "키 ID", zh: "密钥 ID", ja: "キー ID" },
  "th.team": { en: "team", ko: "팀", zh: "团队", ja: "チーム" },
  "th.usd": { en: "USD", ko: "USD", zh: "USD", ja: "USD" },
  "th.time": { en: "time", ko: "시간", zh: "时间", ja: "時刻" },
  "th.tokens": { en: "tokens", ko: "토큰", zh: "令牌", ja: "トークン" },
  "th.cost": { en: "cost", ko: "비용", zh: "费用", ja: "コスト" },
  "th.allowedModels": { en: "allowed models", ko: "허용된 모델", zh: "允许的模型", ja: "許可されたモデル" },
  "th.limits": { en: "limits", ko: "한도", zh: "限额", ja: "上限" },
  "th.name": { en: "name", ko: "이름", zh: "名称", ja: "名前" },
  "th.source": { en: "source", ko: "소스", zh: "来源", ja: "ソース" },
  "th.spend30d": { en: "spend (30d)", ko: "지출 (30일)", zh: "支出（30 天）", ja: "支出（30 日）" },
  "th.owner": { en: "owner", ko: "소유자", zh: "所有者", ja: "所有者" },
  "th.teamsCol": { en: "teams", ko: "팀", zh: "团队", ja: "チーム" },
  "th.keys": { en: "keys", ko: "키", zh: "密钥", ja: "キー" },
  "th.type": { en: "type", ko: "유형", zh: "类型", ja: "種別" },
  "th.endpoint": { en: "endpoint", ko: "엔드포인트", zh: "端点", ja: "エンドポイント" },
  "th.auth": { en: "auth", ko: "인증", zh: "认证", ja: "認証" },
  "th.route": { en: "route (provider · upstream model)", ko: "라우트 (프로바이더 · 업스트림 모델)", zh: "路由（提供方 · 上游模型）", ja: "ルート（プロバイダー · 上流モデル）" },
  "th.window": { en: "window", ko: "윈도우", zh: "窗口", ja: "ウィンドウ" },
  "th.utilization": { en: "utilization", ko: "사용률", zh: "使用率", ja: "使用率" },
  "th.spendUsdCumulative": { en: "spend (USD, cumulative)", ko: "지출 (USD, 누적)", zh: "支出（USD，累计）", ja: "支出（USD、累計）" },
  "th.threshold": { en: "threshold", ko: "임계값", zh: "阈值", ja: "しきい値" },
  "th.ratio": { en: "ratio", ko: "비율", zh: "比例", ja: "比率" },
  "th.delivered": { en: "delivered", ko: "전송됨", zh: "已投递", ja: "配信済み" },

  // usage
  "usage.title": { en: "usage & spend analytics", ko: "사용량 & 지출 분석", zh: "用量与支出分析", ja: "使用状況 & 支出分析" },
  "usage.hint": { en: "Enable the analytics store to see spend over time and per team/key/model breakdowns.", ko: "분석 스토어를 켜면 기간별 지출과 팀·키·모델별 사용량을 볼 수 있습니다.", zh: "启用分析存储即可查看随时间变化的支出以及按团队/密钥/模型的细分。", ja: "分析ストアを有効にすると、期間別の支出やチーム・キー・モデル別の使用量を確認できます。" },
  "usage.totalsTitle": { en: "totals · last 30 days", ko: "합계 · 최근 30일", zh: "合计 · 最近 30 天", ja: "合計 · 過去 30 日間" },
  "usage.byTeam": { en: "spend by team", ko: "팀별 지출", zh: "按团队支出", ja: "チーム別支出" },
  "usage.byModel": { en: "spend by model", ko: "모델별 지출", zh: "按模型支出", ja: "モデル別支出" },

  // logs
  "logs.title": { en: "request log viewer", ko: "요청 로그 뷰어", zh: "请求日志查看器", ja: "リクエストログビューア" },
  "logs.hint": { en: "Enable the analytics store to inspect individual requests (metadata). Prompt/response bodies require the opt-in body store.", ko: "분석 스토어를 켜면 개별 요청(메타데이터)을 볼 수 있습니다. 프롬프트/응답 본문은 옵트인 본문 스토어가 필요합니다.", zh: "启用分析存储即可查看单个请求（元数据）。提示词/响应正文需要选择启用正文存储。", ja: "分析ストアを有効にすると、個々のリクエスト（メタデータ）を確認できます。プロンプト・応答本文にはオプトインの本文ストアが必要です。" },
  "logs.recentTitle": { en: "recent requests", ko: "최근 요청", zh: "最近的请求", ja: "最近のリクエスト" },
  "logs.bodyHint1": { en: "📄 marks a request whose body was captured (opt-in ", ko: "📄는 본문이 저장된 요청입니다 (옵트인 ", zh: "📄 表示已捕获正文的请求（选择启用 ", ja: "📄 は本文が記録されたリクエストを示します（オプトイン " },
  "logs.bodyHint2": { en: ", D4/ADR-018). A streaming response is never captured — request only.", ko: ", D4/ADR-018). 스트리밍 응답은 저장되지 않습니다 — 요청만 저장됩니다.", zh: ", D4/ADR-018）。流式响应绝不会被捕获 — 仅捕获请求。", ja: "、D4/ADR-018）。ストリーミング応答は記録されません — リクエストのみです。" },
  "logs.loadMore": { en: "LOAD MORE", ko: "더 불러오기", zh: "加载更多", ja: "さらに読み込む" },
  "logs.bodyStoreTitle": { en: "body store", ko: "본문 스토어", zh: "正文存储", ja: "本文ストア" },
  "logs.enableBodyPre": { en: "Enable ", ko: "", zh: "启用 ", ja: "" },
  "logs.enableBodyPost": { en: " to view/delete captured request & response bodies from the table above.", ko: "을 켜면 위 표에서 저장된 요청·응답 본문을 보고 삭제할 수 있습니다.", zh: " 以查看/删除上表中捕获的请求与响应正文。", ja: " を有効にすると、上の表で記録されたリクエスト・応答本文を表示・削除できます。" },
  "logs.retentionTitle": { en: "body — retention & privacy", ko: "본문 — 보존 & 개인정보", zh: "正文 — 保留与隐私", ja: "本文 — 保持 & プライバシー" },
  "logs.retentionPre": {
    en: "Bodies live in a separate, deletable, encrypted store OUTSIDE the audit chain. PII masking (if enabled) is best-effort — it does not guarantee removal of secrets, credentials, or other sensitive content. Viewing a body is itself logged (",
    ko: "본문은 감사 체인과 분리된, 삭제 가능하고 암호화된 별도 저장소에 있습니다. PII 마스킹(활성화된 경우)은 최선 노력이며 시크릿·자격증명 등의 제거를 보장하지 않습니다. 본문 조회 자체도 기록됩니다 (",
    zh: "正文保存在与审计链 OUTSIDE 分离的、可删除、加密的独立存储中。PII 屏蔽（如已启用）是尽力而为 — 并不保证移除密钥、凭证或其他敏感内容。查看正文本身也会被记录（",
    ja: "本文は監査チェーンの外部にある、削除可能で暗号化された別のストアに保存されます。PII マスキング（有効な場合）はベストエフォートであり、シークレット・資格情報などの機密内容の除去を保証しません。本文の閲覧自体も記録されます（",
  },
  "logs.retentionPost": { en: ").", ko: ").", zh: "）。", ja: "）。" },
  "logs.close": { en: "CLOSE", ko: "닫기", zh: "关闭", ja: "閉じる" },
  "logs.noRequestsLogged": { en: "no requests logged yet", ko: "아직 기록된 요청 없음", zh: "暂无记录的请求", ja: "記録されたリクエストはまだありません" },
  "body.notCapturedStreaming": { en: "(not captured — streaming responses are request-only)", ko: "(저장되지 않음 — 스트리밍 응답은 요청만 저장됩니다)", zh: "（未捕获 — 流式响应仅捕获请求）", ja: "（記録されていません — ストリーミング応答はリクエストのみ記録されます）" },
  "body.deleted": { en: "body deleted.", ko: "본문이 삭제되었습니다.", zh: "正文已删除。", ja: "本文を削除しました。" },
  "body.deleteFailedPrefix": { en: "delete failed: ", ko: "삭제 실패: ", zh: "删除失败：", ja: "削除失敗: " },
  "body.deleteBtn": { en: "DELETE BODY", ko: "본문 삭제", zh: "删除正文", ja: "本文を削除" },
  "body.storeNotEnabled": { en: "body store not enabled", ko: "본문 스토어가 활성화되지 않음", zh: "正文存储未启用", ja: "本文ストアが有効になっていません" },
  "body.viewBtnTitle": { en: "view captured body", ko: "저장된 본문 보기", zh: "查看已捕获的正文", ja: "記録された本文を表示" },
  "body.confirmDelete": { en: "Permanently delete this body? This cannot be undone.", ko: "이 본문을 영구적으로 삭제할까요? 되돌릴 수 없습니다.", zh: "永久删除此正文？此操作无法撤销。", ja: "この本文を完全に削除しますか？元に戻せません。" },
  "body.recordPrefix": { en: "record ", ko: "레코드 ", zh: "记录 ", ja: "レコード " },
  "body.expiresInfix": { en: " · expires ", ko: " · 만료 ", zh: " · 过期 ", ja: " · 有効期限 " },
  "body.requestLabel": { en: "REQUEST:\n", ko: "요청:\n", zh: "请求：\n", ja: "リクエスト:\n" },
  "body.responseLabel": { en: "RESPONSE:\n", ko: "응답:\n", zh: "响应：\n", ja: "応答:\n" },

  // keys
  "keys.issueTitle": { en: "issue a virtual key", ko: "가상 키 발급", zh: "签发虚拟密钥", ja: "仮想キーの発行" },
  "keys.teamPlaceholder": { en: "team (e.g. demo)", ko: "팀 (예: demo)", zh: "团队（例如 demo）", ja: "チーム（例: demo）" },
  "keys.modelsPlaceholder": { en: "allowed models — comma-separated, * for all", ko: "허용 모델 — 쉼표로 구분, 전체는 *", zh: "允许的模型 — 以逗号分隔，* 表示全部", ja: "許可するモデル — カンマ区切り、全許可は *" },
  "keys.issueBtn": { en: "ISSUE KEY", ko: "키 발급", zh: "签发密钥", ja: "キーを発行" },
  "keys.enforceHint": { en: "Budget/TPM/RPM are enforced live on every request for this key, layered on top of the team's own limits.", ko: "예산/TPM/RPM은 이 키의 모든 요청에서 실시간으로 강제되며, 팀 자체 한도 위에 추가로 적용됩니다.", zh: "预算/TPM/RPM 会在此密钥的每个请求上实时强制执行，并叠加在团队自身限额之上。", ja: "予算/TPM/RPM はこのキーのすべてのリクエストでリアルタイムに強制され、チーム自身の上限にさらに重ねて適用されます。" },
  "keys.shownOnce": { en: "⚠ shown once — copy now, not recoverable", ko: "⚠ 한 번만 표시됨 — 지금 복사하세요, 복구 불가", zh: "⚠ 仅显示一次 — 请立即复制，无法恢复", ja: "⚠ 一度だけ表示されます — 今すぐコピーしてください。復元は不可能です" },
  "keys.issuedTitle": { en: "issued keys", ko: "발급된 키", zh: "已签发的密钥", ja: "発行済みキー" },
  "keys.revokeBtn": { en: "revoke", ko: "폐기", zh: "撤销", ja: "失効" },
  "keys.confirmRevokePrefix": { en: "Revoke ", ko: "폐기할까요: ", zh: "撤销 ", ja: "失効しますか: " },
  "err.budgetTooLarge": { en: "budget must be under $1,000,000,000", ko: "예산은 $1,000,000,000 미만이어야 합니다", zh: "预算必须低于 $1,000,000,000", ja: "予算は $1,000,000,000 未満にしてください" },
  "err.ownerTooLong": { en: "owner must be 256 characters or fewer", ko: "소유자는 256자 이하여야 합니다", zh: "所有者不得超过 256 个字符", ja: "所有者は 256 文字以内にしてください" },

  // teams & users
  "teams.title": { en: "teams & users", ko: "팀 & 사용자", zh: "团队与用户", ja: "チーム & ユーザー" },
  "teams.notEnabledHint": { en: "Team and user records are not enabled. Today teams are derived from issued keys.", ko: "팀·유저 레코드가 비활성화돼 있습니다. 현재 팀은 발급된 키에서 파생됩니다.", zh: "团队与用户记录未启用。目前团队是从已签发的密钥派生的。", ja: "チーム・ユーザーレコードは有効になっていません。現在チームは発行済みキーから導出されます。" },
  "teams.govTitle": { en: "team governance", ko: "팀 거버넌스", zh: "团队治理", ja: "チームガバナンス" },
  "teams.namePlaceholder": { en: "team name", ko: "팀 이름", zh: "团队名称", ja: "チーム名" },
  "teams.tpdPlaceholder": { en: "tokens/day (optional)", ko: "일일 토큰 (선택)", zh: "每日令牌数（可选）", ja: "1 日あたりトークン数（任意）" },
  "teams.quotaBlockDefault": { en: "quota exceeded: block (default)", ko: "쿼터 초과: 차단 (기본값)", zh: "超出配额：拦截（默认）", ja: "クォータ超過: ブロック（デフォルト）" },
  "teams.quotaBlock": { en: "quota exceeded: block", ko: "쿼터 초과: 차단", zh: "超出配额：拦截", ja: "クォータ超過: ブロック" },
  "teams.quotaWarn": { en: "quota exceeded: warn", ko: "쿼터 초과: 경고", zh: "超出配额：警告", ja: "クォータ超過: 警告" },
  "teams.budgetBlockDefault": { en: "budget exceeded: block (default)", ko: "예산 초과: 차단 (기본값)", zh: "超出预算：拦截（默认）", ja: "予算超過: ブロック（デフォルト）" },
  "teams.budgetBlock": { en: "budget exceeded: block", ko: "예산 초과: 차단", zh: "超出预算：拦截", ja: "予算超過: ブロック" },
  "teams.budgetWarn": { en: "budget exceeded: warn", ko: "예산 초과: 경고", zh: "超出预算：警告", ja: "予算超過: 警告" },
  "teams.modelsPlaceholder": { en: "default allowed models — comma-separated (optional)", ko: "기본 허용 모델 — 쉼표로 구분 (선택)", zh: "默认允许的模型 — 以逗号分隔（可选）", ja: "デフォルトで許可するモデル — カンマ区切り（任意）" },
  "teams.regionsPlaceholder": { en: "allowed regions — comma-separated (optional)", ko: "허용 리전 — 쉼표로 구분 (선택)", zh: "允许的区域 — 以逗号分隔（可选）", ja: "許可するリージョン — カンマ区切り（任意）" },
  "teams.saveBtn": { en: "SAVE TEAM", ko: "팀 저장", zh: "保存团队", ja: "チームを保存" },
  "teams.hint1": { en: "Rate/TPM/quota/budget limits are enforced live on every request for this team the moment you save — no restart.", ko: "RPM/TPM/쿼터/예산 한도는 저장 즉시 이 팀의 모든 요청에 실시간으로 적용됩니다 — 재시작이 필요 없습니다.", zh: "速率/TPM/配额/预算限制会在保存后立即对该团队的每个请求实时生效 — 无需重启。", ja: "レート/TPM/クォータ/予算の上限は保存した瞬間からこのチームのすべてのリクエストにリアルタイムで適用されます — 再起動は不要です。" },
  "teams.hint2pre": { en: "Counters are enforced ", ko: "카운터는 ", zh: "计数器按", ja: "カウンターは" },
  "teams.perInstance": { en: "per gateway instance", ko: "인스턴스별로", zh: "每个网关实例", ja: "ゲートウェイインスタンス単位" },
  "teams.hint2post": { en: " (ADR-013) — with N replicas a team may consume up to N× before blocking.", ko: " 집계됩니다(ADR-013) — 레플리카가 N개면 차단 전까지 최대 N배까지 소비될 수 있습니다.", zh: "执行（ADR-013） — 若有 N 个副本，团队在被拦截前最多可消耗 N 倍配额。", ja: "で強制されます（ADR-013） — レプリカが N 個あると、ブロックされるまでにチームは最大 N 倍消費できます。" },
  "teams.guardrailHint": { en: "Guardrail override selects a DIFFERENT Bedrock Guardrail for this team — it cannot remove the provider's configured default (no per-team opt-out, D6/ADR-019).", ko: "가드레일 오버라이드는 이 팀에 다른 Bedrock Guardrail을 지정할 뿐, provider에 설정된 기본 가드레일을 끌 수는 없습니다 (팀별 opt-out 없음, D6/ADR-019).", zh: "护栏覆盖只会为该团队选择一个不同的 Bedrock Guardrail — 它无法移除提供方配置的默认护栏（不提供按团队 opt-out，D6/ADR-019）。", ja: "ガードレールオーバーライドは、このチームに別の Bedrock Guardrail を指定するだけで、プロバイダーに設定されたデフォルトのガードレールを解除することはできません（チーム単位の opt-out はありません、D6/ADR-019）。" },
  "teams.regionsHint": { en: "Allowed regions restrict this team to providers labeled with one of these regions — a provider with NO region label is always blocked for a restricted team (fail-closed, D7/ADR-020).", ko: "허용 리전은 이 팀을 지정된 리전으로 라벨링된 provider로만 제한합니다 — 리전 라벨이 없는 provider는 제한된 팀에서 항상 차단됩니다(fail-closed, D7/ADR-020).", zh: "允许的区域会将该团队限制为仅使用带有这些区域标签的提供方 — 没有区域标签的提供方对受限团队始终会被拦截（fail-closed，D7/ADR-020）。", ja: "許可リージョンは、このチームをそれらのリージョンでラベル付けされたプロバイダーのみに制限します — リージョンラベルのないプロバイダーは、制限されたチームに対して常にブロックされます（fail-closed、D7/ADR-020）。" },
  "teams.regionsSaveWarning": { en: "Submitting this team form replaces any config-declared region policy for that team unless you include the allowed regions here.", ko: "이 팀 양식을 제출하면 여기에 허용 리전을 포함하지 않는 한 해당 팀의 config 선언 region 정책이 대체됩니다.", zh: "提交此团队表单会替换该团队在配置中声明的区域策略，除非您在此处填写允许的区域。", ja: "このチームフォームを送信すると、ここで許可リージョンを指定しない限り、そのチームの config 宣言済みリージョンポリシーが上書きされます。" },
  "teams.tableTitle": { en: "teams", ko: "팀", zh: "团队", ja: "チーム" },
  "teams.noTeams": { en: "no teams yet", ko: "아직 팀 없음", zh: "暂无团队", ja: "チームはまだありません" },
  "teams.usersTitle": { en: "users", ko: "사용자", zh: "用户", ja: "ユーザー" },
  "teams.usersHint": { en: "Users are derived read-only from key owners — there is no users table. Per-user spend is not available: audit events carry a team, not an owner.", ko: "유저는 키 소유자에서 파생된 읽기 전용 목록입니다(별도 테이블 없음). 오디트 이벤트에는 팀만 기록되어 유저별 spend는 제공되지 않습니다.", zh: "用户是从密钥所有者派生的只读列表 — 没有用户表。按用户的支出不可用：审计事件仅携带团队，不携带所有者。", ja: "ユーザーはキー所有者から導出される読み取り専用の一覧です — ユーザーテーブルはありません。ユーザー別の支出は利用できません。監査イベントにはチームのみが記録され、所有者は記録されません。" },
  "teams.noKeysIssued": { en: "no keys issued yet", ko: "아직 발급된 키 없음", zh: "暂无已签发的密钥", ja: "発行済みキーはまだありません" },
  "teams.confirmDeletePrefix": { en: "Delete team record ", ko: "팀 레코드를 삭제할까요: ", zh: "删除团队记录 ", ja: "チームレコードを削除しますか: " },

  // providers & models
  "prov.roPre": { en: "Upstream providers and model routing are declared in ", ko: "업스트림 프로바이더·모델 라우팅은 ", zh: "上游提供方与模型路由在 ", ja: "アップストリームプロバイダー・モデルルーティングは " },
  "prov.configWord": { en: "config", ko: "config", zh: "config", ja: "config" },
  "prov.roPost": { en: " (policy-as-code, ADR-003) — this view is read-only. Secrets never appear here: only the env var / file path the gateway reads, or the IAM mode.", ko: "로 선언합니다(읽기 전용, ADR-003). 시크릿 값은 표시되지 않고, gateway가 읽는 env var / 파일 경로 또는 IAM 모드만 보입니다.", zh: " 中声明（policy-as-code，ADR-003） — 此视图为只读。密钥值绝不会显示在此处：只显示网关读取的环境变量名/文件路径，或 IAM 模式。", ja: " で宣言されます（policy-as-code、ADR-003） — このビューは読み取り専用です。シークレット値はここには表示されず、ゲートウェイが読む環境変数名/ファイルパス、または IAM モードのみが表示されます。" },
  "prov.rwRun1": { en: "This gateway has a ", ko: "이 gateway에는 ", zh: "此网关拥有一个", ja: "このゲートウェイには" },
  "prov.providerStoreWord": { en: "provider store", ko: "provider store", zh: "provider store", ja: "provider store" },
  "prov.rwRun2": { en: " — you can register and edit providers/routes here (ADR-008). You register only the ", ko: "가 있어 여기서 등록·편집할 수 있습니다(ADR-008). 등록하는 것은 오직 ", zh: "，可在此注册和编辑提供方/路由（ADR-008）。您只需注册与提供方进行认证的", ja: "があり、ここでプロバイダー/ルートを登録・編集できます（ADR-008）。登録するのは、プロバイダーが認証に用いる" },
  "prov.referenceWord": { en: "reference", ko: "참조", zh: "引用", ja: "参照" },
  "prov.rwRun3": { en: " a provider authenticates with (an env var name or a file path) — ", ko: "(env var 이름 또는 파일 경로)뿐입니다 — ", zh: "（环境变量名或文件路径）— ", ja: "（環境変数名またはファイルパス）のみです — " },
  "prov.neverSecretWord": { en: "never the secret value", ko: "시크릿 값이 아닙니다", zh: "绝不是密钥值本身", ja: "シークレット値そのものではありません" },
  "prov.rwRun4": { en: "; set the value in your platform's secret store. Changes apply immediately (hot-reload).", ko: "; 값은 본인 시크릿 스토어에 두세요. 변경 사항은 즉시 적용됩니다(hot-reload).", zh: "；请将值设置在您平台的密钥存储中。更改会立即生效（hot-reload）。", ja: "。値はご利用のプラットフォームのシークレットストアに設定してください。変更は即座に反映されます（hot-reload）。" },
  "prov.tableTitle": { en: "providers", ko: "프로바이더", zh: "提供方", ja: "プロバイダー" },
  "prov.noProviders": { en: "no providers configured", ko: "설정된 프로바이더 없음", zh: "未配置提供方", ja: "プロバイダーが設定されていません" },
  "prov.registerTitle": { en: "① register & test a provider", ko: "① 프로바이더 등록 & 연결 테스트", zh: "① 注册并测试提供方", ja: "① プロバイダーの登録 & 接続テスト" },
  "prov.namePlaceholder": { en: "name (e.g. anthropic-prod)", ko: "이름 (예: anthropic-prod)", zh: "名称（例如 anthropic-prod）", ja: "名前（例: anthropic-prod）" },
  "prov.baseUrlPlaceholder": { en: "base_url (anthropic / openai_compatible)", ko: "base_url (anthropic / openai_compatible)", zh: "base_url（anthropic / openai_compatible）", ja: "base_url（anthropic / openai_compatible）" },
  "prov.refEnv": { en: "api_key_ref · env var NAME", ko: "api_key_ref · env var 이름", zh: "api_key_ref · 环境变量名", ja: "api_key_ref · 環境変数名" },
  "prov.refFile": { en: "api_key_ref · file PATH", ko: "api_key_ref · 파일 경로", zh: "api_key_ref · 文件路径", ja: "api_key_ref · ファイルパス" },
  "prov.refNone": { en: "no key (bedrock / keyless)", ko: "키 없음 (bedrock / keyless)", zh: "无密钥（bedrock / keyless）", ja: "キーなし（bedrock / keyless）" },
  "prov.refValPlaceholder": { en: "ref NAME or PATH — never the secret value", ko: "참조 이름 또는 경로 — 시크릿 값이 아닙니다", zh: "引用名称或路径 — 绝不是密钥值本身", ja: "参照名またはパス — シークレット値そのものではありません" },
  "prov.regionPlaceholder": { en: "region label (any provider type; blank = fail-closed for restricted teams)", ko: "리전 라벨 (모든 프로바이더 유형; 비워두면 제한된 팀에 fail-closed)", zh: "区域标签（任意提供方类型；留空则对受限团队 fail-closed）", ja: "リージョンラベル（任意のプロバイダー種別；空欄=制限チームに対して fail-closed）" },
  "prov.authModePlaceholder": { en: "auth mode (bedrock, e.g. irsa)", ko: "인증 모드 (bedrock, 예: irsa)", zh: "认证模式（bedrock，例如 irsa）", ja: "認証モード（bedrock、例: irsa）" },
  "prov.testBtn": { en: "TEST CONNECTION", ko: "연결 테스트", zh: "测试连接", ja: "接続テスト" },
  "prov.saveBtn": { en: "SAVE PROVIDER", ko: "프로바이더 저장", zh: "保存提供方", ja: "プロバイダーを保存" },
  "prov.warnHint": { en: "⚠ enter the env var NAME or file PATH — the gateway never stores a secret value. Set the value in your secret store.", ko: "⚠ env var 이름 또는 파일 경로를 입력하세요 — gateway는 시크릿 값을 저장하지 않습니다. 값은 본인 시크릿 스토어에 두세요.", zh: "⚠ 请输入环境变量名或文件路径 — 网关绝不会存储密钥值。请将值设置在您的密钥存储中。", ja: "⚠ 環境変数名またはファイルパスを入力してください — ゲートウェイはシークレット値を保存しません。値はご自身のシークレットストアに設定してください。" },
  "prov.testHint": { en: "TEST CONNECTION resolves the ref server-side and probes the upstream — you never paste a key.", ko: "TEST CONNECTION은 참조를 서버에서 해석해 업스트림에 연결을 시험합니다 — 키를 붙여넣지 않습니다.", zh: "TEST CONNECTION 会在服务端解析引用并探测上游 — 您无需粘贴任何密钥。", ja: "接続テストはサーバー側で参照を解決し、アップストリームへの接続を試験します — キーを貼り付ける必要はありません。" },
  "prov.addRouteTitle": { en: "② add a model route", ko: "② 모델 라우트 추가", zh: "② 添加模型路由", ja: "② モデルルートを追加" },
  "prov.modelNamePlaceholder": { en: "model name (e.g. claude-sonnet-4-6)", ko: "모델 이름 (예: claude-sonnet-4-6)", zh: "模型名称（例如 claude-sonnet-4-6）", ja: "モデル名（例: claude-sonnet-4-6）" },
  "prov.addTargetBtn": { en: "+ ADD TARGET", ko: "+ 타깃 추가", zh: "+ 添加目标", ja: "+ ターゲットを追加" },
  "prov.saveRouteBtn": { en: "SAVE ROUTE", ko: "라우트 저장", zh: "保存路由", ja: "ルートを保存" },
  "prov.targetsHintPre": { en: "Targets are tried in order (target 1 = primary, the rest = fallback). ", ko: "타깃은 순서대로 시도됩니다(타깃 1 = 기본, 나머지 = 폴백). ", zh: "目标会按顺序依次尝试（目标 1 = 主，其余 = 回退）。", ja: "ターゲットは順番に試行されます（ターゲット 1 = プライマリ、残りはフォールバック）。" },
  "prov.targetsHintPost": { en: " is optional (bedrock: invoke_model / converse).", ko: "은 선택 사항입니다(bedrock: invoke_model / converse).", zh: " 是可选的（bedrock: invoke_model / converse）。", ja: " は任意です（bedrock: invoke_model / converse）。" },
  "prov.routingTitle": { en: "model routing · primary → fallback", ko: "모델 라우팅 · 기본 → 폴백", zh: "模型路由 · 主 → 回退", ja: "モデルルーティング · プライマリ → フォールバック" },
  "prov.noRoutes": { en: "no model routes configured", ko: "설정된 모델 라우트 없음", zh: "未配置模型路由", ja: "モデルルートが設定されていません" },
  "prov.snippetTitle": { en: "add a provider · config block", ko: "프로바이더 추가 · config 블록", zh: "添加提供方 · config 代码块", ja: "プロバイダーを追加 · config ブロック" },
  "prov.bootHintPre": { en: "Without a provider store, config is loaded at boot — edit config and reload (", ko: "provider store가 없으면 config는 부팅 시 로드됩니다 — config를 수정하고 리로드하세요 (", zh: "如果没有 provider store，config 会在启动时加载 — 请修改 config 并重新加载（", ja: "provider store がない場合、config は起動時に読み込まれます — config を編集してリロードしてください（" },
  "prov.bootHintPost": { en: ") to apply. Set the referenced secret in your platform's secret store — never inline.", ko: ")로 적용하세요. 참조된 시크릿은 본인 시크릿 스토어에 두세요 — 인라인으로 두지 마세요.", zh: "）以生效。请将引用的密钥设置在您平台的密钥存储中 — 绝不要内联写入。", ja: "）で適用してください。参照先のシークレットはご利用のプラットフォームのシークレットストアに設定してください — インラインには決して書かないでください。" },
  "prov.exportTitle": { en: "git export · current topology (refs only, secret-free)", ko: "git export · 현재 토폴로지 (참조만, 시크릿 없음)", zh: "git export · 当前拓扑（仅引用，无密钥）", ja: "git export · 現在のトポロジー（参照のみ、シークレットなし）" },
  "prov.exportBtn": { en: "EXPORT", ko: "내보내기", zh: "导出", ja: "エクスポート" },
  "prov.exportHint": { en: "Commit this to Git for review / disaster recovery. The DB is the live source of truth; this is the export half (ADR-008).", ko: "리뷰 / 재해 복구를 위해 이를 Git에 커밋하세요. DB가 살아있는 source of truth이며, 이는 export 절반입니다(ADR-008).", zh: "请将其提交到 Git 以供审查/灾难恢复。数据库是实时的可信来源；这是导出的一半（ADR-008）。", ja: "レビュー・災害復旧のためにこれを Git にコミットしてください。DB がライブの信頼できる情報源であり、これはエクスポート側の半分です（ADR-008）。" },
  "prov.confirmDeletePrefix": { en: "Delete provider ", ko: "프로바이더를 삭제할까요: ", zh: "删除提供方 ", ja: "プロバイダーを削除しますか: " },
  "prov.confirmDeleteRoutePrefix": { en: "Delete model route ", ko: "모델 라우트를 삭제할까요: ", zh: "删除模型路由 ", ja: "モデルルートを削除しますか: " },
  "prov.untested": { en: "untested", ko: "테스트 안 함", zh: "未测试", ja: "未テスト" },
  "prov.ok": { en: "ok", ko: "정상", zh: "正常", ja: "正常" },
  "prov.failed": { en: "failed", ko: "실패", zh: "失败", ja: "失敗" },
  "prov.reachable": { en: "reachable", ko: "연결됨", zh: "可连接", ja: "接続可能" },
  "prov.unreachable": { en: "unreachable", ko: "연결 불가", zh: "无法连接", ja: "接続不可" },
  "prov.needTarget": { en: "add at least one target (provider + model)", ko: "타깃을 하나 이상 추가하세요 (프로바이더 + 모델)", zh: "请至少添加一个目标（提供方 + 模型）", ja: "少なくとも 1 つのターゲット（プロバイダー + モデル）を追加してください" },

  // governance
  "gov.introPre": { en: "Per-team enforcement, read live from ", ko: "팀별 집행 현황을 ", zh: "按团队的强制执行，实时读取自 ", ja: "チーム別の集行状況をリアルタイムで " },
  "gov.introPost": { en: ". Quota utilization is a ratio gauge; budget spend is a cumulative counter since process start (not a utilization gauge — a true budget gauge needs the configured limit).", ko: "에서 실시간으로 읽습니다. 쿼터는 비율 게이지, 예산 지출은 프로세스 시작 이후 누적 카운터입니다(사용률 게이지가 아님 — 진짜 예산 게이지에는 설정된 한도가 필요합니다).", zh: " 读取。配额使用率是比率仪表；预算支出是自进程启动以来的累计计数器（不是使用率仪表 — 真正的预算仪表需要配置的限额）。", ja: " から読み取ります。クォータ使用率は比率ゲージです。予算支出はプロセス起動以降の累計カウンターです（使用率ゲージではありません — 本当の予算ゲージには設定された上限が必要です）。" },
  "gov.quotaTitle": { en: "quota utilization · per team / window", ko: "쿼터 사용률 · 팀 / 윈도우별", zh: "配额使用率 · 按团队 / 窗口", ja: "クォータ使用率 · チーム / ウィンドウ別" },
  "gov.noQuota": { en: "no quota utilization reported yet", ko: "아직 보고된 쿼터 사용률 없음", zh: "暂无配额使用率报告", ja: "クォータ使用率はまだ報告されていません" },
  "gov.spendTitle": { en: "budget spend · cumulative since start", ko: "예산 지출 · 시작 이후 누적", zh: "预算支出 · 自启动以来累计", ja: "予算支出 · 起動以降の累計" },
  "gov.noSpend": { en: "no spend reported yet", ko: "아직 보고된 지출 없음", zh: "暂无支出报告", ja: "支出はまだ報告されていません" },
  "gov.alertsTitle": { en: "budget alerts · recent fires", ko: "예산 알림 · 최근 발화", zh: "预算警报 · 最近触发", ja: "予算アラート · 最近の発火" },
  "gov.alertsHintPre": { en: "Webhook fires when a team's or a key's monthly budget utilization crosses a configured threshold (", ko: "팀 또는 키의 예산 사용률이 임계치를 넘으면 웹훅이 발화합니다 (", zh: "当团队或密钥的月度预算使用率超过配置的阈值时，Webhook 会触发（", ja: "チームまたはキーの月次予算使用率が設定されたしきい値を超えると、Webhook が発火します（" },
  "gov.alertsHintPost": { en: ", ADR-017) — the \"key\" column is blank for a team-level fire. Per-instance state — a multi-replica deployment may fire once per replica.", ko: ", ADR-017) — \"key\" 열은 팀 단위 알림에서는 비어 있습니다. 인스턴스별 상태라 다중 레플리카에서는 레플리카당 중복 발화할 수 있습니다.", zh: "，ADR-017） — 团队级触发时“key”列为空。这是每实例状态 — 多副本部署可能每个副本各触发一次。", ja: "、ADR-017） — チームレベルの発火では「key」列は空です。インスタンスごとの状態のため、マルチレプリカ環境ではレプリカごとに発火する可能性があります。" },
  "gov.noAlerts": { en: "no alerts fired yet", ko: "아직 발화된 알림 없음", zh: "暂无已触发的警报", ja: "発火したアラートはまだありません" },
  "gov.auditTitle": { en: "audit integrity", ko: "감사 무결성", zh: "审计完整性", ja: "監査の整合性" },
  "gov.auditHint": { en: "Verify the tamper-evident hash chain of each file sink, on demand.", ko: "각 파일 sink의 tamper-evident 해시 체인을 필요할 때 검증합니다.", zh: "按需验证每个文件 sink 的防篡改哈希链。", ja: "各ファイル sink の耐タンパー性ハッシュチェーンを、必要に応じて検証します。" },
  "gov.verifyBtn": { en: "VERIFY CHAIN", ko: "체인 검증", zh: "验证链", ja: "チェーンを検証" },
  "gov.noSink": { en: "no file audit sink configured (stdout-only deployment)", ko: "설정된 파일 감사 sink 없음 (stdout 전용 배포)", zh: "未配置文件审计 sink（仅 stdout 部署）", ja: "ファイル監査 sink が設定されていません（stdout 専用デプロイ）" },
  "gov.chainOk": { en: "chain OK", ko: "체인 정상", zh: "链正常", ja: "チェーン正常" },
  "gov.recordsWord": { en: "records", ko: "레코드", zh: "条记录", ja: "件のレコード" },
  "gov.partialTail": { en: "[complete prefix; trailing partial line ignored]", ko: "[완전한 접두부; 마지막 부분 라인은 무시됨]", zh: "[前缀完整；末尾不完整的行已忽略]", ja: "[完全な接頭部；末尾の不完全な行は無視されました]" },
  "gov.brokenAtPrefix": { en: "BROKEN at record ", ko: "레코드 위치에서 손상됨: ", zh: "在记录处损坏: ", ja: "レコードで破損: " },
  "gov.notOk": { en: "not OK", ko: "정상 아님", zh: "不正常", ja: "正常ではありません" },

  // settings
  "settings.introPre": { en: "Point a coding agent at this gateway with a virtual key. Issue a key in ", ko: "가상 키로 코딩 에이전트가 이 gateway를 가리키게 하세요. ", zh: "使用虚拟密钥将编码代理指向此网关。请在", ja: "仮想キーを使ってコーディングエージェントをこのゲートウェイに向けてください。" },
  "settings.virtualKeysWord": { en: "Virtual keys", ko: "가상 키", zh: "虚拟密钥", ja: "仮想キー" },
  "settings.introPost": { en: " and these snippets fill in with live values.", ko: "에서 키를 발급하면 아래 예시에 실제 값이 채워집니다.", zh: "中签发密钥，以下代码片段将自动填入实际值。", ja: "でキーを発行すると、以下のスニペットに実際の値が入ります。" },
  "settings.claudeCodeTitle": { en: "claude code", ko: "claude code", zh: "claude code", ja: "claude code" },
  "settings.curlTitle": { en: "curl · anthropic messages", ko: "curl · anthropic messages", zh: "curl · anthropic messages", ja: "curl · anthropic messages" },
  "settings.openaiTitle": { en: "openai-compatible clients · opencode", ko: "openai-compatible clients · opencode", zh: "openai-compatible clients · opencode", ja: "openai-compatible clients · opencode" },
  "settings.routableModels": { en: "routable models: ", ko: "라우팅 가능한 모델: ", zh: "可路由的模型：", ja: "ルーティング可能なモデル: " },
  "settings.healthLabel": { en: "health ", ko: "health ", zh: "health ", ja: "health " },
  "settings.metricsLabel": { en: " · metrics ", ko: " · metrics ", zh: " · metrics ", ja: " · metrics " },
  "settings.keysApiLabel": { en: " · keys API ", ko: " · keys API ", zh: " · keys API ", ja: " · keys API " },
  "settings.bearerLabel": { en: " (Bearer)", ko: " (Bearer)", zh: " (Bearer)", ja: " (Bearer)" },
  "settings.issueKeyFirst": { en: "<issue a key first>", ko: "<먼저 키를 발급하세요>", zh: "<请先签发密钥>", ja: "<先にキーを発行してください>" },
  "settings.routableModelsFallback": { en: "see your gateway config (models map)", ko: "gateway config를 확인하세요 (models map)", zh: "请查看您的网关配置（models map）", ja: "ゲートウェイの config（models map）を確認してください" },
};

// LANG: navigator.language prefix match against ko/zh/ja, else en. Never
// persisted — every fresh load re-detects (ADR-001 data-free posture).
function detectLang() {
  const p = (navigator.language || "en").slice(0, 2).toLowerCase();
  return (p === "ko" || p === "zh" || p === "ja") ? p : "en";
}
let LANG = detectLang();

function msg(key) {
  const m = MSG[key];
  if (!m) return key;
  return m[LANG] || m.en;
}

// applyLang re-renders every static data-i18n element in place. Elements the
// app.js render functions also write to at runtime are deliberately never
// marked data-i18n (see adminui CLAUDE.md) — they're re-translated by
// re-invoking that view's own refresh function instead, so this walk never
// clobbers a value app.js just computed.
function applyLang(lang) {
  LANG = lang;
  document.documentElement.lang = lang;
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    el.textContent = msg(el.dataset.i18n);
  });
  document.querySelectorAll("[data-i18n-placeholder]").forEach((el) => {
    el.placeholder = msg(el.dataset.i18nPlaceholder);
  });
}
