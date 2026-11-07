// Sol control panel. Plain JS, no framework, no build step.
//
// Talks to /api on 127.0.0.1 (see internal/api). The CSRF token comes from
// GET /api/status and is sent on every state-changing request.
//
// Rules: every value from the API goes through esc() before it is put into
// HTML (names are user-controlled), and no inline event handlers or inline
// styles are used (the Content-Security-Policy forbids them).
"use strict";

let csrf = "";
let state = { status: null, services: [], devices: [], files: [], events: [], audit: [] };

// ---------- helpers ----------

function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, c =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
const $ = id => document.getElementById(id);

async function api(path, opts = {}) {
  opts.headers = Object.assign({ "X-CSRF-Token": csrf }, opts.headers || {});
  const r = await fetch(path, opts);
  const ct = r.headers.get("content-type") || "";
  const body = ct.includes("json") ? await r.json() : await r.text();
  if (!r.ok) throw new Error((body && body.error) || `${path}: HTTP ${r.status}`);
  return body;
}
const json = (method, body) => ({ method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });

function toast(msg, kind = "ok") {
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.textContent = msg;
  $("toasts").appendChild(el);
  setTimeout(() => el.classList.add("hide"), 3500);
  setTimeout(() => el.remove(), 4000);
}
const fail = err => toast(String(err.message || err), "bad");

function ago(iso) {
  if (!iso || iso.startsWith("0001")) return "never";
  const s = Math.max(0, Math.round((Date.now() - new Date(iso)) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return s + " seconds ago";
  if (s < 3600) return Math.round(s / 60) + " min ago";
  if (s < 86400) return Math.round(s / 3600) + " h ago";
  return new Date(iso).toLocaleDateString();
}
const clock = iso => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
function size(n) {
  if (n < 1024) return n + " B";
  if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
  if (n < 1073741824) return (n / 1048576).toFixed(1) + " MB";
  return (n / 1073741824).toFixed(2) + " GB";
}
function qrSvg(text) {
  const q = qrcode(0, "L");
  q.addData(text);
  q.make();
  return q.createSvgTag({ cellSize: 4, margin: 4, scalable: true });
}
async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast("Copied"); }
  catch { toast("Couldn't copy. Select the text and copy it manually.", "bad"); }
}
const visibilityLabel = {
  "private": "Only my devices",
  "public": "Anyone with Tor Browser",
  "anonymous-public": "Anonymous public (experimental)",
};

// ---------- navigation ----------

function show(screen) {
  if (!document.getElementById(screen)?.classList.contains("screen")) screen = "home";
  if (location.hash.slice(1) !== screen) history.replaceState(null, "", "#" + screen);
  document.querySelectorAll(".tabs button").forEach(b => b.classList.toggle("active", b.dataset.screen === screen));
  document.querySelectorAll(".screen").forEach(s => s.classList.toggle("active", s.id === screen));
  refresh();
}
document.querySelectorAll(".tabs button").forEach(b => b.addEventListener("click", () => show(b.dataset.screen)));

// One delegated click handler for every [data-action], [data-copy], [data-close].
document.addEventListener("click", e => {
  const t = e.target.closest("[data-action],[data-copy],[data-close]");
  if (!t) return;
  if (t.dataset.copy) return copy($(t.dataset.copy).textContent);
  if (t.hasAttribute("data-close")) return t.closest("dialog").close();
  const a = t.dataset.action, arg = t.dataset.arg;
  ({
    "add-service": openServiceDialog,
    "pair": openPairDialog,
    "remove-service": () => removeService(arg),
    "remove-device": () => removeDevice(arg),
    "copy-text": () => copy(arg),
    "show-qr": () => showQr(arg),
    "play": () => play(arg),
  })[a]?.();
});

// ---------- home ----------

function torStatus() {
  const ts = (state.status && state.status.transports) || [];
  return ts.find(t => t.name === "tor") || ts[0];
}

