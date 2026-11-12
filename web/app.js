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
let state = { status: null, services: [], peers: [], tunnels: [], files: [], events: [], audit: [], settings: [] };
let onlyPeer = ""; // peers screen: show just this peer (picked in the sidebar)

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
  "private": "Only peers I choose",
  "public": "Anyone with Tor Browser",
  "anonymous-public": "Anonymous public (experimental)",
};

// ---------- look (settings) ----------

// The panel's look is a per-browser preference, so it lives in localStorage
// and never touches the node. Values are applied as attributes and CSS
// custom properties on <html>; style.css does the rest.
const lookDefaults = { theme: "dark", accent: "", layout: "side", size: "14" };
const lookAllowed = { theme: ["dark", "light", "system", "green", "amber"], layout: ["top", "side"], size: ["12", "14", "16", "18"] };
let look = Object.assign({}, lookDefaults);
try {
  const saved = JSON.parse(localStorage.getItem("sol-look") || "{}");
  for (const k of Object.keys(lookAllowed)) if (lookAllowed[k].includes(saved[k])) look[k] = saved[k];
  if (/^#[0-9a-f]{6}$/i.test(saved.accent || "")) look.accent = saved.accent;
} catch { /* storage blocked or corrupt: keep the defaults */ }

function applyLook() {
  const root = document.documentElement;
  root.dataset.theme = look.theme;
  root.dataset.layout = look.layout;
  root.style.setProperty("--size", look.size + "px");
  if (look.accent) root.style.setProperty("--accent", look.accent);
  else root.style.removeProperty("--accent");
}
function saveLook() {
  applyLook();
  try { localStorage.setItem("sol-look", JSON.stringify(look)); } catch { /* not saved; still applied */ }
}
function openSettings() {
  document.querySelectorAll("[data-set]").forEach(el => {
    el.value = look[el.dataset.set] ||
      (el.dataset.set === "accent" ? rgbToHex(getComputedStyle(document.documentElement).getPropertyValue("--accent")) : "");
  });
  $("dlg-settings").showModal();
}
// The colour input needs #rrggbb; the theme's accent is already written so.
const rgbToHex = v => (/^#[0-9a-f]{6}$/i.test(v.trim()) ? v.trim() : "#ff8c2b");
document.querySelectorAll("[data-set]").forEach(el => el.addEventListener("input", () => {
  look[el.dataset.set] = el.value;
  saveLook();
}));
applyLook();

// ---------- navigation ----------

const screens = ["home", "services", "peers", "files", "activity", "settings"];

function show(screen, peer = "") {
  if (!screens.includes(screen)) screen = "home";
  onlyPeer = screen === "peers" ? peer : "";
  if (location.hash.slice(1) !== screen) history.replaceState(null, "", "#" + screen);
  document.querySelectorAll(".nav button").forEach(b => b.classList.toggle("active", b.dataset.screen === screen && !onlyPeer));
  document.querySelectorAll(".screen").forEach(s => s.classList.toggle("active", s.id === screen));
  renderSidebar(); renderPeers();
  refresh();
}
document.querySelectorAll(".nav button").forEach(b => b.addEventListener("click", () => show(b.dataset.screen)));
window.addEventListener("hashchange", () => show(location.hash.slice(1)));

// Keys 1-6 switch screens, unless the user is typing or a dialog is open.
document.addEventListener("keydown", e => {
  if (e.ctrlKey || e.metaKey || e.altKey || document.querySelector("dialog[open]")) return;
  if (/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) return;
  const i = Number(e.key) - 1;
  if (screens[i]) show(screens[i]);
});

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
    "join": openJoinDialog,
    "rename": rename,
    "remove-service": () => removeService(arg),
    "unpair": () => unpair(arg),
    "send-file": () => pickFilesFor(arg),
    "refresh-peer": () => refreshPeer(arg),
    "open-tunnel": () => openTunnel(arg, t.dataset.svc),
    "close-tunnel": () => closeTunnel(arg),
    "copy-text": () => copy(arg),
    "show-qr": () => showQr(arg),
    "play": () => play(arg),
    "restart-tor": restartTor,
    "show-peer": () => show("peers", arg),
    "all-peers": () => show("peers"),
    "settings": openSettings,
    "reset-accent": () => { look.accent = ""; saveLook(); openSettings(); },
    "restart-node": restartNode,
    "save-setting": () => saveSetting(arg, document.querySelector(`[data-setting="${CSS.escape(arg)}"]`).value),
  })[a]?.();
});

