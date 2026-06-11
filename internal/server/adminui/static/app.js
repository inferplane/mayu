// inferplane admin key console (ADR-001). The admin token lives in this
// variable only — never persisted (no storage APIs, no cookies, no URL).
"use strict";

let adminToken = "";

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

async function refreshKeys() {
  const out = await api("GET", "/admin/keys");
  const tbody = $("keys").querySelector("tbody");
  tbody.textContent = "";
  for (const k of out.data || []) {
    const tr = document.createElement("tr");
    for (const v of [k.key_id, k.team, (k.allowed_models || []).join(", ")]) {
      const td = document.createElement("td");
      td.textContent = v; // textContent only — never markup from data
      tr.appendChild(td);
    }
    const td = document.createElement("td");
    const btn = document.createElement("button");
    btn.textContent = "revoke";
    btn.addEventListener("click", async () => {
      if (!confirm("Revoke " + k.key_id + "?")) return;
      await api("DELETE", "/admin/keys/" + encodeURIComponent(k.key_id));
      await refreshKeys();
    });
    td.appendChild(btn);
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

$("token-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  adminToken = $("token").value;
  $("token").value = ""; // don't leave the token sitting in the input
  const status = $("auth-status");
  try {
    await refreshKeys();
    status.textContent = "connected";
    status.className = "status ok";
    $("console").hidden = false;
  } catch (err) {
    adminToken = "";
    status.textContent = String(err.message || err);
    status.className = "status err";
    $("console").hidden = true;
  }
});

$("create-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const team = $("team").value.trim();
  const models = $("models").value.split(",").map((s) => s.trim()).filter(Boolean);
  const out = await api("POST", "/admin/keys", { team: team, allowed_models: models });
  // Plaintext is rendered once, kept only in the DOM until dismissed/reloaded.
  $("plaintext").textContent = out.plaintext;
  $("plaintext-box").hidden = false;
  await refreshKeys();
});

$("copy").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("plaintext").textContent);
});