function renderHome() {
  const s = state.status;
  if (!s) return;
  $("node-id").textContent = s.node_id;
  $("version").textContent = s.version || "dev";
  $("uptime").textContent = s.uptime;

  const t = torStatus();
  const pill = $("status-pill");
  let title, text, cls, pct = null;
  if (!t) {
    cls = "bad"; title = "No network enabled";
    text = "Turn on Tor in ~/.sol/config.json and restart Sol.";
  } else if (t.state === "ready") {
    cls = "ok";
    title = t.name === "tor" ? "You're online over Tor" : "You're online (" + t.name + ")";
    text = state.services.length
      ? "Your paired devices can reach your services from anywhere. No open ports, no accounts."
      : "Next: add a service so your devices have something to reach.";
  } else if (t.state === "starting") {
    cls = "warn"; title = "Connecting to the Tor network…";
    const m = /(\d+)%/.exec(t.detail || "");
    pct = m ? Number(m[1]) : 5;
    text = "This takes about 30 seconds the first time. " + (t.detail || "");
  } else {
    cls = "bad"; title = "Can't reach the Tor network";
    text = t.detail || "Tor stopped.";
  }
  pill.className = "pill " + cls;
  $("status-pill-text").textContent = cls === "ok" ? "Online" : cls === "warn" ? "Connecting" + (pct ? " " + pct + "%" : "") : "Offline";
  $("hero").className = "hero " + cls;
  $("hero-title").textContent = title;
  $("hero-text").textContent = text;
  $("hero-progress").hidden = pct === null;
  if (pct !== null) $("hero-progress-bar").className = "w" + Math.round(pct / 5) * 5;

  // Checklist: hide once everything is done.
  const hasSvc = state.services.length > 0;
  const hasDev = state.devices.length > 0;
  const connected = state.devices.some(d => d.online) ||
    state.events.some(e => e.kind === "connect");
  $("step-service").classList.toggle("done", hasSvc);
  $("step-device").classList.toggle("done", hasDev);
  $("step-connect").classList.toggle("done", connected);
  $("checklist").hidden = hasSvc && hasDev && connected;

  // Live connections.
  const live = state.devices.filter(d => d.online);
  $("live-list").innerHTML = live.length ? live.map(d => d.connections.map(c => `
    <div class="live">
      <span class="lock" title="Verified with this device's key">🔒</span>
      <div><b>${esc(d.name)}</b> → <b>${esc(c.service)}</b>
        <div class="muted small">Verified · ${c.streams} open connection${c.streams === 1 ? "" : "s"} · since ${esc(clock(c.since))}</div></div>
    </div>`).join("")).join("")
    : `<p class="muted">No devices are connected right now.</p>`;

  $("transports").innerHTML = (s.transports || []).map(t => `
    <div class="row"><span class="dot ${esc(t.state)}"></span><b>${esc(t.name)}</b>
      <span class="muted">${esc(t.detail || t.state)}</span>
      ${t.addresses && t.addresses.node ? `<div class="mono small">${esc(t.addresses.node)}</div>` : ""}</div>`).join("");

  $("audit-card").hidden = state.audit.length === 0;
  $("audit-list").innerHTML = state.audit.map(r => `
    <div class="row"><span class="badge ${esc(r.status)}">${esc(r.status)}</span>
      <b>${esc(r.service)}</b> <span class="muted">${esc(r.check)}: ${esc(r.detail)}</span></div>`).join("");
}

// ---------- services ----------