// ---------- sidebar ----------

// How we last reached a peer, in two or three letters.
const pathShort = v => ({ "wg": "wg", "direct": "tls", "tor": "tor", "i2p": "i2p", "clearnet": "lan" })[v] || v;

function renderSidebar() {
  const s = state.status;
  if (!s) return;
  $("side-name").textContent = "~ " + s.name;
  $("side-version").textContent = "sol " + (s.version || "dev");
  const mark = { ready: "✓", starting: "…", failed: "✗", stopped: "-" };
  $("side-transports").innerHTML = (s.transports || []).map(t =>
    `<span class="chip ${esc(t.state)}" title="${esc(t.detail || t.state)}">[${esc(t.name)} ${mark[t.state] || "?"}]</span>`).join("");
  $("side-peers").innerHTML = state.peers.length ? state.peers.map(p => {
    const via = (p.connections.find(c => c.via) || {}).via;
    const path = !p.online ? "idle" : via ? pathShort(via) : "online";
    return `<button class="side-peer ${p.id === onlyPeer ? "active" : ""}" data-action="show-peer" data-arg="${esc(p.id)}"
      title="${esc(p.name)}"><span class="dot ${p.online ? "ready" : "stopped"}"></span><span class="name">${esc(p.name)}</span>
      <span class="path">${esc(path)}</span></button>`;
  }).join("") : `<p class="muted small side-none">none paired yet</p>`;
}

// ---------- home ----------

function torStatus() {
  const ts = (state.status && state.status.transports) || [];
  return ts.find(t => t.name === "tor") || ts[0];
}

