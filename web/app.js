// Sol control panel. Plain JS, no framework, no build step.
// Talks to /api on 127.0.0.1. The CSRF token comes from GET /api/status and
// must be sent on every state-changing request (see internal/api).
"use strict";

let csrf = "";

// esc makes any value safe to put inside HTML. Use it for EVERY value that
// came from the API: service and device names are user-controlled.
function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, c =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function showError(err) {
  const el = document.getElementById("error");
  el.textContent = err ? String(err.message || err) : "";
  el.hidden = !err;
}

async function api(path, opts = {}) {
  opts.headers = Object.assign({ "X-CSRF-Token": csrf }, opts.headers || {});
  const r = await fetch(path, opts);
  const ct = r.headers.get("content-type") || "";
  const body = ct.includes("json") ? await r.json() : await r.text();
  if (!r.ok) throw new Error((body && body.error) || `${path}: HTTP ${r.status}`);
  return body;
}

function json(method, body) {
  return { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) };
}

function ago(iso) {
  if (!iso || iso.startsWith("0001")) return "never";
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.round(s / 60) + " min ago";
  if (s < 86400) return Math.round(s / 3600) + " h ago";
  return new Date(iso).toLocaleDateString();
}

// --- navigation ---
document.querySelectorAll("nav button").forEach(b => {
  b.onclick = () => {
    document.querySelectorAll("nav button").forEach(x => x.classList.remove("active"));
    document.querySelectorAll(".screen").forEach(x => x.classList.remove("active"));
    b.classList.add("active");
    document.getElementById(b.dataset.screen).classList.add("active");
    refreshAll();
  };
});

// --- dashboard ---
async function refreshStatus() {
  const s = await api("/api/status");
  csrf = s.csrf || csrf;
  document.getElementById("node-id").textContent = s.node_id || "";
  document.getElementById("uptime").textContent = s.uptime || "-";
  document.getElementById("version").textContent = s.version || "-";
  const ts = s.transports || [];
  document.getElementById("transports").innerHTML = ts.length ? ts.map(t => {
    const node = (t.addresses || {}).node;
    return `<div class="card"><span class="dot ${esc(t.state)}"></span><b>${esc(t.name)}</b>
      <span class="muted">${esc(t.state)}</span>
      <div class="muted small">${esc(t.detail)}</div>
      ${node ? `<div class="mono small">${esc(node)}</div>` : ""}</div>`;
  }).join("") : `<p class="muted">No transports enabled. Edit config.json.</p>`;
}

async function refreshAudit() {
  const rs = await api("/api/audit");
  document.getElementById("audit-card").hidden = rs.length === 0;
  document.querySelector("#audit-table tbody").innerHTML = rs.map(r =>
    `<tr><td><span class="badge ${esc(r.status)}">${esc(r.status)}</span></td><td>${esc(r.service)}</td>
     <td>${esc(r.check)}</td><td class="small">${esc(r.detail)}</td></tr>`).join("");
}

// --- services ---
async function refreshServices() {
  const list = await api("/api/services");
  const tb = document.querySelector("#services-table tbody");
  tb.innerHTML = list.length ? list.map(s => {
    const addrs = Object.entries(s.addresses || {}).map(([k, v]) =>
      `<div class="mono small">${esc(k)}: ${esc(v)}</div>`).join("") || `<span class="muted">-</span>`;
    const opts = ["private", "public", "anonymous-public"].map(v =>
      `<option ${v === s.visibility ? "selected" : ""}>${v}</option>`).join("");
    return `<tr><td>${esc(s.name)}</td><td>${esc(s.local_port)}</td><td>${esc(s.protocol)}</td>
      <td><select data-vis="${esc(s.name)}">${opts}</select></td><td>${addrs}</td>
      <td><button class="danger" data-del="${esc(s.name)}">Remove</button></td></tr>`;
  }).join("") : `<tr><td colspan="6" class="muted">No services yet. Add one below.</td></tr>`;
  tb.querySelectorAll("[data-del]").forEach(b => b.onclick = () => {
    if (!confirm(`Remove ${b.dataset.del}? Open connections will be closed.`)) return;
    api("/api/services/" + encodeURIComponent(b.dataset.del), { method: "DELETE" })
      .then(refreshServices).catch(showError);
  });
  tb.querySelectorAll("[data-vis]").forEach(sel => sel.onchange = () =>
    api(`/api/services/${encodeURIComponent(sel.dataset.vis)}/visibility`, json("PUT", { visibility: sel.value }))
      .then(refreshAll).catch(showError));
}