function renderServices() {
  const list = state.services;
  $("service-list").innerHTML = list.length ? list.map(s => {
    const addrs = Object.entries(s.addresses || {});
    return `<div class="card service">
      <div class="card-head"><h3>${esc(s.name)}</h3>
        <button class="ghost small danger-text" data-action="remove-service" data-arg="${esc(s.name)}">Remove</button></div>
      ${s.running
        ? `<p class="okline">● Running on port ${esc(s.local_port)}</p>`
        : `<p class="warnline">⚠ Nothing is running on port ${esc(s.local_port)}. Start the app, then check again.</p>`}
      <label class="inline">Who can open it
        <select data-vis="${esc(s.name)}">${Object.entries(visibilityLabel).map(([v, l]) =>
          `<option value="${v}" ${v === s.visibility ? "selected" : ""}>${l}</option>`).join("")}</select></label>
      ${s.active_streams ? `<p class="muted small">🔒 ${s.active_streams} verified connection${s.active_streams === 1 ? "" : "s"} open</p>` : ""}
      ${addrs.length ? addrs.map(([net, a]) => `
        <div class="addr"><span class="muted small">${esc(net)} address</span>
          <div class="copyrow"><code>${esc(a)}</code>
            <button class="ghost small" data-action="copy-text" data-arg="${esc(net === "tor" ? "http://" + a : a)}">Copy</button>
            ${net === "tor" ? `<button class="ghost small" data-action="show-qr" data-arg="http://${esc(a)}">QR</button>` : ""}</div></div>`).join("")
        : s.visibility !== "private" ? `<p class="muted small">Publishing address…</p>` : ""}
    </div>`;
  }).join("")
  : `<div class="empty card"><p><b>No services yet.</b></p>
      <p class="muted">A service is an app running on this computer: a music server, a photo library, a website.</p>
      <button data-action="add-service">Add your first service</button></div>`;

  document.querySelectorAll("[data-vis]").forEach(sel => sel.addEventListener("change", () =>
    api(`/api/services/${encodeURIComponent(sel.dataset.vis)}/visibility`, json("PUT", { visibility: sel.value }))
      .then(() => { toast(`${sel.dataset.vis}: ${visibilityLabel[sel.value]}`); refresh(); }).catch(fail)));
}

const templates = [
  { label: "Navidrome", hint: "music", name: "music", port: 4533, protocol: "http" },
  { label: "Jellyfin", hint: "movies & TV", name: "jellyfin", port: 8096, protocol: "http" },
  { label: "Immich", hint: "photos", name: "photos", port: 2283, protocol: "http" },
  { label: "Home Assistant", hint: "smart home", name: "home", port: 8123, protocol: "http" },
  { label: "Nextcloud", hint: "files", name: "cloud", port: 8080, protocol: "http" },
  { label: "SSH", hint: "terminal", name: "ssh", port: 22, protocol: "tcp" },
  { label: "Website", hint: "any web app", name: "site", port: 8000, protocol: "http" },
  { label: "Custom", hint: "anything else", name: "", port: "", protocol: "tcp" },
];

function openServiceDialog() {
  const f = $("service-form");
  f.reset();
  $("service-error").hidden = true;
  $("templates").innerHTML = templates.map((t, i) =>
    `<button type="button" class="tpl" data-tpl="${i}"><b>${esc(t.label)}</b><span class="muted small">${esc(t.hint)}</span></button>`).join("");
  $("templates").querySelectorAll("[data-tpl]").forEach(b => b.addEventListener("click", () => {
    const t = templates[b.dataset.tpl];
    $("templates").querySelectorAll(".tpl").forEach(x => x.classList.toggle("active", x === b));
    let name = t.name, n = 2;
    while (name && state.services.some(s => s.name === name)) name = t.name + "-" + n++;
    f.name.value = name; f.local_port.value = t.port; f.protocol.value = t.protocol;
    (name ? f.local_port : f.name).focus();
  }));
  $("dlg-service").showModal();
}

$("service-form").addEventListener("submit", async e => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const f = e.target;
  try {
    await api("/api/services", json("POST", {
      name: f.name.value, local_port: Number(f.local_port.value),
      visibility: f.visibility.value, protocol: f.protocol.value,
    }));
    $("dlg-service").close();
    toast(`Added ${f.name.value}`);
    show("services");
  } catch (err) {
    $("service-error").textContent = err.message;
    $("service-error").hidden = false;
  }
});

async function removeService(name) {
  if (!confirm(`Remove "${name}"? Devices using it are disconnected. The app itself is not touched.`)) return;
  try { await api("/api/services/" + encodeURIComponent(name), { method: "DELETE" }); toast(`Removed ${name}`); refresh(); }
  catch (err) { fail(err); }
}

function showQr(text) {
  $("qr-big").innerHTML = qrSvg(text);
  $("qr-text").textContent = text;
  $("dlg-qr").showModal();
}

// ---------- devices ----------