function renderHome() {
  const s = state.status;
  if (!s) return;
  $("node-id").textContent = s.node_id;
  $("node-name").textContent = s.name;
  $("version").textContent = s.version || "dev";
  $("uptime").textContent = s.uptime;
  const as = s.autostart || {};
  $("autostart").innerHTML = !as.supported ? `<span class="muted">not on this system yet</span>`
    : as.enabled ? "Yes"
    : as.other ? `No <span class="muted small">· set up for another Sol data folder</span>`
    : `No <span class="muted small">· turn on with <code>sol autostart on</code></span>`;

  const ht = s.help_tor || { state: "off" };
  if (document.activeElement !== $("help-tor")) $("help-tor").checked = !!ht.enabled;
  $("help-tor-state").textContent = ht.state === "running" ? "on" : ht.enabled ? "on, not running" : "off";
  $("help-tor-state").className = ht.state === "running" ? "okline" : ht.enabled ? "warnline" : "";
  $("help-tor-detail").textContent = ht.detail || "";

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
      ? "Your peers can reach what you share with them, from anywhere. No open ports, no accounts."
      : "Next: add a service, or pair another machine to send files.";
  } else if (t.state === "starting") {
    cls = "warn";
    const d = t.detail || "";
    const m = /bootstrapping (\d+)%/.exec(d);
    if (m) {
      title = "Connecting to the Tor network…"; pct = Number(m[1]);
      text = "This takes about 30 seconds the first time.";
    } else if (/publishing/.test(d)) {
      title = "Almost there: publishing your address…"; pct = 95;
      text = "Tor is telling the network how to find you. Usually under a minute.";
    } else if (/reachable|lost contact/.test(d)) {
      title = "Your peers can't reach you right now";
      text = d.charAt(0).toUpperCase() + d.slice(1) + ". Sol keeps trying and reconnects by itself.";
    } else {
      title = "Reconnecting to Tor…"; pct = 10;
      text = d.charAt(0).toUpperCase() + d.slice(1) + ".";
    }
  } else {
    cls = "bad"; title = "Can't reach the Tor network";
    text = t.detail || "Tor stopped.";
  }
  pill.className = "pill " + cls;
  $("status-pill-text").textContent = cls === "ok" ? "online" + (t.name === "tor" ? " over tor" : "")
    : cls === "warn" ? "connecting" + (pct ? " " + pct + "%" : "") : "offline";
  $("hero").className = "hero " + cls;
  $("hero-title").textContent = title;
  $("hero-text").textContent = text;
  $("hero-progress").hidden = pct === null;
  // Reconnecting only makes sense on a node that runs Tor (tor can be off in
  // config.json, e.g. a LAN-only test node).
  const hasTor = (s.transports || []).some(x => x.name === "tor");
  $("hero-reconnect").hidden = !hasTor || cls === "ok" || pct !== null;
  $("tor-reconnect").hidden = !hasTor;
  if (pct !== null) $("hero-progress-bar").className = "w" + Math.round(pct / 5) * 5;

  // Checklist: hide once everything is done.
  const hasSvc = state.services.length > 0;
  const hasDev = state.peers.length > 0;
  const connected = state.peers.some(p => p.shares.some(s => s !== "_inbox"));
  $("step-service").classList.toggle("done", hasSvc);
  $("step-device").classList.toggle("done", hasDev);
  $("step-connect").classList.toggle("done", connected);
  $("checklist").hidden = hasSvc && hasDev && connected;

  // Peers and tunnels, as tables.
  const svcNames = p => p.services.filter(x => x.name !== "_inbox").map(x => svcLabel(x.name)).join(" ") || "-";
  $("live-list").innerHTML = `<table class="tbl">
    <tr><th>Name</th><th>State</th><th>Path</th><th>Shares with you</th><th>Last seen</th></tr>
    <tr><td>${esc(s.name)}*</td><td class="self">self</td><td>-</td><td>-</td><td>now</td></tr>
    ${state.peers.map(p => {
      const via = (p.connections.find(c => c.via) || {}).via;
      return `<tr class="click" data-action="show-peer" data-arg="${esc(p.id)}" title="Show ${esc(p.name)}">
        <td>${esc(p.name)}</td>
        <td class="${p.online ? "on" : "off"}">${p.online ? "connected ✓" : "idle"}</td>
        <td>${p.online ? esc(via ? viaLabel(via) : "connected to you") : "-"}</td>
        <td class="wrap">${esc(svcNames(p))}</td>
        <td>${p.online ? "now" : esc(ago(p.last_seen))}</td></tr>`;
    }).join("")}
  </table>${state.peers.length ? "" : `<p class="muted small">No peers yet. <button class="linkish" data-action="pair">Invite a machine</button>
    or <button class="linkish" data-action="join">join with a link</button>.</p>`}`;

  $("tunnels-card").hidden = state.tunnels.length === 0;
  $("tunnel-list").innerHTML = `<table class="tbl">
    <tr><th>Local address</th><th>Peer / service</th><th>Proto</th><th>Via</th><th>State</th><th></th></tr>
    ${state.tunnels.map(t => `<tr>
      <td>${t.url ? `<a href="${esc(t.url)}" target="_blank" rel="noopener noreferrer">${esc(t.addr)}</a>` : esc(t.addr)}</td>
      <td>${esc(peerName(t.peer))}/${esc(svcLabel(t.service))}</td>
      <td>${esc(t.protocol || "")}</td>
      <td>${esc(t.via ? pathShort(t.via) : "-")}</td>
      <td class="${!t.listening ? "" : t.last_error ? "off" : "on"}">${!t.listening ? `<span class="danger-text">port unavailable</span>`
        : t.last_error ? "peer unreachable" : "ready"}</td>
      <td><button class="linkish" data-action="copy-text" data-arg="${esc(t.addr)}">[copy]</button>
        <button class="linkish danger-text" data-action="close-tunnel" data-arg="${esc(String(t.port))}">[close]</button></td></tr>`).join("")}
  </table>`;

  $("transports").innerHTML = (s.transports || []).map(t => `
    <div class="row"><span class="dot ${esc(t.state)}"></span><b>${esc(t.name)}</b>
      <span class="muted">${esc(t.detail || t.state)}</span>
      ${t.addresses && t.addresses.node ? `<div class="mono small">${esc(t.addresses.node)}</div>` : ""}</div>`).join("");

  $("audit-card").hidden = state.audit.length === 0;
  $("audit-list").innerHTML = state.audit.map(r => `
    <div class="row"><span class="badge ${esc(r.status)}">${esc(r.status)}</span>
      <b>${esc(r.service)}</b> <span class="muted">${esc(r.check)}: ${esc(r.detail)}</span></div>`).join("");
}

async function restartTor() {
  try {
    await api("/api/transports/tor/restart", { method: "POST" });
    toast("Reconnecting to Tor. This takes about a minute.");
    refresh();
  } catch (err) { fail(err); }
}