document.getElementById("add-service").onsubmit = e => {
  e.preventDefault();
  const f = new FormData(e.target);
  api("/api/services", json("POST", {
    name: f.get("name"), local_port: Number(f.get("local_port")),
    protocol: f.get("protocol"), visibility: f.get("visibility"),
  })).then(() => { e.target.reset(); showError(null); refreshAll(); }).catch(showError);
};

// --- devices ---
async function refreshDevices() {
  const list = await api("/api/devices");
  const tb = document.querySelector("#devices-table tbody");
  tb.innerHTML = list.length ? list.map(d =>
    `<tr><td>${esc(d.name)}</td><td class="mono">${esc(d.id)}</td><td>${esc(ago(d.last_seen))}</td>
     <td><button class="danger" data-rev="${esc(d.id)}">Remove</button></td></tr>`).join("")
    : `<tr><td colspan="4" class="muted">No paired devices.</td></tr>`;
  tb.querySelectorAll("[data-rev]").forEach(b => b.onclick = () => {
    if (!confirm("Remove this device? Its access ends immediately.")) return;
    api("/api/devices/" + encodeURIComponent(b.dataset.rev), { method: "DELETE" })
      .then(refreshDevices).catch(showError);
  });
}

document.getElementById("pair-btn").onclick = async () => {
  try {
    const r = await api("/api/devices/pair", { method: "POST" });
    document.getElementById("pair-out").hidden = false;
    document.getElementById("pair-cmd").textContent = `sol connect '${r.link}'`;
    document.getElementById("pair-expires").textContent = new Date(r.expires).toLocaleTimeString();
    const qr = qrcode(0, "L");
    qr.addData(r.link);
    qr.make();
    document.getElementById("qr").innerHTML = qr.createSvgTag({ cellSize: 4, margin: 4, scalable: true });
    document.getElementById("copy-link").onclick = () => navigator.clipboard.writeText(r.link);
    showError(null);
  } catch (err) { showError(err); }
};

// --- files ---
async function refreshFiles() {
  const list = await api("/api/files");
  const ul = document.getElementById("file-list");
  ul.innerHTML = list.length ? list.map(f =>
    `<li><a href="/blob/${esc(f.hash)}" data-play="/blob/${esc(f.hash)}" class="mono">${esc(f.hash.slice(0, 16))}…</a>
     <span class="muted">${(f.size / 1048576).toFixed(2)} MB</span></li>`).join("")
    : `<li class="muted">No files yet.</li>`;
  ul.querySelectorAll("[data-play]").forEach(a => a.onclick = e => {
    e.preventDefault();
    const p = document.getElementById("player");
    p.src = a.dataset.play; p.hidden = false; p.play().catch(() => window.open(a.href));
  });
}

document.getElementById("upload").onsubmit = e => {
  e.preventDefault();
  api("/api/files", { method: "POST", body: new FormData(e.target) })
    .then(() => { e.target.reset(); refreshFiles(); }).catch(showError);
};

// --- logs ---
async function refreshLogs() {
  const list = await api("/api/events");
  document.getElementById("log-list").innerHTML = list.length ? list.map(e =>
    `<li><span class="mono">${esc(new Date(e.time).toLocaleTimeString())}</span>
     <span class="badge">${esc(e.kind)}</span> ${esc(e.message)}</li>`).join("")
    : `<li class="muted">Nothing yet.</li>`;
}

async function refreshAll() {
  try {
    await refreshStatus();
    await Promise.all([refreshServices(), refreshDevices(), refreshFiles(), refreshLogs(), refreshAudit()]);
  } catch (err) { showError(err); }
}

refreshAll();
setInterval(() => refreshStatus().catch(showError), 3000);
setInterval(() => Promise.all([refreshDevices(), refreshLogs()]).catch(showError), 10000);