function renderDevices() {
  const list = state.devices;
  $("device-list").innerHTML = list.length ? list.map(d => `
    <div class="card device ${d.online ? "online" : ""}">
      <div class="card-head"><h3><span class="dot ${d.online ? "ready" : "stopped"}"></span>${esc(d.name)}</h3>
        <button class="ghost small danger-text" data-action="remove-device" data-arg="${esc(d.id)}">Remove</button></div>
      ${d.online
        ? `<p class="okline">🔒 Connected and verified</p>
           ${d.connections.map(c => `<p class="small">→ <b>${esc(c.service)}</b>
             <span class="muted">· ${c.streams} connection${c.streams === 1 ? "" : "s"} · since ${esc(clock(c.since))}</span></p>`).join("")}`
        : `<p class="muted">Not connected · last seen ${esc(ago(d.last_seen))}</p>`}
      <details class="small"><summary>How is this device verified?</summary>
        <p class="muted">Each connection starts with a fresh random challenge from this node, which the device
          must sign with its private key. Only the key paired here (fingerprint <code>${esc(d.key_fingerprint)}</code>)
          is accepted, and old signatures can't be replayed.
          ${d.tor_auth ? "It also holds a Tor access key, so without it the node's address can't even be found." : ""}</p>
        <p class="muted">Paired ${esc(new Date(d.added).toLocaleString())}.</p>
      </details>
    </div>`).join("")
  : `<div class="empty card"><p><b>No devices paired yet.</b></p>
      <p class="muted">Pair a laptop or desktop to reach your services from anywhere.</p>
      <button data-action="pair">Pair a device</button></div>`;
}

async function removeDevice(id) {
  const d = state.devices.find(x => x.id === id);
  if (!confirm(`Remove "${d ? d.name : id}"? Its access ends immediately, including open connections.`)) return;
  try { await api("/api/devices/" + encodeURIComponent(id), { method: "DELETE" }); toast("Device removed"); refresh(); }
  catch (err) { fail(err); }
}

// Pairing wizard.
const installCmds = {
  arch: "sudo pacman -S --needed tor go\ngo install github.com/apv022/sol/cmd/sol@latest\nexport PATH=\"$HOME/go/bin:$PATH\"",
  debian: "sudo apt update && sudo apt install -y tor golang\ngo install github.com/apv022/sol/cmd/sol@latest\nexport PATH=\"$HOME/go/bin:$PATH\"",
  mac: "brew install tor go\ngo install github.com/apv022/sol/cmd/sol@latest\nexport PATH=\"$HOME/go/bin:$PATH\"",
};
let pairTimer = null;

function setOS(os) {
  document.querySelectorAll("[data-os]").forEach(b => b.classList.toggle("active", b.dataset.os === os));
  $("install-cmd").textContent = installCmds[os];
}
document.querySelectorAll("[data-os]").forEach(b => b.addEventListener("click", () => setOS(b.dataset.os)));

async function openPairDialog() {
  setOS(/Mac/.test(navigator.platform) ? "mac" : "arch");
  $("pair-error").hidden = true;
  $("pair-cmd").textContent = "Creating a link…";
  $("qr").innerHTML = "";
  $("pair-wait-title").textContent = "Waiting for your device…";
  $("pair-wait-text").textContent = "Keep this open. The first connection over Tor usually takes 30 seconds to 2 minutes.";
  $("dlg-pair").classList.remove("success");
  $("dlg-pair").showModal();

  const known = new Set(state.devices.map(d => d.id));
  try {
    const r = await api("/api/devices/pair", { method: "POST" });
    $("pair-cmd").textContent = `sol connect '${r.link}'`;
    $("qr").innerHTML = qrSvg(r.link);
    const expires = new Date(r.expires);
    clearInterval(pairTimer);
    pairTimer = setInterval(async () => {
      if (!$("dlg-pair").open) return clearInterval(pairTimer);
      const left = Math.max(0, Math.round((expires - Date.now()) / 1000));
      $("pair-countdown").textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
      try {
        const devs = await api("/api/devices");
        const fresh = devs.find(d => !known.has(d.id));
        if (fresh) {
          clearInterval(pairTimer);
          $("dlg-pair").classList.add("success");
          $("pair-wait-title").textContent = `✓ Paired with ${fresh.name}`;
          $("pair-wait-text").textContent = "On that computer, open the address sol connect printed (usually http://127.0.0.1:9000).";
          toast(`${fresh.name} is paired`);
          refresh();
        } else if (left === 0) {
          clearInterval(pairTimer);
          $("pair-wait-title").textContent = "This link expired";
          $("pair-wait-text").textContent = "Close this window and pair again to get a new link.";
        }
      } catch { /* keep polling */ }
    }, 2000);
  } catch (err) {
    $("pair-error").textContent = "Couldn't create a pairing link: " + err.message;
    $("pair-error").hidden = false;
  }
}
$("pair-close").addEventListener("click", () => { clearInterval(pairTimer); $("dlg-pair").close(); });