$("help-tor").addEventListener("change", async e => {
  const on = e.target.checked;
  try {
    await api("/api/help-tor", json("PUT", { enabled: on }));
    toast(on ? "Helping the Tor network: Snowflake proxy on" : "Snowflake proxy off");
    refresh();
  } catch (err) { e.target.checked = !on; fail(err); }
});

// ---------- services ----------

function renderServices() {
  const list = state.services;
  $("service-list").innerHTML = list.length ? list.map(s => {
    const addrs = Object.entries(s.addresses || {});
    return `<div class="card service">
      <div class="card-head"><h3>${esc(s.name)}</h3>
        <button class="ghost small danger-text" data-action="remove-service" data-arg="${esc(s.name)}">Remove</button></div>
      ${s.dir
        ? (s.running
          ? `<p class="okline">● serving the folder <code>${esc(s.dir)}</code></p>`
          : `<p class="warnline">! The folder ${esc(s.dir)} can't be read.</p>`)
        : s.running
        ? `<p class="okline">● running on port ${esc(s.local_port)}</p>`
        : `<p class="warnline">! Nothing is running on port ${esc(s.local_port)}. Start the app, then check again.</p>`}
      ${s.record ? `<p class="muted small">Signed record at <code>/.well-known/sol.json</code> on each public address.</p>` : ""}
      <label class="inline">Who can open it
        <select data-vis="${esc(s.name)}">${Object.entries(visibilityLabel).map(([v, l]) =>
          `<option value="${v}" ${v === s.visibility ? "selected" : ""}>${l}</option>`).join("")}</select></label>
      ${s.active_streams ? `<p class="muted small">✓ ${s.active_streams} verified connection${s.active_streams === 1 ? "" : "s"} open</p>` : ""}
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
  { label: "Web app", hint: "on a port", name: "web", port: 8000, protocol: "http" },
  { label: "Website", hint: "a folder", name: "site", port: "", protocol: "http", site: true },
  { label: "Custom", hint: "anything else", name: "", port: "", protocol: "tcp" },
];

// The dialog adds an app (a port) or a site (a folder Sol serves itself).
function setSiteMode(on) {
  const f = $("service-form");
  $("field-port").hidden = on; $("field-dir").hidden = !on; $("field-protocol").hidden = on;
  f.local_port.required = !on; f.dir.required = on;
}

function openServiceDialog() {
  const f = $("service-form");
  f.reset();
  setSiteMode(false);
  $("service-error").hidden = true;
  $("templates").innerHTML = templates.map((t, i) =>
    `<button type="button" class="tpl" data-tpl="${i}"><b>${esc(t.label)}</b><span class="muted small">${esc(t.hint)}</span></button>`).join("");
  $("templates").querySelectorAll("[data-tpl]").forEach(b => b.addEventListener("click", () => {
    const t = templates[b.dataset.tpl];
    $("templates").querySelectorAll(".tpl").forEach(x => x.classList.toggle("active", x === b));
    let name = t.name, n = 2;
    while (name && state.services.some(s => s.name === name)) name = t.name + "-" + n++;
    f.name.value = name; f.local_port.value = t.port; f.protocol.value = t.protocol;
    setSiteMode(!!t.site);
    if (t.site) f.visibility.value = "public";
    (t.site ? f.dir : name ? f.local_port : f.name).focus();
  }));
  $("dlg-service").showModal();
}

$("service-form").addEventListener("submit", async e => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const f = e.target;
  try {
    const site = f.dir.required;
    await api("/api/services", json("POST", site
      ? { name: f.name.value, dir: f.dir.value.trim(), visibility: f.visibility.value, protocol: "http", record: f.record.checked }
      : { name: f.name.value, local_port: Number(f.local_port.value), visibility: f.visibility.value,
          protocol: f.protocol.value, record: f.protocol.value === "http" && f.record.checked }));
    $("dlg-service").close();
    toast(`Added ${f.name.value}`);
    show("services");
  } catch (err) {
    $("service-error").textContent = err.message;
    $("service-error").hidden = false;
  }
});

async function removeService(name) {
  if (!confirm(`Remove "${name}"? Peers using it are disconnected. The app itself is not touched.`)) return;
  try { await api("/api/services/" + encodeURIComponent(name), { method: "DELETE" }); toast(`Removed ${name}`); refresh(); }
  catch (err) { fail(err); }
}

