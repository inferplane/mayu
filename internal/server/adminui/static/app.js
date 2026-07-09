// inferplane control plane (ADR-001/ADR-002). The admin token lives in this
// variable only — never persisted (no storage APIs, no cookies, no URL).
// All rendering is textContent-only; data never becomes markup.
"use strict";

let adminToken = "";
let lastIssuedKey = ""; // shown-once plaintext, page-lifetime only
let whoamiIsAdmin = false; // gates the Teams write form/actions client-side (server still enforces via requireAdmin)

const $ = (id) => document.getElementById(id);

const DISABLED = Symbol("capability-disabled");

async function api(method, path, body, optional) {
  const resp = await fetch(path, {
    method,
    headers: {
      "Authorization": "Bearer " + adminToken,
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) throw new Error("unauthorized — check the admin token");
  // Opt-in only: an optional/capability endpoint that is absent → disabled,
  // not an error (§9.1). Required calls (optional falsy) still throw below.
  if (optional && (resp.status === 404 || resp.status === 405 || resp.status === 501)) return DISABLED;
  if (!resp.ok && resp.status !== 204) {
    // Surface the server's own {"error":...} message when present (the write
    // endpoints return sanitized, secret-free messages).
    let msg = "API error " + resp.status;
    try { const j = await resp.json(); if (j && j.error) msg = j.error; } catch { /* keep generic */ }
    throw new Error(msg);
  }
  return resp.status === 204 ? null : resp.json();
}

/* ---------- views ---------- */

const VIEWS = {
  overview: "Overview",
  usage: "Usage",
  logs: "Logs",
  keys: "Virtual keys",
  teams: "Teams & Users",
  providers: "Providers & Models",
  governance: "Governance",
  settings: "Settings",
};

function showView(name) {
  for (const v of Object.keys(VIEWS)) $("view-" + v).hidden = v !== name;
  document.querySelectorAll("button.nav").forEach((b) =>
    b.classList.toggle("active", b.dataset.view === name));
  $("view-title").textContent = VIEWS[name];
  if (name === "overview") refreshOverview();
  if (name === "usage") refreshUsageView();
  if (name === "logs") refreshLogsView();
  if (name === "teams") refreshTeamsView();
  if (name === "providers") refreshProviders();
  if (name === "governance") refreshGovernance();
}

document.querySelectorAll("[data-view]").forEach((b) =>
  b.addEventListener("click", () => showView(b.dataset.view)));

/* ---------- capabilities (bootstrap, §4.4 / degradation §9.1) ---------- */

let caps = null;

async function loadCapabilities() {
  const out = await api("GET", "/admin/capabilities", null, true); // optional=true
  caps = (out && out !== DISABLED) ? out : {}; // absent endpoint → all-off (safe default)
  applyCapabilities();
}

// Each affordance card declares the capability it needs via data-cap; when the
// capability is present we hide the "enable X" card. Nav buttons are NOT
// disabled — sections stay navigable and show the affordance (§9.1).
function applyCapabilities() {
  document.querySelectorAll(".affordance[data-cap]").forEach((el) => {
    el.hidden = capOn(el.dataset.cap);
  });
}

// capOn maps a capability key to a strict boolean. analytics_index is an enum
// ("A"|"B"|"off"); everything else is a bool.
function capOn(key) {
  if (!caps) return false;
  const v = caps[key];
  if (v === "off") return false; // any enum-valued capability explicitly off (e.g. analytics_index)
  return !!v;                    // bool true, or a non-empty/non-"off" enum ("A"/"B")
}

/* ---------- usage analytics (real data when the index is on) ---------- */

const usd = (micros) => "$" + (Number(micros) / 1e6).toFixed(4);

async function refreshUsageView() {
  const content = $("usage-content");
  if (!capOn("analytics_index")) { content.hidden = true; return; }
  const s = await api("GET", "/admin/analytics/summary", null, true); // optional
  if (!s || s === DISABLED) { content.hidden = true; return; }
  content.hidden = false;

  const totals = $("usage-totals").querySelector("tbody");
  totals.replaceChildren();
  const tline = (k, v) => { const tr = document.createElement("tr"); tr.append(td(k), td(v)); totals.append(tr); };
  tline("requests", String(s.totals.requests));
  tline("input tokens", String(s.totals.input_tokens));
  tline("output tokens", String(s.totals.output_tokens));
  tline("cache read", String(s.totals.cache_read_tokens));
  tline("spend", usd(s.totals.cost_micros));

  const fill = (id, rows, nameKey) => {
    const tb = $(id).querySelector("tbody");
    tb.replaceChildren();
    (rows || []).forEach((r) => {
      const tr = document.createElement("tr");
      tr.append(td(r[nameKey] || "—"), td(String(r.requests)), td(usd(r.cost_micros)));
      tb.append(tr);
    });
  };
  fill("usage-by-team", s.by_team, "team");
  fill("usage-by-model", s.by_model, "model");
}

/* ---------- sparkline (hand-rolled inline SVG; no vendored chart lib, §10) ---------- */

// Builds SVG via DOM node APIs only — data never becomes markup.
function renderSparkline(container, values) {
  container.replaceChildren();
  if (values.length <= 1) return; // one point has no line to draw
  const w = 160, h = 36, pad = 2;
  const max = Math.max(...values, 1); // avoid divide-by-zero on all-zero data
  const step = values.length > 1 ? (w - pad * 2) / (values.length - 1) : 0;
  const d = values
    .map((v, i) => (i === 0 ? "M" : "L") + (pad + i * step) + "," + (h - pad - (v / max) * (h - pad * 2)))
    .join(" ");
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${w} ${h}`);
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", d);
  path.setAttribute("fill", "none");
  svg.appendChild(path);
  container.appendChild(svg);
}

/* ---------- health ---------- */

async function pollHealth() {
  const led = $("health-led"), txt = $("health-text");
  try {
    const r = await fetch("/healthz");
    led.className = r.ok ? "led on" : "led bad";
    txt.textContent = r.ok ? "healthy" : "unhealthy";
  } catch {
    led.className = "led bad";
    txt.textContent = "unreachable";
  }
}

/* ---------- metrics (Prometheus text, parsed client-side) ---------- */

function parseMetrics(text) {
  // inferplane_requests_total{ingress="anthropic",model="m",provider="p",status="2xx",team="t"} 3
  const rows = [];
  let spendMicros = 0;
  for (const line of text.split("\n")) {
    if (line.startsWith("inferplane_requests_total{")) {
      const m = line.match(/\{(.*)\}\s+([0-9.e+]+)\s*$/);
      if (!m) continue;
      const labels = {};
      for (const kv of m[1].match(/(\w+)="([^"]*)"/g) || []) {
        const [, k, v] = kv.match(/(\w+)="([^"]*)"/);
        labels[k] = v;
      }
      rows.push({ ...labels, count: Number(m[2]) });
    } else if (line.startsWith("inferplane_budget_spend_usd_micros")) {
      const m = line.match(/\s([0-9.e+]+)\s*$/);
      if (m) spendMicros += Number(m[1]);
    }
  }
  return { rows, spendMicros };
}

async function refreshOverview() {
  // spend trend (real data when the analytics index is on; else no sparkline)
  if (capOn("analytics_index")) {
    try {
      const ts = await api("GET", "/admin/analytics/timeseries?days=30", null, true);
      if (ts && ts !== DISABLED) {
        renderSparkline($("stat-spend-spark"), ts.slice().reverse().map((p) => p.cost_micros));
      }
    } catch { /* sparkline is best-effort; don't abort the rest of the overview */ }
  }

  // keys (authoritative, via admin API)
  try {
    const out = await api("GET", "/admin/keys");
    const keys = out.data || [];
    $("stat-keys").textContent = String(keys.length);
    const tbody = $("recent-keys").querySelector("tbody");
    tbody.textContent = "";
    if (!keys.length) {
      tbody.appendChild(emptyRow(2, "none yet"));
    } else {
      for (const k of keys.slice(-6).reverse()) {
        const tr = document.createElement("tr");
        for (const v of [k.key_id, k.team]) {
          const td = document.createElement("td");
          td.textContent = v;
          tr.appendChild(td);
        }
        tbody.appendChild(tr);
      }
    }
  } catch { /* keys table keeps its last state */ }

  // traffic (best-effort, /metrics is unauthenticated on this plane)
  try {
    const text = await (await fetch("/metrics")).text();
    const { rows, spendMicros } = parseMetrics(text);
    const total = rows.reduce((n, r) => n + r.count, 0);
    $("stat-requests").textContent = String(total);
    const teams = new Set(rows.map((r) => r.team));
    $("stat-teams").textContent = String(teams.size);
    $("stat-teams-sub").textContent = teams.size ? [...teams].slice(0, 4).join(", ") : "across traffic";
    $("stat-spend").textContent = spendMicros ? spendMicros.toLocaleString() : "0";
    const tbody = $("traffic-table").querySelector("tbody");
    tbody.textContent = "";
    if (!rows.length) {
      tbody.appendChild(emptyRow(4, "no traffic yet — issue a key and send a request"));
    } else {
      for (const r of rows.sort((a, b) => b.count - a.count).slice(0, 8)) {
        const tr = document.createElement("tr");
        tr.appendChild(td(r.model));
        tr.appendChild(td(r.provider));
        const st = document.createElement("td");
        const pill = document.createElement("span");
        pill.className = r.status === "2xx" ? "pill" : "pill err";
        pill.textContent = r.status;
        st.appendChild(pill);
        tr.appendChild(st);
        const n = td(String(r.count));
        n.className = "num";
        tr.appendChild(n);
        tbody.appendChild(tr);
      }
    }
  } catch {
    $("stat-requests-sub").textContent = "/metrics unreachable";
  }
}

function td(text) { const e = document.createElement("td"); e.textContent = text; return e; }
function emptyRow(span, text) {
  const tr = document.createElement("tr");
  const e = document.createElement("td");
  e.colSpan = span; e.className = "empty"; e.textContent = text;
  tr.appendChild(e);
  return tr;
}

/* ---------- providers (topology view, ADR-005; UI-write when a store is
   enabled, ADR-008) ---------- */

async function refreshProviders() {
  let view;
  try {
    view = await api("GET", "/admin/config");
  } catch {
    return; // keep last state; auth errors surface on the lock screen
  }
  const writable = !!view.writable;
  // Toggle write affordances (CSP-safe: the `hidden` DOM property, no inline style).
  $("providers-mode-ro").hidden = writable;
  $("providers-mode-rw").hidden = !writable;
  $("provider-write").hidden = !writable;
  $("model-write").hidden = !writable;
  lastProviders = view.providers || [];
  $("export-card").hidden = !writable;
  $("prov-act-col").hidden = !writable;
  $("prov-status-col").hidden = !writable;
  $("route-act-col").hidden = !writable;
  if (writable && !$("mf-targets").querySelector(".target-row")) addTargetRow();

  const pbody = $("providers-table").querySelector("tbody");
  pbody.textContent = "";
  const provs = view.providers || [];
  if (!provs.length) {
    pbody.appendChild(emptyRow(writable ? 6 : 4, "no providers configured"));
  } else {
    for (const p of provs) {
      const tr = document.createElement("tr");
      tr.appendChild(td(p.name));
      tr.appendChild(td(p.type));
      tr.appendChild(td(p.base_url || "(default)"));
      tr.appendChild(td(p.auth)); // ref name / IAM mode — never a secret value
      if (writable) {
        const statusCell = document.createElement("td");
        probeBadge(statusCell, probeCacheGet(p.name));
        tr.appendChild(statusCell);
        tr.appendChild(providerActions(p));
      }
      pbody.appendChild(tr);
    }
  }

  const rbody = $("routes-table").querySelector("tbody");
  rbody.textContent = "";
  const models = view.models || [];
  if (!models.length) {
    rbody.appendChild(emptyRow(writable ? 3 : 2, "no model routes configured"));
  } else {
    for (const m of models) {
      const tr = document.createElement("tr");
      tr.appendChild(td(m.name));
      const route = (m.targets || [])
        .map((t) => t.provider + " · " + t.model + (t.api ? " (" + t.api + ")" : ""))
        .join("  →  ");
      tr.appendChild(td(route));
      if (writable) tr.appendChild(routeActions(m));
      rbody.appendChild(tr);
    }
  }
}

// providerActions builds the edit/delete cell for a provider row (writable only).
function providerActions(p) {
  const cell = document.createElement("td");
  const edit = document.createElement("button");
  edit.className = "ghost"; edit.textContent = "edit";
  edit.addEventListener("click", () => fillProviderForm(p));
  const del = document.createElement("button");
  del.className = "ghost"; del.textContent = "✕";
  del.addEventListener("click", async () => {
    if (!confirm("Delete provider " + p.name + "?")) return;
    try { await api("DELETE", "/admin/providers/" + encodeURIComponent(p.name)); await refreshProviders(); }
    catch (err) { $("provider-form-status").className = "status err"; $("provider-form-status").textContent = String(err.message || err); }
  });
  cell.append(edit, del);
  return cell;
}

// routeActions builds the edit/delete cell for a model-route row.
function routeActions(m) {
  const cell = document.createElement("td");
  const edit = document.createElement("button");
  edit.className = "ghost"; edit.textContent = "edit";
  edit.addEventListener("click", () => fillModelForm(m));
  const del = document.createElement("button");
  del.className = "ghost"; del.textContent = "✕";
  del.addEventListener("click", async () => {
    if (!confirm("Delete model route " + m.name + "?")) return;
    try { await api("DELETE", "/admin/models/" + encodeURIComponent(m.name)); await refreshProviders(); }
    catch (err) { $("model-form-status").className = "status err"; $("model-form-status").textContent = String(err.message || err); }
  });
  cell.append(edit, del);
  return cell;
}

// PROVIDER_FIELDS maps a provider type to the form fields relevant to it
// (ADR-014 D1). The form morphs on type change: anthropic/openai_compatible
// authenticate with an api_key_ref over a base_url; bedrock uses an AWS region +
// IAM auth mode and no key. Irrelevant fields are hidden, not just ignored.
const PROVIDER_FIELDS = {
  anthropic: ["pf-baseurl", "pf-refkind", "pf-refval"],
  openai_compatible: ["pf-baseurl", "pf-refkind", "pf-refval"],
  bedrock: ["pf-region", "pf-authmode"],
};
const PROVIDER_FIELD_IDS = ["pf-baseurl", "pf-refkind", "pf-refval", "pf-region", "pf-authmode"];

// Registered providers from the last topology load, used to populate the route
// target dropdown + typeahead (ADR-014 D4). catalogCache memoizes type→models.
let lastProviders = [];
let catalogCache = {};
let targetSeq = 0;

// loadCatalog returns known model ids for a provider type (ADR-014 D3),
// memoized. Advisory only — failures degrade to free-text (empty list).
async function loadCatalog(type) {
  if (!type) return [];
  if (catalogCache[type]) return catalogCache[type];
  try {
    const out = await api("GET", "/admin/providers/catalog?type=" + encodeURIComponent(type));
    catalogCache[type] = (out && out.models) || [];
  } catch { catalogCache[type] = []; }
  return catalogCache[type];
}

// providerType returns the configured type for a provider name (or "").
function providerType(name) {
  const p = lastProviders.find((x) => x.name === name);
  return p ? p.type : "";
}

function applyProviderTypeFields() {
  const shown = PROVIDER_FIELDS[$("pf-type").value] || [];
  for (const id of PROVIDER_FIELD_IDS) $(id).hidden = !shown.includes(id);
}

// fillProviderForm prefills the register/edit form from a provider view row.
// The auth STRING is parsed back to the ref kind/name (never a secret value).
function fillProviderForm(p) {
  $("pf-name").value = p.name;
  $("pf-type").value = p.type;
  applyProviderTypeFields();
  $("pf-baseurl").value = (p.base_url && p.base_url !== "(default)") ? p.base_url : "";
  $("pf-region").value = p.region || "";
  $("pf-authmode").value = "";
  $("pf-refkind").value = "none";
  $("pf-refval").value = "";
  const a = p.auth || "";
  if (a.indexOf("IAM · ") === 0) {
    $("pf-authmode").value = a.slice("IAM · ".length);
  } else if (a.indexOf("api key · env:") === 0) {
    $("pf-refkind").value = "env"; $("pf-refval").value = a.slice("api key · env:".length);
  } else if (a.indexOf("api key · file:") === 0) {
    $("pf-refkind").value = "file"; $("pf-refval").value = a.slice("api key · file:".length);
  }
}

// fillModelForm prefills the model-route form from a route view row.
function fillModelForm(m) {
  $("mf-name").value = m.name;
  $("mf-targets").textContent = "";
  for (const t of m.targets || []) addTargetRow(t.provider, t.model, t.api);
  if (!(m.targets || []).length) addTargetRow();
}

// addTargetRow appends one ordered-target row to the model form (ADR-014 D4).
// The provider is a <select> populated from the registered providers (a route to
// a missing provider is rejected server-side, so this moves the failure to
// authoring time). The upstream model is a free-text input with a <datalist>
// typeahead sourced from the provider type's catalog (advisory — never blocks).
function addTargetRow(provider, model, apiv) {
  const row = document.createElement("div");
  row.className = "row target-row";

  const p = document.createElement("select");
  p.className = "t-provider";
  const names = lastProviders.map((x) => x.name);
  if (provider && !names.includes(provider)) names.unshift(provider); // preserve on edit
  for (const name of names) {
    const opt = document.createElement("option");
    opt.value = name; opt.textContent = name;
    if (name === provider) opt.selected = true;
    p.appendChild(opt);
  }

  const dlId = "tgt-models-" + (++targetSeq);
  const dl = document.createElement("datalist");
  dl.id = dlId;
  const m = document.createElement("input");
  m.type = "text"; m.placeholder = "upstream model"; m.className = "t-model"; m.value = model || "";
  m.setAttribute("list", dlId);

  const fillModels = async () => {
    const models = await loadCatalog(providerType(p.value));
    dl.textContent = "";
    for (const id of models) {
      const opt = document.createElement("option");
      opt.value = id;
      dl.appendChild(opt);
    }
  };
  p.addEventListener("change", fillModels);
  fillModels(); // initial population for the selected provider

  const a = document.createElement("input");
  a.type = "text"; a.placeholder = "api (optional)"; a.className = "t-api"; a.value = apiv || "";
  const rm = document.createElement("button");
  rm.type = "button"; rm.className = "ghost"; rm.textContent = "✕";
  rm.addEventListener("click", () => row.remove());
  row.append(p, m, dl, a, rm);
  $("mf-targets").appendChild(row);
}

// providerFormBody reads the register/edit form into a ProviderWrite body
// (refs only — env NAME / file PATH, never a secret value). Shared by the save
// and the connection-test paths (ADR-014 D2).
function providerFormBody() {
  const name = $("pf-name").value.trim();
  const body = { type: $("pf-type").value };
  const bu = $("pf-baseurl").value.trim(); if (bu) body.base_url = bu;
  const region = $("pf-region").value.trim(); if (region) body.region = region;
  const mode = $("pf-authmode").value.trim(); if (mode) body.auth = { mode: mode };
  const kind = $("pf-refkind").value, val = $("pf-refval").value.trim();
  if (kind === "env" && val) body.api_key_ref = { env: val };
  else if (kind === "file" && val) body.api_key_ref = { file: val };
  return { name, body };
}

// Probe results are cached IN MEMORY (page-session only) keyed by provider name
// (ADR-014 D5). The server probe is stateless; this client cache keeps the table
// status across re-renders within the open page. It deliberately avoids any
// browser-persistent store — the data-free console invariant (ADR-001, enforced
// by adminui_test) forbids client-side persistence — so status resets on a full
// page reload (re-test to refresh). Never holds a secret.
const probeResults = {};
function probeCacheGet(name) { return probeResults[name] || null; }
function probeCacheSet(name, result) { probeResults[name] = result; }

// probeBadge renders a provider's cached health into a table cell.
function probeBadge(cell, result) {
  cell.className = "probe-badge";
  if (!result) { cell.classList.add("untested"); cell.textContent = "○ untested"; return; }
  if (result.ok) {
    cell.classList.add("ok");
    cell.textContent = "● ok" + (result.latency_ms ? " (" + result.latency_ms + "ms)" : "");
  } else {
    cell.classList.add("fail");
    cell.textContent = "● " + (result.detail || "failed");
  }
}

// Provider register/edit: PUT replaces the named provider (upsert). The body
// carries only the ref (env NAME / file PATH), never a secret value.
$("provider-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const { name, body } = providerFormBody();
  const status = $("provider-form-status");
  try {
    await api("PUT", "/admin/providers/" + encodeURIComponent(name), body);
    status.className = "status"; status.textContent = "saved ✓ " + name;
    $("provider-form").reset();
    applyProviderTypeFields();
    await refreshProviders();
  } catch (err) {
    status.className = "status err"; status.textContent = String(err.message || err);
  }
});

// TEST CONNECTION: probe the DRAFT in the form (server resolves the ref; the
// client sends no secret). Caches the result by provider name so the table
// status survives a refresh (ADR-014 D2/D5).
$("pf-test").addEventListener("click", async () => {
  const { name, body } = providerFormBody();
  const status = $("provider-form-status");
  status.className = "status"; status.textContent = "testing…";
  try {
    const res = await api("POST", "/admin/providers/test", body);
    if (name) { probeCacheSet(name, res); }
    status.className = res.ok ? "status" : "status err";
    status.textContent = (res.ok ? "✓ reachable" : "✗ " + (res.detail || "unreachable"))
      + (res.latency_ms ? " · " + res.latency_ms + "ms" : "");
    await refreshProviders();
  } catch (err) {
    status.className = "status err"; status.textContent = String(err.message || err);
  }
});

// Model route save: PUT replaces the named route's ordered target chain.
$("model-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = $("mf-name").value.trim();
  const targets = [];
  for (const row of $("mf-targets").querySelectorAll(".target-row")) {
    const provider = row.querySelector(".t-provider").value.trim();
    const model = row.querySelector(".t-model").value.trim();
    const apiv = row.querySelector(".t-api").value.trim();
    if (provider && model) {
      const t = { provider: provider, model: model };
      if (apiv) t.api = apiv;
      targets.push(t);
    }
  }
  const status = $("model-form-status");
  if (!targets.length) {
    status.className = "status err"; status.textContent = "add at least one target (provider + model)";
    return;
  }
  try {
    await api("PUT", "/admin/models/" + encodeURIComponent(name), { targets: targets });
    status.className = "status"; status.textContent = "saved ✓ " + name;
    $("mf-name").value = ""; $("mf-targets").textContent = ""; addTargetRow();
    await refreshProviders();
  } catch (err) {
    status.className = "status err"; status.textContent = String(err.message || err);
  }
});

$("mf-add-target").addEventListener("click", () => addTargetRow());

// Morph the provider form to the selected type (ADR-014 D1).
$("pf-type").addEventListener("change", applyProviderTypeFields);
applyProviderTypeFields();

// Git export: render the secret-free committable config fragment.
$("export-btn").addEventListener("click", async () => {
  try {
    const out = await api("GET", "/admin/config/export");
    $("export-out").textContent = JSON.stringify(out, null, 2);
  } catch (err) {
    $("export-out").textContent = String(err.message || err);
  }
});

/* ---------- keys ---------- */

// keyLimitsSummary renders the recorded (not-yet-enforced, §8 D2) governance
// fields as one compact string — avoids five mostly-empty table columns.
function keyLimitsSummary(k) {
  const parts = [];
  if (k.budget_usd_micros) parts.push("$" + (k.budget_usd_micros / 1e6).toFixed(2));
  if (k.tpm) parts.push(k.tpm + " tpm");
  if (k.rpm) parts.push(k.rpm + " rpm");
  if (k.expires_at) parts.push("exp " + k.expires_at.slice(0, 10));
  if (k.owner) parts.push(k.owner);
  return parts.length ? parts.join(" · ") : "—";
}

async function refreshKeys() {
  const out = await api("GET", "/admin/keys");
  const tbody = $("keys").querySelector("tbody");
  tbody.textContent = "";
  for (const k of out.data || []) {
    const tr = document.createElement("tr");
    for (const v of [k.key_id, k.team, (k.allowed_models || []).join(", "), keyLimitsSummary(k)]) {
      tr.appendChild(td(v)); // textContent only — never markup from data
    }
    const cell = document.createElement("td");
    const btn = document.createElement("button");
    btn.textContent = "revoke";
    btn.addEventListener("click", async () => {
      if (!confirm("Revoke " + k.key_id + "?")) return;
      await api("DELETE", "/admin/keys/" + encodeURIComponent(k.key_id));
      await refreshKeys();
    });
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  }
}

// loadWhoami fetches the caller's resolved identity (ADR-010) and adapts the
// issue-key form: a non-admin with entitled teams picks from a constrained
// <select> (so the UI never invites a cross-team request the server would 403);
// an admin keeps the free-text team input. Identity is rendered via textContent.
async function loadWhoami() {
  let me;
  try {
    me = await api("GET", "/admin/whoami");
  } catch {
    return; // identity is advisory; the form still works (server enforces)
  }
  whoamiIsAdmin = !!me.is_admin;
  const line = $("whoami-line");
  const input = $("team"), sel = $("team-select");
  const teams = me.teams || [];
  let note = "signed in as " + me.subject + " · " + (me.auth_method || "");
  if (!me.is_admin && teams.length) {
    // The select is advisory; the server is the entitlement authority (it 403s a
    // cross-team request regardless of the UI).
    note += " · you may issue keys for your team(s); entitlement is enforced server-side";
  }
  line.textContent = note;
  line.hidden = false;

  if (!me.is_admin && teams.length) {
    sel.textContent = "";
    for (const t of teams) {
      const opt = document.createElement("option");
      opt.value = t;
      opt.textContent = t;
      sel.appendChild(opt);
    }
    sel.value = teams[0];
    sel.hidden = false;
    sel.required = true;
    input.hidden = true;
    input.required = false;
  } else {
    // admin / break-glass / no mapped team → free entry (unchanged)
    sel.hidden = true;
    sel.required = false;
    input.hidden = false;
    input.required = true;
  }
}

// currentTeam reads whichever team control is active (select for self-service,
// text input for admin free-entry).
function currentTeam() {
  const sel = $("team-select");
  return sel.hidden ? $("team").value.trim() : sel.value;
}

$("create-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const status = $("create-form-status");
  status.textContent = "";
  status.className = "status";
  try {
    const team = currentTeam();
    const models = $("models").value.split(",").map((s) => s.trim()).filter(Boolean);
    const body = { team: team, allowed_models: models };
    const budget = $("kf-budget").value;
    if (budget) {
      // JS Number is a 53-bit-mantissa float; above ~$9B (Number.MAX_SAFE_INTEGER
      // / 1e6) budget*1e6 silently loses precision before it ever reaches the
      // server's integer-microUSD path. No real key needs a nine-figure budget.
      if (Number(budget) > 1e9) throw new Error("budget must be under $1,000,000,000");
      body.budget_usd_micros = Math.round(Number(budget) * 1e6);
    }
    // parseInt (not Number): TPM/RPM are integers server-side (int64) — a float
    // like "1000.5" would otherwise fail JSON decode with a misleading error.
    if ($("kf-tpm").value) body.tpm = parseInt($("kf-tpm").value, 10);
    if ($("kf-rpm").value) body.rpm = parseInt($("kf-rpm").value, 10);
    // End-of-day UTC, not midnight — a UTC-negative operator picking "today"
    // would otherwise see the key expire hours before their own day ends.
    if ($("kf-expires").value) body.expires_at = $("kf-expires").value + "T23:59:59Z";
    if ($("kf-owner").value) {
      const owner = $("kf-owner").value.trim();
      if (owner.length > 256) throw new Error("owner must be 256 characters or fewer");
      body.owner = owner;
    }
    const out = await api("POST", "/admin/keys", body);
    ["kf-budget", "kf-tpm", "kf-rpm", "kf-expires", "kf-owner"].forEach((id) => { $(id).value = ""; });
    // Plaintext is rendered once, kept only in the DOM/page until reload.
    lastIssuedKey = out.plaintext;
    $("plaintext").textContent = out.plaintext;
    $("plaintext-box").hidden = false;
    renderUsage(out.plaintext);
    loadModels(out.plaintext);
    await refreshKeys();
  } catch (err) {
    status.className = "status err";
    status.textContent = String(err.message || err);
  }
});

$("copy").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("plaintext").textContent);
});

/* ---------- teams & users (D3, ADR-016) ---------- */

// teamLimitsSummary mirrors keyLimitsSummary's compact-string convention —
// avoids five mostly-empty table columns.
function teamLimitsSummary(t) {
  const parts = [];
  if (t.budget_usd_micros) parts.push("$" + (t.budget_usd_micros / 1e6).toFixed(2) + " (" + (t.budget_on_exceeded || "block") + ")");
  if (t.rpm) parts.push(t.rpm + " rpm");
  if (t.tpm) parts.push(t.tpm + " tpm");
  if (t.tokens_per_day) parts.push(t.tokens_per_day + " tok/day (" + (t.quota_on_exceeded || "block") + ")");
  return parts.length ? parts.join(" · ") : "—";
}

function fillTeamForm(t) {
  $("tf-name").value = t.name;
  $("tf-budget").value = t.budget_usd_micros ? String(t.budget_usd_micros / 1e6) : "";
  $("tf-rpm").value = t.rpm || "";
  $("tf-tpm").value = t.tpm || "";
  $("tf-tpd").value = t.tokens_per_day || "";
  $("tf-quota-exceeded").value = t.quota_on_exceeded || "";
  $("tf-budget-exceeded").value = t.budget_on_exceeded || "";
  $("tf-models").value = (t.allowed_models || []).join(", ");
}

// refreshTeamsView renders the team table (joined with spend when the
// analytics index is on) and the derived, read-only users table. The write
// form/actions are hidden for a non-admin identity — the server enforces via
// requireAdmin regardless (§9.1: this is a client-side hint, not the gate).
async function refreshTeamsView() {
  const content = $("teams-content");
  if (!capOn("teams_records")) { content.hidden = true; return; }
  content.hidden = false;
  $("team-form-card").hidden = !whoamiIsAdmin;

  let spendByTeam = {};
  if (capOn("analytics_index")) {
    try {
      const s = await api("GET", "/admin/analytics/summary", null, true);
      if (s && s !== DISABLED) {
        for (const r of s.by_team || []) spendByTeam[r.team] = r.cost_micros;
      }
    } catch { /* spend join is best-effort */ }
  }

  let teams;
  try { teams = await api("GET", "/admin/teams"); } catch { return; }
  const tbody = $("teams-table").querySelector("tbody");
  tbody.textContent = "";
  const rows = teams.data || [];
  if (!rows.length) {
    tbody.appendChild(emptyRow(5, "no teams yet"));
  } else {
    for (const t of rows) {
      const tr = document.createElement("tr");
      tr.append(td(t.name), td(t.source));
      tr.appendChild(td(t.name in spendByTeam ? usd(spendByTeam[t.name]) : "—"));
      tr.appendChild(td(t.source === "record" ? teamLimitsSummary(t) : "—"));
      const cell = document.createElement("td");
      if (whoamiIsAdmin && t.source === "record") {
        const edit = document.createElement("button");
        edit.className = "ghost"; edit.textContent = "edit";
        edit.addEventListener("click", () => fillTeamForm(t));
        const del = document.createElement("button");
        del.className = "ghost"; del.textContent = "✕";
        del.addEventListener("click", async () => {
          if (!confirm("Delete team record " + t.name + "?")) return;
          try { await api("DELETE", "/admin/teams/" + encodeURIComponent(t.name)); await refreshTeamsView(); }
          catch (err) { $("team-form-status").className = "status err"; $("team-form-status").textContent = String(err.message || err); }
        });
        cell.append(edit, del);
      }
      tr.appendChild(cell);
      tbody.appendChild(tr);
    }
  }

  let users;
  try { users = await api("GET", "/admin/users"); } catch { users = { data: [] }; }
  const ubody = $("users-table").querySelector("tbody");
  ubody.textContent = "";
  const urows = users.data || [];
  if (!urows.length) {
    ubody.appendChild(emptyRow(3, "no keys issued yet"));
  } else {
    for (const u of urows) {
      const tr = document.createElement("tr");
      tr.append(td(u.owner), td((u.teams || []).join(", ")), td(String(u.key_count)));
      ubody.appendChild(tr);
    }
  }
}

$("team-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const status = $("team-form-status");
  status.textContent = "";
  status.className = "status";
  try {
    const name = $("tf-name").value.trim();
    const body = {};
    const budget = $("tf-budget").value;
    if (budget) {
      // Same 53-bit-float precision guard as the key-issuance form.
      if (Number(budget) > 1e9) throw new Error("budget must be under $1,000,000,000");
      body.budget_usd_micros = Math.round(Number(budget) * 1e6);
    }
    if ($("tf-rpm").value) body.rpm = parseInt($("tf-rpm").value, 10);
    if ($("tf-tpm").value) body.tpm = parseInt($("tf-tpm").value, 10);
    if ($("tf-tpd").value) body.tokens_per_day = parseInt($("tf-tpd").value, 10);
    if ($("tf-quota-exceeded").value) body.quota_on_exceeded = $("tf-quota-exceeded").value;
    if ($("tf-budget-exceeded").value) body.budget_on_exceeded = $("tf-budget-exceeded").value;
    const models = $("tf-models").value.split(",").map((s) => s.trim()).filter(Boolean);
    if (models.length) body.allowed_models = models;
    await api("PUT", "/admin/teams/" + encodeURIComponent(name), body);
    status.textContent = "saved ✓ " + name;
    $("team-form").reset();
    await refreshTeamsView();
  } catch (err) {
    status.className = "status err";
    status.textContent = String(err.message || err);
  }
});

/* ---------- settings: connection quickstart snippets ---------- */

// renderUsage fills the snippets with this gateway's own origin and, once
// issued, the real virtual key — the page answers "how do I use this?" itself.
function renderUsage(key) {
  const origin = window.location.origin;
  const k = key || "<issue a key first>";
  $("usage-claude").textContent =
    "export ANTHROPIC_BASE_URL=" + origin + "\n" +
    "export ANTHROPIC_API_KEY=" + k + "\n" +
    "claude";
  $("usage-curl").textContent =
    "curl -X POST " + origin + "/v1/messages \\\n" +
    "  -H 'x-api-key: " + k + "' \\\n" +
    "  -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \\\n" +
    '  -d \'{"model":"<model>","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}\'';
  $("usage-openai").textContent =
    "base URL: " + origin + "/v1\n" +
    "api key:  " + k;
}

// loadModels lists routable models via the data plane using a just-issued
// virtual key. Through CloudFront both planes share one origin so the relative
// path works; on port-split (NLB-direct) setups it degrades to a hint.
async function loadModels(key) {
  try {
    const resp = await fetch("/v1/models", { headers: { "x-api-key": key } });
    if (!resp.ok) throw new Error(String(resp.status));
    const out = await resp.json();
    const names = (out.data || []).map((m) => m.id || m.name).filter(Boolean);
    if (names.length) $("usage-models").textContent = names.join(", ");
  } catch {
    $("usage-models").textContent = "see your gateway config (models map)";
  }
}

document.querySelectorAll(".copy-snippet").forEach((b) =>
  b.addEventListener("click", async () => {
    await navigator.clipboard.writeText($(b.dataset.target).textContent);
  }));


/* ---------- governance (quota gauges, budget spend, audit verify) ---------- */

// parseLabeled returns [{labels:{...}, value}] for every sample of metricName.
function parseLabeled(text, metricName) {
  const out = [];
  for (const line of text.split("\n")) {
    if (!line.startsWith(metricName + "{")) continue;
    const m = line.match(/\{(.*)\}\s+([0-9.eE+-]+)\s*$/);
    if (!m) continue;
    const labels = {};
    for (const kv of m[1].match(/(\w+)="([^"]*)"/g) || []) {
      const mm = kv.match(/(\w+)="([^"]*)"/);
      labels[mm[1]] = mm[2];
    }
    out.push({ labels, value: Number(m[2]) });
  }
  return out;
}

async function refreshGovernance() {
  let text = "";
  try {
    text = await (await fetch("/metrics")).text();
  } catch {
    return;
  }
  // quota utilization ratio gauge (team, window)
  const qbody = $("quota-table").querySelector("tbody");
  qbody.textContent = "";
  const quota = parseLabeled(text, "inferplane_quota_utilization_ratio");
  if (!quota.length) {
    qbody.appendChild(emptyRow(3, "no quota utilization reported yet"));
  } else {
    for (const q of quota.sort((a, b) => (a.labels.team || "").localeCompare(b.labels.team || ""))) {
      const tr = document.createElement("tr");
      tr.appendChild(td(q.labels.team || ""));
      tr.appendChild(td(q.labels.window || ""));
      const cell = document.createElement("td");
      const pct = Math.max(0, Math.min(1, q.value)) * 100;
      // <progress> needs no style — bulletproof under CSP style-src 'self'
      // (no .style writes, no inline style attribute).
      const bar = document.createElement("progress");
      bar.className = "bar";
      bar.max = 100;
      bar.value = pct;
      if (pct >= 90) bar.classList.add("over");
      else if (pct >= 75) bar.classList.add("warn");
      cell.appendChild(bar);
      const label = document.createElement("span");
      label.className = "bar-label";
      label.textContent = pct.toFixed(0) + "%";
      cell.appendChild(label);
      tr.appendChild(cell);
      qbody.appendChild(tr);
    }
  }
  // budget spend — CUMULATIVE counter since process start (honest label).
  const sbody = $("spend-table").querySelector("tbody");
  sbody.textContent = "";
  const spend = {};
  for (const s of parseLabeled(text, "inferplane_budget_spend_usd_total")) {
    const team = s.labels.team || "";
    spend[team] = (spend[team] || 0) + s.value;
  }
  const teams = Object.keys(spend).sort();
  if (!teams.length) {
    sbody.appendChild(emptyRow(2, "no spend reported yet"));
  } else {
    for (const team of teams) {
      const tr = document.createElement("tr");
      tr.appendChild(td(team));
      tr.appendChild(td("$" + spend[team].toFixed(6)));
      sbody.appendChild(tr);
    }
  }
  // budget-alert recent fires (D5b, ADR-017) — capability-gated, full-admin only.
  if (capOn("budget_alerts")) {
    const abody = $("alerts-table").querySelector("tbody");
    abody.textContent = "";
    try {
      const out = await api("GET", "/admin/alerts/recent", null, true);
      const fires = (out && out !== DISABLED) ? (out.fires || []) : [];
      if (!fires.length) {
        abody.appendChild(emptyRow(5, "no alerts fired yet"));
      } else {
        for (const f of fires) {
          const tr = document.createElement("tr");
          tr.appendChild(td(f.ts || ""));
          tr.appendChild(td(f.team || ""));
          tr.appendChild(td(((f.threshold || 0) * 100).toFixed(0) + "%"));
          tr.appendChild(td(((f.ratio || 0) * 100).toFixed(0) + "%"));
          tr.appendChild(td(f.delivered ? "yes" : ("no" + (f.error ? " (" + f.error + ")" : ""))));
          abody.appendChild(tr);
        }
      }
    } catch {
      abody.textContent = "";
      abody.appendChild(emptyRow(5, "failed to load"));
    }
  }
}

// Verify the audit chain via the token-gated admin API (NOT a bare fetch — it
// goes through api() so it carries the in-memory admin token and handles 401).
$("verify-audit").addEventListener("click", async () => {
  const box = $("verify-result");
  box.textContent = "verifying…";
  box.className = "";
  try {
    const out = await api("GET", "/admin/audit/verify");
    const sinks = out.sinks || [];
    if (!sinks.length) {
      box.textContent = "no file audit sink configured (stdout-only deployment)";
      return;
    }
    box.textContent = "";
    for (const s of sinks) {
      const line = document.createElement("div");
      line.className = "verify-line " + (s.ok ? "ok" : "err");
      if (s.ok) {
        line.textContent = "✓ " + s.path + " — chain OK (" + (s.records || 0) + " records)" +
          (s.partial_tail ? " [complete prefix; trailing partial line ignored]" : "");
      } else {
        line.textContent = "✗ " + s.path + " — " + (s.broken_at ? "BROKEN at record " + s.broken_at : (s.reason || "not OK"));
      }
      box.appendChild(line);
    }
  } catch (err) {
    box.className = "status err";
    box.textContent = String(err.message || err);
  }
});

/* ---------- logs + body drawer (D4, ADR-018) ---------- */

let logsCursor = null; // last-seen event id, for "load more" keyset pagination

async function refreshLogsView() {
  const content = $("logs-content");
  if (!capOn("analytics_index")) { content.hidden = true; return; }
  content.hidden = false;
  logsCursor = null;
  $("body-drawer").hidden = true;
  await loadLogsPage(null, true);
}

async function loadLogsPage(before, reset) {
  const tbody = $("logs-table").querySelector("tbody");
  let out;
  try {
    const qs = before ? ("?before=" + encodeURIComponent(before)) : "";
    out = await api("GET", "/admin/logs" + qs, null, true);
  } catch {
    return;
  }
  if (!out || out === DISABLED) return;
  const events = out.events || [];
  if (reset) tbody.textContent = "";
  if (!events.length) {
    if (reset) tbody.appendChild(emptyRow(8, "no requests logged yet"));
    $("logs-load-more").hidden = true;
    return;
  }
  for (const e of events) {
    const tr = document.createElement("tr");
    tr.appendChild(td(e.ts || ""));
    tr.appendChild(td(e.team || ""));
    tr.appendChild(td(e.model || ""));
    tr.appendChild(td(e.provider || ""));
    tr.appendChild(td(String(e.status || "")));
    tr.appendChild(td(String((e.input_tokens || 0) + (e.output_tokens || 0))));
    tr.appendChild(td(usd(e.cost_micros || 0)));
    const cell = document.createElement("td");
    if (e.body_ref) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "ghost";
      btn.textContent = "📄";
      btn.title = "view captured body";
      btn.addEventListener("click", () => openBodyDrawer(e.body_ref));
      cell.appendChild(btn);
    }
    tr.appendChild(cell);
    tbody.appendChild(tr);
  }
  logsCursor = events[events.length - 1].id;
  $("logs-load-more").hidden = false;
}

$("logs-load-more").addEventListener("click", () => loadLogsPage(logsCursor, false));

async function openBodyDrawer(ref) {
  const drawer = $("body-drawer");
  const box = $("body-drawer-content");
  box.textContent = "loading…";
  drawer.hidden = false;
  if (!capOn("logs_bodies")) {
    box.textContent = "body store not enabled";
    return;
  }
  try {
    const body = await api("GET", "/admin/bodies/" + encodeURIComponent(ref), null, true);
    if (body === DISABLED) { box.textContent = "body store not enabled"; return; }
    renderBodyDrawer(box, ref, body);
  } catch (err) {
    // A purged/erased/absent ref surfaces the server's tombstone message
    // (410, never a 500) — rendered here as plain text, no special-casing.
    box.textContent = String(err.message || err);
  }
}

function renderBodyDrawer(box, ref, body) {
  box.textContent = "";
  const meta = document.createElement("p");
  meta.className = "hint";
  meta.textContent = "record " + (body.record_id || "") + " · expires " + (body.expires_ts || "");
  box.appendChild(meta);

  const req = document.createElement("pre");
  req.textContent = "REQUEST:\n" + JSON.stringify(body.request, null, 2);
  box.appendChild(req);

  const resp = document.createElement("pre");
  resp.textContent = "RESPONSE:\n" + (body.response == null
    ? "(not captured — streaming responses are request-only)"
    : JSON.stringify(body.response, null, 2));
  box.appendChild(resp);

  const del = document.createElement("button");
  del.type = "button";
  del.className = "ghost";
  del.textContent = "DELETE BODY";
  del.addEventListener("click", async () => {
    if (!confirm("Permanently delete this body? This cannot be undone.")) return;
    try {
      await api("DELETE", "/admin/bodies/" + encodeURIComponent(ref));
      box.textContent = "";
      const done = document.createElement("p");
      done.textContent = "body deleted.";
      box.appendChild(done);
    } catch (err) {
      const errLine = document.createElement("p");
      errLine.className = "status err";
      errLine.textContent = "delete failed: " + (err.message || err);
      box.appendChild(errLine);
    }
  });
  box.appendChild(del);
}

$("body-drawer-close").addEventListener("click", () => { $("body-drawer").hidden = true; });

/* ---------- session ---------- */

$("token-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  adminToken = $("token").value;
  $("token").value = ""; // don't leave the token sitting in the input
  const status = $("auth-status");
  try {
    await refreshKeys();
    status.textContent = "";
    document.body.classList.remove("locked");
    $("shell").hidden = false;
    $("origin-chip").textContent = window.location.origin;
    renderUsage(lastIssuedKey || null);
    await loadWhoami(); // self-service identity + team scoping (ADR-010)
    await loadCapabilities(); // capability-driven section affordances (spec §9.1)
    showView("overview");
    pollHealth();
    setInterval(pollHealth, 15000);
  } catch (err) {
    adminToken = "";
    status.textContent = String(err.message || err);
    status.className = "status err";
  }
});

$("disconnect").addEventListener("click", () => {
  adminToken = "";
  lastIssuedKey = "";
  location.reload(); // wipes all page state, returns to the lock screen
});