// ---------- files ----------

const playable = /\.(mp3|flac|ogg|opus|wav|m4a|aac|mp4|webm)$/i;

function renderFiles() {
  $("file-list").innerHTML = state.files.length ? state.files.map(f => `
    <div class="file row">
      <div class="grow"><b>${esc(f.name || "(unnamed file)")}</b>
        <div class="muted small">${esc(size(f.size))} · ${esc(ago(f.modified))} · <code title="${esc(f.hash)}">${esc(f.hash.slice(0, 12))}…</code></div></div>
      ${playable.test(f.name || "") ? `<button class="ghost small" data-action="play" data-arg="${esc(f.hash)}">Play</button>` : ""}
      <a class="button ghost small" href="/blob/${esc(f.hash)}" download="${esc(f.name || f.hash)}">Download</a>
      <button class="ghost small" data-action="copy-text" data-arg="${esc(f.hash)}">Copy hash</button>
    </div>`).join("")
    : `<p class="muted">No files yet.</p>`;
}

function play(hash) {
  const p = $("player");
  p.src = "/blob/" + hash;
  p.hidden = false;
  p.play().catch(fail);
}

async function upload(files) {
  for (const file of files) {
    $("upload-status").textContent = `Uploading ${file.name}…`;
    const fd = new FormData();
    fd.append("file", file, file.name);
    try { await api("/api/files", { method: "POST", body: fd }); toast(`Stored ${file.name}`); }
    catch (err) { fail(err); }
  }
  $("upload-status").textContent = "";
  refresh();
}
$("file-input").addEventListener("change", e => upload(e.target.files));
const dz = $("dropzone");
["dragenter", "dragover"].forEach(ev => dz.addEventListener(ev, e => { e.preventDefault(); dz.classList.add("over"); }));
["dragleave", "drop"].forEach(ev => dz.addEventListener(ev, e => { e.preventDefault(); dz.classList.remove("over"); }));
dz.addEventListener("drop", e => upload(e.dataTransfer.files));

// ---------- activity ----------

const eventStyle = {
  pair: ["✓", "ok", "Paired"], connect: ["🔒", "ok", "Connected"], revoke: ["✕", "warn", "Removed"],
  reject: ["⛔", "bad", "Blocked"], service: ["●", "", "Service"], transport: ["⇄", "", "Network"], audit: ["!", "warn", "Audit"],
};
function renderEvents() {
  $("event-list").innerHTML = state.events.length ? state.events.map(e => {
    const [icon, cls, label] = eventStyle[e.kind] || ["•", "", e.kind];
    return `<li class="${cls}"><span class="ev-icon">${icon}</span>
      <div><b>${esc(label)}</b> ${esc(e.message)}<div class="muted small">${esc(ago(e.time))}</div></div></li>`;
  }).join("") : `<li class="muted">Nothing has happened yet.</li>`;
}

// ---------- refresh loop ----------

let refreshing = false;
async function refresh() {
  if (refreshing) return;
  refreshing = true;
  try {
    const s = await api("/api/status");
    csrf = s.csrf || csrf;
    const [services, devices, events, files, audit] = await Promise.all([
      api("/api/services"), api("/api/devices"), api("/api/events"), api("/api/files"), api("/api/audit"),
    ]);
    state = { status: s, services, devices, events, files, audit };
    renderHome(); renderServices(); renderDevices(); renderFiles(); renderEvents();
  } catch (err) {
    $("status-pill").className = "pill bad";
    $("status-pill-text").textContent = "Panel can't reach Sol";
  } finally {
    refreshing = false;
  }
}

// Don't re-render a <select> the user is about to change.
setInterval(() => { if (!document.querySelector("select:focus, dialog[open] form")) refresh(); }, 3000);
show(location.hash.slice(1) || "home");