function showQr(text) {
  $("qr-big").innerHTML = qrSvg(text);
  $("qr-text").textContent = text;
  $("dlg-qr").showModal();
}

// ---------- peers ----------

const builtinShares = [
  ["_inbox", "Can send you files", "Files it sends land in your Files tab."],
  ["_files", "Shared files", "Your shared-files page (files you added on the Files tab)."],
];
const svcLabel = name => ({ "_inbox": "inbox", "_files": "shared files" })[name] || name;
const peerName = id => (state.peers.find(p => p.id === id) || {}).name || id;

function shareBox(p, name, label, hint) {
  const on = p.shares.includes(name);
  return `<label class="share"><input type="checkbox" data-share="${esc(p.id)}" data-svc="${esc(name)}" ${on ? "checked" : ""}>
    <span><b>${esc(label)}</b>${hint ? `<span class="muted small"> ${esc(hint)}</span>` : ""}</span></label>`;
}

function renderPeers() {
  if (onlyPeer && !state.peers.some(p => p.id === onlyPeer)) onlyPeer = ""; // unpaired meanwhile
  const list = onlyPeer ? state.peers.filter(p => p.id === onlyPeer) : state.peers;
  const filter = onlyPeer ? `<p class="peer-filter"><button class="ghost small" data-action="all-peers">← all peers</button></p>` : "";
  $("peer-list").innerHTML = list.length ? filter + list.map(p => {
    const theirs = p.services.filter(s => s.name !== "_inbox");
    return `<div class="card device ${p.online ? "online" : ""}">
      <div class="card-head"><h3><span class="dot ${p.online ? "ready" : "stopped"}"></span>${esc(p.name)}</h3>
        <button class="ghost small danger-text" data-action="unpair" data-arg="${esc(p.id)}">Unpair</button></div>
      ${p.online
        ? `<p class="okline">✓ connected and verified</p>
           ${p.connections.map(c => `<p class="small">${c.outbound ? "You →" : "→ your"} <b>${esc(svcLabel(c.service))}</b>
             <span class="muted">· ${c.streams} connection${c.streams === 1 ? "" : "s"} · since ${esc(clock(c.since))}${c.via ? " · " + esc(viaLabel(c.via)) : ""}</span></p>`).join("")}`
        : `<p class="muted">Not connected · last seen ${esc(ago(p.last_seen))}</p>`}

      <h4>It shares with you</h4>
      ${theirs.length
        ? `<div class="theirs">${theirs.map(svc => theirService(p, svc)).join("")}</div>`
        : `<p class="muted small">Nothing yet (it can still receive files from you).</p>`}
      <button class="ghost small" data-action="refresh-peer" data-arg="${esc(p.id)}">Check again</button>

      <h4>You share with it</h4>
      <div class="shares">
        ${builtinShares.map(([n, l, h]) => shareBox(p, n, l, h)).join("")}
        ${state.services.map(s => shareBox(p, s.name, s.name, s.dir ? "website" : "port " + s.local_port)).join("")}
      </div>

      <button class="small" data-action="send-file" data-arg="${esc(p.id)}">Send a file…</button>

      <details class="small"><summary>How is this peer verified?</summary>
        <p class="muted">Every connection, in either direction, starts with a fresh random challenge that the
          other side must sign with its private key. Only the key paired here (fingerprint
          <code>${esc(p.id)}</code>) is accepted, and old signatures can't be replayed.
          ${p.locked.includes("tor") ? "It also holds a Tor access key made just for it, so without that key this computer's address can't even be found." : ""}</p>
        <p class="muted">Paired ${esc(new Date(p.added).toLocaleString())}.</p>
      </details>
    </div>`;
  }).join("")
  : `<div class="empty card"><p><b>No peers yet.</b></p>
      <p class="muted">Pair another computer that runs Sol. Then you can share apps with it and send files both ways.</p>
      <button data-action="pair">Invite a machine</button> <button class="ghost" data-action="join">Join with a link</button></div>`;
}

const viaLabel = v => ({ "tor": "via Tor", "wg": "direct (WireGuard)", "direct": "direct (TLS)", "clearnet": "LAN (unencrypted)", "i2p": "via I2P" })[v] || "via " + v;

