// inferplane control plane (ADR-001/ADR-002). The admin token lives in this
// variable only — never persisted (no storage APIs, no cookies, no URL).
// All rendering is textContent-only; data never becomes markup.
"use strict";

let adminToken = "";
let lastIssuedKey = ""; // shown-once plaintext, page-lifetime only

const $ = (id) => document.getElementById(id);

async function api(method, path, body) {
  const resp = await fetch(path, {
    method,
    headers: {
      "Authorization": "Bearer " + adminToken,
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) throw new Error("unauthorized — check the admin token");
  if (!resp.ok && resp.status !== 204) throw new Error("API error " + resp.status);
  return resp.status === 204 ? null : resp.json();
}

/* ---------- views ---------- */

const VIEWS = { overview: "Overview", keys: "Virtual keys", quickstart: "Quickstart" };

function showView(name) {
  for (const v of Object.keys(VIEWS)) $("view-" + v).hidden = v !== name;
  document.querySelectorAll("button.nav").forEach((b) =>
    b.classList.toggle("active", b.dataset.view === name));
  $("view-title").textContent = VIEWS[name];
  if (name === "overview") refreshOverview();
}

document.querySelectorAll("[data-view]").forEach((b) =>
  b.addEventListener("click", () => showView(b.dataset.view)));

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

/* ---------- keys ---------- */

async function refreshKeys() {
  const out = await api("GET", "/admin/keys");
  const tbody = $("keys").querySelector("tbody");
  tbody.textContent = "";
  for (const k of out.data || []) {
    const tr = document.createElement("tr");
    for (const v of [k.key_id, k.team, (k.allowed_models || []).join(", ")]) {
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

$("create-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const team = $("team").value.trim();
  const models = $("models").value.split(",").map((s) => s.trim()).filter(Boolean);
  const out = await api("POST", "/admin/keys", { team: team, allowed_models: models });
  // Plaintext is rendered once, kept only in the DOM/page until reload.
  lastIssuedKey = out.plaintext;
  $("plaintext").textContent = out.plaintext;
  $("plaintext-box").hidden = false;
  renderUsage(out.plaintext);
  loadModels(out.plaintext);
  await refreshKeys();
});

$("copy").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("plaintext").textContent);
});

/* ---------- quickstart ---------- */

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