// One service a peer shares with us: Open it, or show its open tunnel.
function theirService(p, svc) {
  const tun = state.tunnels.find(t => t.peer === p.id && t.service === svc.name);
  const label = esc(svcLabel(svc.name));
  if (!tun) {
    return `<div class="svcrow"><span class="tag">${label}</span>
      <button class="small" data-action="open-tunnel" data-arg="${esc(p.id)}" data-svc="${esc(svc.name)}">Open</button></div>`;
  }
  const status = !tun.listening ? `<span class="error">${esc(tun.last_error || "port unavailable")}</span>`
    : tun.last_error ? `<span class="warn-text">peer unreachable right now</span>`
    : tun.via ? `<span class="muted">${esc(viaLabel(tun.via))}</span>` : `<span class="muted">ready</span>`;
  const target = tun.url
    ? `<a href="${esc(tun.url)}" target="_blank" rel="noopener noreferrer">${esc(tun.addr)}</a>`
    : `<code>${esc(tun.addr)}</code> <button class="ghost small" data-action="copy-text" data-arg="${esc(tun.addr)}">Copy</button>`;
  return `<div class="svcrow"><span class="tag">${label}</span> ${target} ${status}
    ${tun.url ? `<button class="small" data-action="open-tunnel" data-arg="${esc(p.id)}" data-svc="${esc(svc.name)}">Open</button>` : ""}
    <button class="ghost small" data-action="close-tunnel" data-arg="${esc(String(tun.port))}">Close</button></div>`;
}

// Open a peer's service. The node keeps the tunnel (also after restarts),
// so this page can be closed. The tab is opened right away, inside the
// click, so popup blockers allow it; it's pointed at the tunnel once the
// node answers.
async function openTunnel(id, svc) {
  const known = (state.peers.find(p => p.id === id)?.services || []).find(s => s.name === svc);
  const tab = known && known.protocol === "http" ? window.open("about:blank", "_blank") : null;
  if (tab) tab.opener = null;
  try {
    const tun = await api(`/api/peers/${encodeURIComponent(id)}/services/${encodeURIComponent(svc)}/open`, { method: "POST" });
    if (tun.url && tab) tab.location.href = tun.url;
    else if (tab) tab.close();
    if (!tun.url) toast(`${svcLabel(svc)} is on ${tun.addr} (for apps that aren't web pages)`);
    refresh();
  } catch (err) { if (tab) tab.close(); fail(err); }
}

async function closeTunnel(port) {
  try { await api("/api/tunnels/" + encodeURIComponent(port), { method: "DELETE" }); toast("Tunnel closed"); refresh(); }
  catch (err) { fail(err); }
}


document.addEventListener("change", e => {
  const box = e.target.closest("[data-share]");
  if (!box) return;
  const id = box.dataset.share, svc = box.dataset.svc;
  api(`/api/peers/${encodeURIComponent(id)}/shares/${encodeURIComponent(svc)}`, json("PUT", { shared: box.checked }))
    .then(() => { toast(`${peerName(id)} ${box.checked ? "can now" : "can no longer"} open ${svcLabel(svc)}`); refresh(); })
    .catch(err => { box.checked = !box.checked; fail(err); });
});

async function unpair(id) {
  const name = peerName(id);
  if (!confirm(`Unpair from "${name}"? Both of you lose access to each other immediately, including open connections.`)) return;
  toast(`Unpairing from ${name}…`);
  try {
    const r = await api("/api/peers/" + encodeURIComponent(id), { method: "DELETE" });
    toast(r.notified ? `Unpaired. ${name} removed you too.` : `Removed ${name}. It couldn't be reached, but it can no longer reach you.`);
    refresh();
  } catch (err) { fail(err); }
}

async function refreshPeer(id) {
  toast(`Asking ${peerName(id)} what it shares…`);
  try { await api(`/api/peers/${encodeURIComponent(id)}/refresh`, { method: "POST" }); refresh(); }
  catch (err) { fail(err); }
}

let sendTo = "";
function pickFilesFor(id) {
  sendTo = id;
  $("send-input").value = "";
  $("send-input").click();
}
$("send-input").addEventListener("change", async e => {
  const id = sendTo, name = peerName(id);
  for (const file of e.target.files) {
    toast(`Sending ${file.name} to ${name}…`);
    const fd = new FormData();
    fd.append("file", file, file.name);
    try {
      await api(`/api/peers/${encodeURIComponent(id)}/send`, { method: "POST", body: fd });
      toast(`${file.name} delivered to ${name} and verified`);
    } catch (err) { fail(err); }
  }
  refresh();
});

async function rename() {
  const name = prompt("Name for this computer, as your peers see it:", state.status ? state.status.name : "");
  if (!name) return;
  try { await api("/api/name", json("PUT", { name })); toast(`This computer is now "${name}"`); refresh(); }
  catch (err) { fail(err); }
}

// Pairing wizard (invite).
const installCmds = {
  arch: "sudo pacman -S --needed tor go\ngo install github.com/apv022/sol/cmd/sol@latest\nexport PATH=\"$HOME/go/bin:$PATH\"\nsol",
  debian: "sudo apt update && sudo apt install -y tor golang\ngo install github.com/apv022/sol/cmd/sol@latest\nexport PATH=\"$HOME/go/bin:$PATH\"\nsol",
  mac: "brew install tor go\ngo install github.com/apv022/sol/cmd/sol@latest\nexport PATH=\"$HOME/go/bin:$PATH\"\nsol",
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
  $("pair-wait-title").textContent = "Waiting for the other machine…";
  $("pair-wait-text").textContent = "Keep this open. The first connection over Tor usually takes 30 seconds to 2 minutes.";
  $("dlg-pair").classList.remove("success");
  $("dlg-pair").showModal();

  const known = new Set(state.peers.map(p => p.id));
  try {
    const r = await api("/api/peers/invite", { method: "POST" });
    $("pair-cmd").textContent = `sol pair '${r.link}'`;
    $("qr").innerHTML = qrSvg(r.link);
    const expires = new Date(r.expires);
    clearInterval(pairTimer);
    pairTimer = setInterval(async () => {
      if (!$("dlg-pair").open) return clearInterval(pairTimer);
      const left = Math.max(0, Math.round((expires - Date.now()) / 1000));
      $("pair-countdown").textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
      try {
        const peers = await api("/api/peers");
        const fresh = peers.find(p => !known.has(p.id));
        if (fresh) {
          clearInterval(pairTimer);
          $("dlg-pair").classList.add("success");
          $("pair-wait-title").textContent = `✓ Paired with ${fresh.name}`;
          $("pair-wait-text").textContent = "You can now send each other files. Tick what you share with it on the Peers tab.";
          toast(`${fresh.name} is paired`);
          refresh();
        } else if (left === 0) {
          clearInterval(pairTimer);
          $("pair-wait-title").textContent = "This link expired";
          $("pair-wait-text").textContent = "Close this window and invite again to get a new link.";
        }
      } catch { /* keep polling */ }
    }, 2000);
  } catch (err) {
    $("pair-error").textContent = "Couldn't create a pairing link: " + err.message;
    $("pair-error").hidden = false;
  }
}
$("pair-close").addEventListener("click", () => { clearInterval(pairTimer); $("dlg-pair").close(); });

// Join (the other machine made the link).
function openJoinDialog() {
  $("join-form").reset();
  $("join-error").hidden = true;
  $("join-status").hidden = true;
  $("join-submit").disabled = false;
  $("dlg-join").showModal();
}
$("join-form").addEventListener("submit", async e => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const link = e.target.link.value.trim();
  $("join-error").hidden = true;
  $("join-status").textContent = link.includes("tor=")
    ? "Finding the other machine over Tor. This usually takes 30 seconds to 2 minutes…"
    : "Pairing…";
  $("join-status").hidden = false;
  $("join-submit").disabled = true;
  try {
    const p = await api("/api/peers/pair", json("POST", { link }));
    $("dlg-join").close();
    toast(`Paired with ${p.name}`);
    show("peers");
  } catch (err) {
    $("join-status").hidden = true;
    $("join-error").textContent = err.message;
    $("join-error").hidden = false;
    $("join-submit").disabled = false;
  }
});

// ---------- files ----------

const playable = /\.(mp3|flac|ogg|opus|wav|m4a|aac|mp4|webm)$/i;

function renderFiles() {
  $("file-list").innerHTML = state.files.length ? state.files.map(f => `
    <div class="file row">
      <div class="grow"><b>${esc(f.name || "(unnamed file)")}</b>
        ${f.from ? `<span class="tag">from ${esc(peerName(f.from))}</span>` : ""}
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

// ---------- settings (the node's) ----------

function renderSettings() {
  $("restart-banner").hidden = !(state.status && state.status.restart_needed);
  // Don't redraw under someone who is typing a value.
  if ($("settings-list").contains(document.activeElement)) return;
  $("settings-list").innerHTML = state.settings.map(o => `
    <div class="card setting">
      <div class="card-head"><h3>${esc(o.label)}</h3>
        ${o.restart ? `<span class="tag">needs restart</span>` : ""}</div>
      ${o.kind === "bool"
        ? `<label class="share"><input type="checkbox" data-setting-toggle="${esc(o.key)}" ${o.value === "on" ? "checked" : ""}>
            <span><b>${o.value === "on" ? "on" : "off"}</b></span></label>`
        : `<div class="copyrow"><input data-setting="${esc(o.key)}" value="${esc(o.value)}"
              ${o.kind === "port" ? `type="number" min="1" max="65535"` : `placeholder="(none)"`}>
            <button class="ghost small" data-action="save-setting" data-arg="${esc(o.key)}">Save</button></div>`}
      <p class="muted small">${esc(o.help)} <code>sol config ${esc(o.key)}</code></p>
    </div>`).join("");
}

async function saveSetting(key, value) {
  try {
    const r = await api("/api/settings/" + encodeURIComponent(key), json("PUT", { value }));
    toast(r.restart ? "Saved. Restart the node to apply it." : "Saved");
    document.activeElement && document.activeElement.blur();
    refresh();
  } catch (err) { fail(err); refresh(); }
}
document.addEventListener("change", e => {
  const box = e.target.closest("[data-setting-toggle]");
  if (box) saveSetting(box.dataset.settingToggle, box.checked ? "on" : "off");
});
document.addEventListener("keydown", e => {
  if (e.key === "Enter" && e.target.matches("[data-setting]")) saveSetting(e.target.dataset.setting, e.target.value);
});

async function restartNode() {
  if (!confirm("Restart the node now? Open connections drop for a few seconds.")) return;
  try { await api("/api/restart", { method: "POST" }); } catch (err) { return fail(err); }
  toast("Restarting…");
  // The panel may come back on another port if panel_port was changed.
  const port = (state.settings.find(o => o.key === "panel_port") || {}).value;
  const next = port && port !== location.port ? `${location.protocol}//${location.hostname}:${port}/#settings` : "";
  for (let i = 0; i < 40; i++) {
    await new Promise(r => setTimeout(r, 500));
    if (next) { location.href = next; return; }
    try { await api("/api/status"); toast("The node is back"); return refresh(); } catch { /* still restarting */ }
  }
  toast("The node didn't come back. Start it with `sol` in a terminal.", "bad");
}

// ---------- activity ----------

const eventStyle = {
  pair: ["ok", "paired"], connect: ["ok", "connected"], revoke: ["warn", "removed"], file: ["ok", "file"],
  reject: ["bad", "blocked"], service: ["", "service"], transport: ["", "network"], audit: ["warn", "audit"],
};
const logTime = iso => new Date(iso).toLocaleTimeString([], { hour12: false });
function renderEvents() {
  $("event-list").innerHTML = state.events.length ? state.events.map(e => {
    const [cls, label] = eventStyle[e.kind] || ["", e.kind];
    return `<li class="${cls}"><span class="ev-time" title="${esc(new Date(e.time).toLocaleString())}">${esc(logTime(e.time))}</span>
      <span class="ev-kind">${esc(label)}</span><span class="ev-msg">${esc(e.message)}</span></li>`;
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
    const [services, peers, tunnels, events, files, audit, settings] = await Promise.all([
      api("/api/services"), api("/api/peers"), api("/api/tunnels"), api("/api/events"), api("/api/files"), api("/api/audit"),
      api("/api/settings"),
    ]);
    state = { status: s, services, peers, tunnels, events, files, audit, settings };
    renderSidebar(); renderHome(); renderServices(); renderPeers(); renderFiles(); renderEvents(); renderSettings();
  } catch (err) {
    $("status-pill").className = "pill bad";
    $("status-pill-text").textContent = "panel can't reach sol";
  } finally {
    refreshing = false;
  }
}

// Don't re-render a <select> the user is about to change.
setInterval(() => { if (!document.querySelector("select:focus, input:focus, dialog[open]")) refresh(); }, 3000);
show(location.hash.slice(1) || "home");
