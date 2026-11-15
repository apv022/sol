// Sol control panel. Plain JS, no framework, no build step.
//
// Talks to /api on 127.0.0.1 (see internal/api). The CSRF token comes from
// GET /api/status and is sent on every state-changing request.
//
// Rules: every value from the API goes through esc() before it is put into
// HTML (names are user-controlled), and no inline event handlers or inline
// styles are used (the Content-Security-Policy forbids them). The same page
// is the whole UI of the Android app, a WebView whose user agent contains
// "SolAndroid/<version>"; see `android` below.
"use strict";

let csrf = "";
let state = { status: null, services: [], peers: [], tunnels: [], files: [], events: [], audit: [], settings: [] };
let loaded = false;  // the first refresh has finished
let onlyPeer = "";   // peers screen: show just this peer (picked in the sidebar or on Home)
let logFilter = "all";
const notes = {};    // setting key -> sentences from its last impact, shown under the control
const busy = {};     // setting key -> a change is in flight

// The Android app. Downloads stay plain <a href=/blob/... download> links
// (the app hands those to the system's download manager); never blob: URLs
// or window.open.
const android = /SolAndroid\//.test(navigator.userAgent);
if (android) document.documentElement.classList.add("android");

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
  setTimeout(() => el.classList.add("hide"), 3800);
  setTimeout(() => el.remove(), 4300);
}
const fail = err => toast(String(err.message || err), "bad");
const plural = (n, w) => `${n} ${w}${n === 1 ? "" : "s"}`;
const zeroTime = iso => !iso || iso.startsWith("0001");

function ago(iso) {
  if (zeroTime(iso)) return "never";
  const s = Math.max(0, Math.round((Date.now() - new Date(iso)) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return s + " seconds ago";
  if (s < 3600) return Math.round(s / 60) + " min ago";
  if (s < 86400) return Math.round(s / 3600) + " h ago";
  return new Date(iso).toLocaleDateString();
}
// A duration in seconds, as a clock: "0:42", "12:05", "1:02:05".
function clockDur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec / 60) % 60, s = String(sec % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
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

// Elements that count: [data-since] shows the time since an instant,
// [data-until] the time left until one. One ticker updates them all, so a
// wait is visibly moving between refreshes.
function tick() {
  const now = Date.now();
  document.querySelectorAll("[data-since]").forEach(el => {
    el.textContent = clockDur((now - new Date(el.dataset.since)) / 1000);
  });
  document.querySelectorAll("[data-until]").forEach(el => {
    const left = (new Date(el.dataset.until) - now) / 1000;
    el.textContent = left > 0 ? (left < 90 ? Math.ceil(left) + " s" : clockDur(left)) : "a moment";
  });
}
setInterval(tick, 1000);
const sinceSpan = iso => zeroTime(iso) ? "" : `<span data-since="${esc(iso)}">${esc(clockDur((Date.now() - new Date(iso)) / 1000))}</span>`;
const untilSpan = iso => `<span data-until="${esc(iso)}">a moment</span>`;

// ---------- the ask dialog (instead of confirm and prompt) ----------

// ask shows a question and resolves to true (or the typed text) when the user
// agrees, null otherwise. html must already be escaped. run, if given, does
// the work before the dialog closes; if it throws, the error stays shown in
// the dialog so the user can correct the value or cancel.
function ask({ title, html = "", yes = "OK", danger = false, input = null, run = null }) {
  return new Promise(resolve => {
    const d = $("dlg-ask"), form = $("ask-form"), yesBtn = $("ask-yes");
    $("ask-title").textContent = title;
    $("ask-body").innerHTML = html;
    $("ask-field").hidden = !input;
    if (input) { $("ask-label").textContent = input.label; $("ask-input").value = input.value || ""; }
    $("ask-error").hidden = true;
    yesBtn.textContent = yes;
    yesBtn.disabled = false;
    yesBtn.classList.toggle("danger", danger);
    let done = false;
    form.onsubmit = async e => {
      e.preventDefault();
      const v = input ? $("ask-input").value.trim() : true;
      if (input && !v) return;
      if (run) {
        yesBtn.disabled = true;
        try { await run(v); }
        catch (err) {
          $("ask-error").textContent = err.message || String(err);
          $("ask-error").hidden = false;
          yesBtn.disabled = false;
          return;
        }
      }
      done = true;
      d.close();
      resolve(v);
    };
    d.onclose = () => { if (!done) { done = true; resolve(null); } };
    d.showModal();
    if (input) $("ask-input").focus();
  });
}

// ---------- look (appearance) ----------

// The panel's look is a per-browser preference, so it lives in localStorage
// and never touches the node. Values are applied as attributes and CSS
// custom properties on <html>; style.css does the rest.
const lookDefaults = { theme: "system", accent: "", layout: "side", size: "14" };
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
  // The browser (and Android) colour the system bars from theme-color.
  const bg = getComputedStyle(root).getPropertyValue("--bg").trim();
  if (bg) document.querySelector('meta[name="theme-color"]').content = bg;
}
function saveLook() {
  applyLook();
  try { localStorage.setItem("sol-look", JSON.stringify(look)); } catch { /* not saved; still applied */ }
}
function openAppearance() {
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
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyLook);
applyLook();

// ---------- paths: how a connection travels ----------

// Every transport in plain words: a short name and what it means for speed
// and for who sees your IP address. Shown wherever a connection is.
const paths = {
  tor: { label: "Tor", cls: "tor",
    means: "Through the Tor network: private and works behind any router, but slower. Neither machine sees the other's IP address." },
  wg: { label: "WireGuard direct", cls: "fast",
    means: "Straight between the two machines over WireGuard (UDP), encrypted. The fastest path. Each machine sees the other's IP address." },
  direct: { label: "TLS direct", cls: "fast",
    means: "Straight between the two machines over TLS (TCP), encrypted. Fast, a little slower than WireGuard. Each machine sees the other's IP address." },
  clearnet: { label: "LAN, unencrypted", cls: "lan",
    means: "Plain TCP on the local network: fast, but not encrypted, so anyone on that network can read it." },
  i2p: { label: "I2P", cls: "tor", means: "Through the I2P network." },
};
const pathOf = v => paths[v] || { label: v, cls: "", means: "" };
const pathShort = v => ({ wg: "wg", direct: "tls", tor: "tor", i2p: "i2p", clearnet: "lan" })[v] || v;
function viaBadge(via, extra = "") {
  if (!via) return `<span class="via none">not connected yet</span>`;
  const p = pathOf(via);
  return `<span class="via ${esc(p.cls)}" title="${esc(p.means)}">${esc(p.label)}${extra ? ` <span class="via-x">${esc(extra)}</span>` : ""}</span>`;
}

// The path block of a peer: which way its connections go, what that means,
// and, when direct mode could be faster but isn't carrying them, why not and
// what fixes it (PeerView.path, worked out by the node).
function pathBlock(p) {
  const path = p.path || {};
  let html;
  if (path.via) {
    html = `<div class="path-line">${viaBadge(path.via, path.live ? "in use" : "last used")}</div>
      <p class="muted small">${esc(pathOf(path.via).means)}</p>`;
  } else {
    html = `<div class="path-line">${viaBadge("")}</div>
      <p class="muted small">This machine hasn't connected to ${esc(p.name)} since it started.${path.next
        ? ` The next connection tries ${esc(pathOf(path.next).label)} first.` : ""}</p>`;
  }
  if (path.down) {
    const redial = path.redial_in ? new Date(Date.now() + path.redial_in * 1000).toISOString() : "";
    html += `<div class="why"><b>Not reachable right now</b>
      <p>${esc(p.name)} didn't answer. Sol keeps trying${redial ? `; next try in ${untilSpan(redial)}` : ""}.</p></div>`;
  }
  if (path.reason && path.detail) {
    const tip = path.reason === "direct_off"; // nothing is broken; a faster option exists
    const until = path.retry_in ? new Date(Date.now() + path.retry_in * 1000).toISOString() : "";
    html += `<div class="why ${tip ? "tip" : ""}">
      <b>${tip ? "Want it faster?" : "Why not direct?"}</b>
      <p>${esc(path.detail)}${until ? ` ${path.reason === "direct_probation" ? "Back to direct in" : "Next direct check in"} ${untilSpan(until)}.` : ""}</p>
      ${path.fix ? `<p class="fix">${esc(path.fix)}</p>` : ""}
      <div class="actions start">
        ${/direct_off|direct_restart|direct_failed/.test(path.reason) ? `<button class="ghost small" data-screen-link="settings">Connection settings</button>` : ""}
        ${/peer_no_direct|direct_backoff|direct_probation|direct_next/.test(path.reason) ? `<button class="ghost small" data-action="refresh-peer" data-arg="${esc(p.id)}">Check again</button>` : ""}
      </div></div>`;
  }
  return html;
}

// ---------- navigation ----------

const screens = ["home", "peers", "services", "files", "activity", "settings"];
const screenTitle = { home: "Sol", peers: "Peers", services: "Services", files: "Files", activity: "Log", settings: "Settings" };
let current = "home";

function show(screen, peer = "") {
  if (!screens.includes(screen)) screen = "home";
  const changed = screen !== current;
  current = screen;
  onlyPeer = screen === "peers" ? peer : "";
  if (location.hash.slice(1) !== screen) history.replaceState(null, "", "#" + screen);
  document.querySelectorAll(".nav button, .tabbar button").forEach(b => {
    const on = b.dataset.screen === screen && !(onlyPeer && b.closest(".nav"));
    b.classList.toggle("active", on);
    if (on) b.setAttribute("aria-current", "page"); else b.removeAttribute("aria-current");
  });
  document.querySelectorAll(".screen").forEach(s => s.classList.toggle("active", s.id === screen));
  $("topbar-title").textContent = screenTitle[screen];
  $("topbar-back").hidden = screen !== "activity"; // the only screen not on the tab bar
  $("topbar-sun").hidden = screen === "activity";
  if (changed) window.scrollTo(0, 0);
  renderSidebar(); renderPeers();
  refresh();
}

// A sol://pair link handed over by the Android app (or any link to
// /#pair=<url-encoded link>). It is untrusted: it is only ever put into the
// join form, and the user still has to press Pair.
function takePairHash() {
  const h = location.hash;
  if (!h.startsWith("#pair=")) return false;
  let link = "";
  try { link = decodeURIComponent(h.slice(6)).trim(); } catch { link = ""; }
  history.replaceState(null, "", location.pathname + location.search);
  show("peers");
  if (link.length > 4096 || !link.startsWith("sol://") || /[\u0000-\u001f\u007f]/.test(link)) {
    toast("That pairing link isn't valid.", "bad");
    return true;
  }
  openJoinDialog(link);
  return true;
}
window.addEventListener("hashchange", () => { if (!takePairHash()) show(location.hash.slice(1)); });

// Keys 1-6 switch screens, unless the user is typing or a dialog is open.
document.addEventListener("keydown", e => {
  if (e.ctrlKey || e.metaKey || e.altKey || document.querySelector("dialog[open]")) return;
  if (/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) return;
  const i = Number(e.key) - 1;
  if (screens[i]) show(screens[i]);
});

// One delegated click handler for every [data-screen], [data-action],
// [data-copy] and [data-close].
document.addEventListener("click", e => {
  if (e.target instanceof HTMLDialogElement) return e.target.close(); // a tap on the backdrop
  const t = e.target.closest("[data-screen],[data-screen-link],[data-action],[data-copy],[data-close]");
  if (!t) return;
  if (t.dataset.screen || t.dataset.screenLink) {
    t.closest("dialog")?.close();
    return show(t.dataset.screen || t.dataset.screenLink);
  }
  if (t.dataset.copy) return copy($(t.dataset.copy).textContent);
  if (t.hasAttribute("data-close")) return t.closest("dialog").close();
  const a = t.dataset.action, arg = t.dataset.arg;
  ({
    "go-home": () => show("home"),
    "add-service": openServiceDialog,
    "pair": openPairDialog,
    "join": () => openJoinDialog(),
    "join-paste": joinPaste,
    "join-cancel": joinCancel,
    "rename": rename,
    "copy-node-id": () => copy(state.status ? state.status.node_id : ""),
    "copy-pair-cmd": () => copy(`sol pair '${$("pair-link").textContent}'`),
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
    "appearance": openAppearance,
    "reset-accent": () => { look.accent = ""; saveLook(); openAppearance(); },
    "restart-node": restartNode,
    "undo-settings": undoSettings,
    "save-setting": () => { const el = document.querySelector(`[data-setting="${CSS.escape(arg)}"]`); if (el) changeSetting(arg, el.value); },
  })[a]?.();
});

// ---------- node status ----------

const transports = () => (state.status && state.status.transports) || [];
const transportNamed = name => transports().find(t => t.name === name);

// The Tor supervisor's phases, in words.
const torPhase = {
  launching: "Starting the Tor program",
  bootstrapping: "Connecting to the Tor network",
  publishing: "Publishing this machine's address",
  ready: "Connected",
  offline: "Waiting for a network connection",
  retrying: "Waiting to try again",
  stopped: "Stopped",
};
// phaseOf fills in the phase for a backend that doesn't report one.
function phaseOf(t) {
  if (t.phase) return t.phase;
  if (t.state === "ready") return "ready";
  if (t.state === "starting") return /publish/.test(t.detail || "") ? "publishing" : "bootstrapping";
  return t.state === "failed" ? "retrying" : "stopped";
}
function torProgress(t) {
  if (t.progress) return t.progress;
  const m = /bootstrapping (\d+)%/.exec(t.detail || "");
  return m ? Number(m[1]) : 0;
}
// Stuck: Sol is waiting rather than making progress, so a manual reconnect
// can help. Only then is the Reconnect button shown.
const torStuck = t => t.state === "failed" || ["retrying", "offline"].includes(phaseOf(t));

// overall sums the node up for the status pill and the Home hero.
function overall() {
  const ts = transports();
  const tor = transportNamed("tor");
  const ready = ts.filter(t => t.state === "ready");
  if (!ts.length) return { cls: "bad", pill: "offline", title: "No way to connect is on",
    text: "Peers can't reach this machine and it can reach none. Turn on Tor or direct mode in Settings." };
  if (tor && tor.state === "ready") {
    const fast = ready.some(t => t.name === "wg" || t.name === "direct");
    return { cls: "ok", pill: "online", title: fast ? "Online over Tor and direct mode" : "Online over Tor",
      text: fast ? "Peers that have direct mode on connect straight to this machine; the others go through Tor."
        : "Your peers can reach what you share with them, from anywhere. No open ports, no accounts." };
  }
  if (tor) {
    const ph = phaseOf(tor), pct = torProgress(tor);
    const others = ready.filter(t => t.name !== "tor").map(t => pathOf(t.name).label);
    if (others.length) return { cls: "warn", pill: "online · tor " + (ph === "bootstrapping" ? pct + "%" : "starting"),
      title: "Online over " + others.join(" and "), text: "Tor is still starting: peers that only have this machine's Tor address can't reach it yet." };
    if (torStuck(tor)) return { cls: "bad", pill: "offline", title: ph === "offline" ? "No network connection" : "Can't reach the Tor network",
      text: "Sol keeps trying by itself. Peers can't reach this machine until Tor is back." };
    return { cls: "warn", pill: ph === "bootstrapping" ? `connecting ${pct}%` : ph === "publishing" ? "almost online" : "starting",
      title: ph === "publishing" ? "Almost online: publishing this machine's address" : "Connecting to Tor…",
      text: "Usually 30 seconds to two minutes. You can use the panel meanwhile." };
  }
  if (ready.length) return { cls: "ok", pill: "online · " + ready.map(t => pathShort(t.name)).join("+"),
    title: "Online over " + ready.map(t => pathOf(t.name).label).join(" and "), text: "Tor is off: peers reach this machine by its IP address." };
  return { cls: "warn", pill: "starting", title: "Starting up…", text: "" };
}

// What each transport means, for the Home screen's list of networks.
function networkLine(t) {
  const p = pathOf(t.name);
  const st = t.state === "ready" ? ["ok", "working"] : t.state === "starting" ? ["warn", "starting"]
    : t.state === "failed" ? ["bad", "not working"] : ["", "stopped"];
  return `<div class="net"><span class="dot ${esc(t.state)}"></span>
    <div class="grow"><b>${esc(p.label)}</b> <span class="state ${st[0]}">${st[1]}</span>
      ${t.detail && t.state !== "ready" ? `<span class="muted small"> · ${esc(t.detail)}</span>` : ""}
      <p class="muted small">${esc(p.means)}</p></div></div>`;
}

// The Tor start-up, step by step, with the real percentage and the time
// spent in the current step: never a spinner that might be stuck.
function torProgressHtml(t) {
  const ph = phaseOf(t), pct = torProgress(t);
  const steps = [["launching", "Start Tor"], ["bootstrapping", "Connect" + (ph === "bootstrapping" ? ` ${pct}%` : "")], ["publishing", "Publish address"]];
  const at = steps.findIndex(s => s[0] === ph);
  const since = sinceSpan(t.phase_since);
  let html = `<div class="startup-head"><b>Tor: ${esc(torPhase[ph] || ph)}</b>${since ? `<span class="muted small">for ${since}</span>` : ""}</div>`;
  if (at >= 0) {
    html += `<ol class="phases">${steps.map(([k, l], i) =>
      `<li class="${i < at ? "done" : i === at ? "now" : ""}">${esc(l)}</li>`).join("")}</ol>
      <progress max="100" value="${ph === "launching" ? 2 : ph === "publishing" ? 100 : Math.max(2, pct)}" aria-label="Tor start-up"></progress>`;
  }
  if (t.waiting_for) html += `<p class="muted small">Waiting for ${esc(t.waiting_for)}.</p>`;
  else if (ph === "publishing") html += `<p class="muted small">Tor is connected and is telling the network how to find this machine. Usually under a minute.</p>`;
  if (ph === "retrying") {
    html += `<p>Retrying automatically${t.attempt ? `, attempt ${esc(t.attempt)}` : ""}${!zeroTime(t.retry_at) ? `, next try in ${untilSpan(t.retry_at)}` : ""}.</p>`;
  } else if (t.attempt > 1) {
    html += `<p class="muted small">Attempt ${esc(t.attempt)}.</p>`;
  }
  if (t.problem) html += `<p class="warnline small">Last attempt: ${esc(t.problem)}</p>`;
  if (!t.phase && t.detail && ph !== "ready") html += `<p class="muted small">${esc(t.detail)}</p>`;
  if (torStuck(t)) html += `<div class="actions start"><button class="small" data-action="restart-tor">Reconnect now</button></div>`;
  return html;
}

function renderStatusBits() {
  const s = state.status;
  if (!s) return;
  const o = overall();
  for (const [pill, text] of [["status-pill", "status-pill-text"], ["topbar-pill", "topbar-pill-text"]]) {
    $(pill).className = "pill " + o.cls;
    $(text).textContent = o.pill;
  }
  $("hero").className = "hero " + o.cls;
  $("hero-title").textContent = o.title;
  $("hero-text").textContent = o.text;
  const tor = transportNamed("tor");
  const showTor = tor && tor.state !== "ready";
  $("tor-progress").hidden = !showTor;
  if (showTor) $("tor-progress").innerHTML = torProgressHtml(tor);
  $("networks").innerHTML = transports().map(networkLine).join("");
  tick();
}

// ---------- sidebar ----------

function renderSidebar() {
  const s = state.status;
  if (!s) return;
  $("side-name").textContent = "~ " + s.name;
  $("side-version").textContent = "sol " + (s.version || "dev");
  $("side-transports").innerHTML = transports().map(t =>
    `<span class="chip ${esc(t.state)}" title="${esc(pathOf(t.name).label + ": " + (t.detail || t.state))}"><span class="dot ${esc(t.state)}"></span>${esc(pathShort(t.name))}</span>`).join("");
  $("side-peers").innerHTML = state.peers.length ? state.peers.map(p => {
    const path = p.path || {};
    const label = path.via && (path.live || p.online) ? pathShort(path.via) : p.online ? "online" : "idle";
    return `<button class="side-peer ${p.id === onlyPeer ? "active" : ""}" data-action="show-peer" data-arg="${esc(p.id)}"
      title="${esc(p.name)}"><span class="dot ${p.online ? "ready" : "stopped"}"></span><span class="name">${esc(p.name)}</span>
      <span class="path">${esc(label)}</span></button>`;
  }).join("") : `<p class="muted small side-none">None paired yet.</p>`;
}

// ---------- home ----------

const svcLabel = name => ({ "_inbox": "inbox", "_files": "shared files" })[name] || name;
const peerName = id => (state.peers.find(p => p.id === id) || {}).name || id;

function renderHome() {
  const s = state.status;
  if (!s) return;
  renderStatusBits();

  // Checklist: hide once everything is done.
  const hasSvc = state.services.length > 0;
  const hasDev = state.peers.length > 0;
  const connected = state.peers.some(p => p.shares.some(x => x !== "_inbox"));
  $("step-service").classList.toggle("done", hasSvc);
  $("step-device").classList.toggle("done", hasDev);
  $("step-connect").classList.toggle("done", connected);
  $("checklist").hidden = hasSvc && hasDev && connected;

  $("live-list").innerHTML = state.peers.length ? `<div class="rows">${state.peers.map(p => {
    const path = p.path || {};
    const theirs = p.services.filter(x => x.name !== "_inbox").map(x => svcLabel(x.name));
    return `<button class="peer-row" data-action="show-peer" data-arg="${esc(p.id)}">
      <span class="dot ${p.online ? "ready" : "stopped"}"></span>
      <span class="pr-main"><b>${esc(p.name)}</b>
        <span class="muted small">${p.online ? "connected now" : "last seen " + esc(ago(p.last_seen))}${theirs.length ? " · shares " + esc(theirs.join(", ")) : ""}</span></span>
      ${path.via ? viaBadge(path.via) : ""}${path.reason && path.reason !== "direct_off" ? `<span class="flag" title="${esc(path.detail || "")}">!</span>` : ""}
    </button>`;
  }).join("")}</div>`
    : `<div class="empty"><p>No peers yet. Pair another machine that runs Sol to open each other's apps and send files.</p>
      <div class="actions start"><button data-action="pair">Invite a machine</button><button class="ghost" data-action="join">Join with a link</button></div></div>`;

  $("tunnels-card").hidden = state.tunnels.length === 0;
  $("tunnel-list").innerHTML = `<div class="rows">${state.tunnels.map(tunnelRow).join("")}</div>`;

  $("audit-card").hidden = state.audit.length === 0;
  $("audit-list").innerHTML = state.audit.map(r => `
    <div class="row"><span class="badge ${esc(r.status)}">${esc(r.status)}</span>
      <b>${esc(r.service)}</b> <span class="muted">${esc(r.check)}: ${esc(r.detail)}</span></div>`).join("");

  $("recent-events").innerHTML = state.events.length ? state.events.slice(0, 6).map(eventItem).join("")
    : `<li class="muted">Nothing has happened yet.</li>`;
}

// One open tunnel: a peer's service reachable on this machine, and the path it takes.
function tunnelRow(t) {
  const state_ = !t.listening ? `<span class="state bad">${esc(t.last_error || "port unavailable")}</span>`
    : t.last_error ? `<span class="state warn" title="${esc(t.last_error)}">peer unreachable right now</span>`
    : `<span class="state ok">ready</span>`;
  const target = t.url
    ? `<a href="${esc(t.url)}" target="_blank" rel="noopener noreferrer">${esc(t.addr)}</a>`
    : `<code>${esc(t.addr)}</code>`;
  return `<div class="tunnel">
    <div class="grow"><b>${esc(t.peer_name || peerName(t.peer))} / ${esc(svcLabel(t.service))}</b>
      <div class="small">${target} ${state_}</div>
      <div class="small">${t.via ? viaBadge(t.via) + ` <span class="muted">${esc(pathOf(t.via).means)}</span>` : `<span class="muted">Connects when you open it.</span>`}</div></div>
    <div class="actions"><button class="ghost small" data-action="copy-text" data-arg="${esc(t.addr)}">Copy</button>
      <button class="ghost small danger-text" data-action="close-tunnel" data-arg="${esc(String(t.port))}">Close</button></div>
  </div>`;
}

async function restartTor() {
  try {
    await api("/api/transports/tor/restart", { method: "POST" });
    toast("Reconnecting to Tor. This takes about a minute.");
    refresh();
  } catch (err) { fail(err); }
}

// ---------- services ----------

function renderServices() {
  const list = state.services;
  if (!loaded) return;
  $("service-list").innerHTML = list.length ? list.map(s => {
    const addrs = Object.entries(s.addresses || {});
    return `<div class="card service">
      <div class="card-head"><h3>${esc(s.name)}</h3>
        <button class="ghost small danger-text" data-action="remove-service" data-arg="${esc(s.name)}">Remove</button></div>
      ${s.dir
        ? (s.running
          ? `<p class="okline">● Serving the folder <code>${esc(s.dir)}</code></p>`
          : `<p class="warnline">! The folder ${esc(s.dir)} can't be read.</p>`)
        : s.running
        ? `<p class="okline">● Running on port ${esc(s.local_port)}</p>`
        : `<p class="warnline">! Nothing is running on port ${esc(s.local_port)}. Start the app; this updates by itself.</p>`}
      ${s.record ? `<p class="muted small">Signed record at <code>/.well-known/sol.json</code> on each public address.</p>` : ""}
      <label class="inline">Who can open it
        <select data-vis="${esc(s.name)}">${Object.entries(visibilityLabel).map(([v, l]) =>
          `<option value="${v}" ${v === s.visibility ? "selected" : ""}>${l}</option>`).join("")}</select></label>
      ${s.active_streams ? `<p class="muted small">✓ ${esc(plural(s.active_streams, "verified connection"))} open</p>` : ""}
      ${addrs.length ? addrs.map(([net, a]) => `
        <div class="addr"><span class="muted small">${esc(pathOf(net).label)} address</span>
          <div class="copyrow"><code>${esc(a)}</code>
            <button class="ghost small" data-action="copy-text" data-arg="${esc(net === "tor" ? "http://" + a : a)}">Copy</button>
            ${net === "tor" ? `<button class="ghost small" data-action="show-qr" data-arg="http://${esc(a)}">QR</button>` : ""}</div></div>`).join("")
        : s.visibility !== "private" ? `<p class="muted small">Publishing the address…</p>` : ""}
    </div>`;
  }).join("")
  : `<div class="empty card"><p><b>No services yet.</b></p>
      <p class="muted">A service is an app on this machine (a music server, a photo library) or a folder served as a website.</p>
      <button data-action="add-service">Add your first service</button></div>`;
}
document.addEventListener("change", e => {
  const sel = e.target.closest("[data-vis]");
  if (!sel) return;
  api(`/api/services/${encodeURIComponent(sel.dataset.vis)}/visibility`, json("PUT", { visibility: sel.value }))
    .then(() => { toast(`${sel.dataset.vis}: ${visibilityLabel[sel.value]}`); refresh(); })
    .catch(err => { fail(err); refresh(); });
});

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
  $("service-submit").disabled = false;
  $("templates").innerHTML = templates.map((t, i) =>
    `<button type="button" class="tpl" data-tpl="${i}"><b>${esc(t.label)}</b><span class="muted small">${esc(t.hint)}</span></button>`).join("");
  $("dlg-service").showModal();
}
$("templates").addEventListener("click", e => {
  const b = e.target.closest("[data-tpl]");
  if (!b) return;
  const f = $("service-form"), t = templates[b.dataset.tpl];
  $("templates").querySelectorAll(".tpl").forEach(x => x.classList.toggle("active", x === b));
  let name = t.name, n = 2;
  while (name && state.services.some(s => s.name === name)) name = t.name + "-" + n++;
  f.name.value = name; f.local_port.value = t.port; f.protocol.value = t.protocol;
  setSiteMode(!!t.site);
  if (t.site) f.visibility.value = "public";
  (t.site ? f.dir : name ? f.local_port : f.name).focus();
});

$("service-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = e.target;
  $("service-submit").disabled = true;
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
  } finally { $("service-submit").disabled = false; }
});

async function removeService(name) {
  await ask({ title: `Remove ${name}?`, yes: "Remove", danger: true,
    html: `<p>Peers using it are disconnected and can no longer open it. The app itself is not touched.</p>`,
    run: async () => { await api("/api/services/" + encodeURIComponent(name), { method: "DELETE" }); toast(`Removed ${name}`); refresh(); } });
}

function showQr(text) {
  $("qr-big").innerHTML = qrSvg(text);
  $("qr-text").textContent = text;
  $("dlg-qr").showModal();
}

// ---------- peers ----------

const builtinShares = [
  ["_inbox", "Can send you files", "Files it sends land in your Files screen."],
  ["_files", "Shared files", "Your shared-files page (files you added on the Files screen)."],
];

function shareBox(p, name, label, hint) {
  const on = p.shares.includes(name);
  return `<label class="share"><input type="checkbox" data-share="${esc(p.id)}" data-svc="${esc(name)}" ${on ? "checked" : ""}>
    <span><b>${esc(label)}</b>${hint ? `<span class="muted small"> ${esc(hint)}</span>` : ""}</span></label>`;
}

function renderPeers() {
  if (!loaded) return;
  if (onlyPeer && !state.peers.some(p => p.id === onlyPeer)) onlyPeer = ""; // unpaired meanwhile
  // Don't redraw under a pointer that is about to tick a share box.
  if ($("peer-list").contains(document.activeElement) && document.activeElement.matches("input")) return;
  const list = onlyPeer ? state.peers.filter(p => p.id === onlyPeer) : state.peers;
  const filter = onlyPeer && state.peers.length > 1 ? `<p class="peer-filter"><button class="ghost small" data-action="all-peers">← All peers</button></p>` : "";
  $("peer-list").innerHTML = list.length ? filter + list.map(p => {
    const theirs = p.services.filter(s => s.name !== "_inbox");
    return `<article class="card device ${p.online ? "online" : ""}">
      <div class="card-head"><h3><span class="dot ${p.online ? "ready" : "stopped"}"></span>${esc(p.name)}</h3>
        <button class="ghost small danger-text" data-action="unpair" data-arg="${esc(p.id)}">Unpair</button></div>
      <p class="${p.online ? "okline" : "muted"} small">${p.online ? "✓ Connected and verified" : "Not connected · last seen " + esc(ago(p.last_seen))}</p>

      <div class="pathbox">${pathBlock(p)}</div>

      ${p.connections.length ? `<h4>Open connections</h4><div class="conns">${p.connections.map(c => `
        <div class="conn small">${c.outbound ? "You → its" : "It → your"} <b>${esc(svcLabel(c.service))}</b>
          <span class="muted">· ${esc(plural(c.streams, "stream"))} · since ${esc(clock(c.since))}</span>
          ${c.via ? viaBadge(c.via) : c.outbound ? "" : `<span class="muted">· it chose the path</span>`}</div>`).join("")}</div>` : ""}

      <h4>It shares with you</h4>
      ${theirs.length
        ? `<div class="theirs">${theirs.map(svc => theirService(p, svc)).join("")}</div>`
        : `<p class="muted small">Nothing yet (it can still receive files from you).</p>`}
      <div class="actions start"><button class="ghost small" data-action="refresh-peer" data-arg="${esc(p.id)}">Check again</button></div>

      <h4>You share with it</h4>
      <div class="shares">
        ${builtinShares.map(([n, l, h]) => shareBox(p, n, l, h)).join("")}
        ${state.services.map(s => shareBox(p, s.name, s.name, s.dir ? "website" : "port " + s.local_port)).join("")}
      </div>

      <div class="actions start"><button class="small" data-action="send-file" data-arg="${esc(p.id)}">Send a file…</button></div>

      <details class="disclosure small"><summary>How is this peer verified?</summary>
        <p class="muted">Every connection, in either direction, starts with a fresh random challenge that the
          other side must sign with its private key. Only the key paired here (fingerprint
          <code>${esc(p.id)}</code>) is accepted, and old signatures can't be replayed.
          ${p.locked.includes("tor") ? "It also holds a Tor access key made just for it, so without that key this machine's address can't even be found." : ""}</p>
        <p class="muted">Paired ${esc(new Date(p.added).toLocaleString())}.</p>
      </details>
    </article>`;
  }).join("")
  : `<div class="empty card"><p><b>No peers yet.</b></p>
      <p class="muted">Pair another machine that runs Sol. Then you can share apps with it and send files both ways.</p>
      <div class="actions start"><button data-action="pair">Invite a machine</button><button class="ghost" data-action="join">Join with a link</button></div></div>`;
  tick();
}

// One service a peer shares with us: Open it, or show its open tunnel.
function theirService(p, svc) {
  const tun = state.tunnels.find(t => t.peer === p.id && t.service === svc.name);
  const label = esc(svcLabel(svc.name));
  if (!tun) {
    return `<div class="svcrow"><span class="tag">${label}</span>
      <button class="small" data-action="open-tunnel" data-arg="${esc(p.id)}" data-svc="${esc(svc.name)}">Open</button></div>`;
  }
  const status = !tun.listening ? `<span class="state bad">${esc(tun.last_error || "port unavailable")}</span>`
    : tun.last_error ? `<span class="state warn">peer unreachable right now</span>`
    : tun.via ? viaBadge(tun.via) : `<span class="state ok">ready</span>`;
  const target = tun.url
    ? `<a href="${esc(tun.url)}" target="_blank" rel="noopener noreferrer">${esc(tun.addr)}</a>`
    : `<code>${esc(tun.addr)}</code> <button class="ghost small" data-action="copy-text" data-arg="${esc(tun.addr)}">Copy</button>`;
  return `<div class="svcrow"><span class="tag">${label}</span> ${target} ${status}
    ${tun.url && !android ? `<button class="small" data-action="open-tunnel" data-arg="${esc(p.id)}" data-svc="${esc(svc.name)}">Open</button>` : ""}
    <button class="ghost small" data-action="close-tunnel" data-arg="${esc(String(tun.port))}">Close</button></div>`;
}

// Open a peer's service. The node keeps the tunnel (also after restarts),
// so this page can be closed. In a browser the tab is opened right away,
// inside the click, so popup blockers allow it; it's pointed at the tunnel
// once the node answers. The Android app has no tabs: there the address
// appears as a link to tap.
async function openTunnel(id, svc) {
  const known = (state.peers.find(p => p.id === id)?.services || []).find(s => s.name === svc);
  const tab = !android && known && known.protocol === "http" ? window.open("about:blank", "_blank") : null;
  if (tab) tab.opener = null;
  try {
    const tun = await api(`/api/peers/${encodeURIComponent(id)}/services/${encodeURIComponent(svc)}/open`, { method: "POST" });
    if (tun.url && tab) tab.location.href = tun.url;
    else if (tab) tab.close();
    if (!tun.url) toast(`${svcLabel(svc)} is on ${tun.addr} (for apps that aren't web pages)`);
    else if (android) toast(`${svcLabel(svc)} is open: tap its address to use it`);
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
  box.blur();
  api(`/api/peers/${encodeURIComponent(id)}/shares/${encodeURIComponent(svc)}`, json("PUT", { shared: box.checked }))
    .then(() => { toast(`${peerName(id)} ${box.checked ? "can now" : "can no longer"} open ${svcLabel(svc)}`); refresh(); })
    .catch(err => { box.checked = !box.checked; fail(err); });
});

async function unpair(id) {
  const name = peerName(id);
  await ask({ title: `Unpair ${name}?`, yes: "Unpair", danger: true,
    html: `<p>Both machines lose access to each other immediately, including open connections. To connect again you pair again.</p>`,
    run: async () => {
      const r = await api("/api/peers/" + encodeURIComponent(id), { method: "DELETE" });
      toast(r.notified ? `Unpaired. ${name} removed you too.` : `Removed ${name}. It couldn't be told, but it can no longer reach you.`);
      refresh();
    } });
}

async function refreshPeer(id) {
  toast(`Asking ${peerName(id)} what it shares…`);
  try { await api(`/api/peers/${encodeURIComponent(id)}/refresh`, { method: "POST" }); toast(`${peerName(id)} answered`); refresh(); }
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
  await ask({ title: "Rename this machine", yes: "Rename",
    html: `<p class="muted">Your peers see this name.</p>`,
    input: { label: "Name", value: state.status ? state.status.name : "" },
    run: async name => { await api("/api/name", json("PUT", { name })); toast(`This machine is now "${name}"`); refresh(); } });
}

// ---------- pairing: invite ----------

const installCmds = {
  arch: "sudo pacman -S --needed tor\ncurl -fsSL https://raw.githubusercontent.com/apv022/sol/main/install.sh | sh\nsol",
  debian: "sudo apt update && sudo apt install -y tor\ncurl -fsSL https://raw.githubusercontent.com/apv022/sol/main/install.sh | sh\nsol",
  mac: "brew install tor\ncurl -fsSL https://raw.githubusercontent.com/apv022/sol/main/install.sh | sh\nsol",
};
let pairTimer = null;

function setOS(os) {
  document.querySelectorAll("[data-os]").forEach(b => {
    b.classList.toggle("active", b.dataset.os === os);
    b.setAttribute("aria-pressed", String(b.dataset.os === os));
  });
  $("install-cmd").textContent = installCmds[os];
}
document.querySelectorAll("[data-os]").forEach(b => b.addEventListener("click", () => setOS(b.dataset.os)));

// pairNotReady says, inside the invite dialog, why a link made now might not
// work yet: Tor is still publishing this machine's address.
function pairNotReady() {
  const tor = transportNamed("tor");
  const otherReady = transports().some(t => t.name !== "tor" && t.state === "ready");
  const show_ = tor && tor.state !== "ready";
  $("pair-notready").hidden = !show_;
  if (!show_) return;
  const ph = phaseOf(tor);
  $("pair-notready-title").textContent = otherReady ? "Tor isn't ready yet" : "This machine isn't reachable yet";
  $("pair-notready-text").textContent = `Tor: ${torPhase[ph] || ph}${ph === "bootstrapping" ? " (" + torProgress(tor) + "%)" : ""}. ` +
    (otherReady ? "The link works over the other networks now, and over Tor once it is ready." : "A machine that joins now waits until Tor is ready.");
  $("pair-notready-bar").value = ph === "publishing" ? 100 : torProgress(tor);
}

async function openPairDialog() {
  setOS(/Mac/.test(navigator.platform) ? "mac" : "debian");
  $("pair-error").hidden = true;
  $("pair-link").textContent = "Creating a link…";
  $("pair-copy").disabled = $("pair-copy-cmd").disabled = true;
  $("qr").innerHTML = "";
  $("pair-countdown").textContent = "10:00";
  $("pair-wait").classList.remove("done");
  $("pair-wait-title").textContent = "Waiting for the other machine…";
  $("pair-wait-text").textContent = "Keep this open. Over Tor the first connection usually takes 30 seconds to 2 minutes.";
  pairNotReady();
  $("dlg-pair").showModal();

  const known = new Set(state.peers.map(p => p.id));
  let expires = null, tries = 0;
  const invite = async () => {
    try {
      const r = await api("/api/peers/invite", { method: "POST" });
      $("pair-link").textContent = r.link;
      $("pair-copy").disabled = $("pair-copy-cmd").disabled = false;
      $("qr").innerHTML = qrSvg(r.link);
      $("pair-error").hidden = true;
      expires = new Date(r.expires);
    } catch (err) {
      // Usually: no address to put in the link yet. Try again while open.
      $("pair-error").textContent = "Couldn't create a pairing link yet: " + err.message + (tries++ ? "" : " Trying again…");
      $("pair-error").hidden = false;
    }
  };
  await invite();
  clearInterval(pairTimer);
  pairTimer = setInterval(async () => {
    if (!$("dlg-pair").open) return clearInterval(pairTimer);
    pairNotReady();
    if (!expires) return invite();
    const left = Math.max(0, Math.round((expires - Date.now()) / 1000));
    $("pair-countdown").textContent = clockDur(left);
    try {
      const peers = await api("/api/peers");
      const fresh = peers.find(p => !known.has(p.id));
      if (fresh) {
        clearInterval(pairTimer);
        $("pair-wait").classList.add("done");
        $("pair-wait-title").textContent = `✓ Paired with ${fresh.name}`;
        $("pair-wait-text").textContent = "You can now send each other files. Choose what you share with it on the Peers screen.";
        toast(`${fresh.name} is paired`);
        refresh();
      } else if (left === 0) {
        clearInterval(pairTimer);
        $("pair-wait-title").textContent = "This link expired";
        $("pair-wait-text").textContent = "Close this and invite again for a new link.";
      }
    } catch { /* keep polling */ }
  }, 2000);
}
$("dlg-pair").addEventListener("close", () => clearInterval(pairTimer));

// ---------- pairing: join ----------

let joinAbort = null;

// What a pasted link says about the other machine, before pairing. The
// link is untrusted text: only shown, escaped.
function previewLink(link) {
  link = link.trim();
  if (!link) return "";
  let u;
  try { u = new URL(link); } catch { u = null; }
  if (!u || u.protocol !== "sol:" || !/^\/*pair/.test(u.host + u.pathname)) {
    return `<p class="warnline">This doesn't look like a Sol pairing link. It starts with <code>sol://pair?</code></p>`;
  }
  const q = u.searchParams, nets = [];
  for (const k of ["tor", "wg", "direct", "clearnet", "i2p"]) if (q.get(k)) nets.push(pathOf(k).label);
  return `<p>Pairs with <b>${esc(q.get("name") || "a machine")}</b>${nets.length ? `, reachable over ${esc(nets.join(", "))}` : ""}.</p>
    ${!q.get("s") || !q.get("node") ? `<p class="warnline small">Parts of the link are missing. Copy the whole link again.</p>` : ""}`;
}
function updateJoinPreview() {
  const html = previewLink($("join-link").value);
  $("join-preview").innerHTML = html;
  $("join-preview").hidden = !html;
}
$("join-link").addEventListener("input", updateJoinPreview);

function joinMode(busy_) {
  $("join-entry").hidden = busy_;
  $("join-busy").hidden = !busy_;
  $("join-submit").disabled = busy_;
}

// openJoinDialog opens the join form, optionally prefilled (from the
// Android app's sol:// handler). It never pairs by itself.
function openJoinDialog(link = "") {
  $("join-form").reset();
  $("join-link").value = link;
  $("join-error").hidden = true;
  joinMode(false);
  updateJoinPreview();
  $("join-paste").hidden = !navigator.clipboard || !navigator.clipboard.readText;
  if (!$("dlg-join").open) $("dlg-join").showModal();
  if (link) $("join-submit").focus(); else $("join-link").focus();
}
async function joinPaste() {
  try { $("join-link").value = (await navigator.clipboard.readText()).trim(); updateJoinPreview(); }
  catch { toast("Couldn't read the clipboard. Paste into the box instead.", "bad"); }
}
function joinCancel() {
  if (joinAbort) joinAbort.abort();
  $("dlg-join").close();
}
$("join-form").addEventListener("submit", async e => {
  e.preventDefault();
  const link = $("join-link").value.trim();
  if (!link) return;
  $("join-error").hidden = true;
  $("join-busy-title").textContent = "Pairing…";
  $("join-status").textContent = link.includes("tor=")
    ? "Finding the other machine over Tor. This usually takes 30 seconds to 2 minutes; Sol retries by itself."
    : "Contacting the other machine…";
  joinMode(true);
  joinAbort = new AbortController();
  try {
    const p = await api("/api/peers/pair", Object.assign(json("POST", { link }), { signal: joinAbort.signal }));
    $("dlg-join").close();
    toast(`Paired with ${p.name}`);
    show("peers", p.id);
  } catch (err) {
    joinMode(false);
    if (err.name === "AbortError") return;
    $("join-error").textContent = err.message;
    $("join-error").hidden = false;
  } finally { joinAbort = null; }
});

// ---------- files ----------

const playable = /\.(mp3|flac|ogg|opus|wav|m4a|aac|mp4|webm)$/i;

function renderFiles() {
  if (!loaded) return;
  $("file-list").innerHTML = state.files.length ? `<div class="rows">${state.files.map(f => `
    <div class="file">
      <div class="grow"><b class="fname">${esc(f.name || "(unnamed file)")}</b>
        ${f.from ? `<span class="tag">from ${esc(peerName(f.from))}</span>` : ""}
        <div class="muted small">${esc(size(f.size))} · ${esc(ago(f.modified))} · <code title="${esc(f.hash)}">${esc(f.hash.slice(0, 12))}…</code></div></div>
      <div class="actions">
        ${playable.test(f.name || "") ? `<button class="ghost small" data-action="play" data-arg="${esc(f.hash)}">Play</button>` : ""}
        <a class="button ghost small" href="/blob/${esc(f.hash)}" download="${esc(f.name || f.hash)}">Download</a>
        <button class="ghost small desktop-only" data-action="copy-text" data-arg="${esc(f.hash)}">Copy hash</button>
      </div>
    </div>`).join("")}</div>`
    : `<div class="empty"><p class="muted">No files yet. Add some above, or ask a peer to send you one.</p></div>`;
}

function play(hash) {
  const p = $("player");
  p.src = "/blob/" + encodeURIComponent(hash);
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
  $("file-input").value = "";
  refresh();
}
$("file-input").addEventListener("change", e => upload(e.target.files));
const dz = $("dropzone");
["dragenter", "dragover"].forEach(ev => dz.addEventListener(ev, e => { e.preventDefault(); dz.classList.add("over"); }));
["dragleave", "drop"].forEach(ev => dz.addEventListener(ev, e => { e.preventDefault(); dz.classList.remove("over"); }));
dz.addEventListener("drop", e => upload(e.dataTransfer.files));

// ---------- settings: how this node connects ----------

// The three ways to connect, each with what it does and what it costs.
const connectChoices = [
  { key: "tor", title: "Tor", runs: ["tor"],
    what: "Reaches peers through the Tor network. Works behind any router or firewall with nothing to set up.",
    pros: ["Hides this machine's IP address from peers and from the network", "Works anywhere: no ports to open, no router setup"],
    cons: ["Slower: noticeable delay, modest speeds", "Takes 30 seconds to a couple of minutes to come online after a start"] },
  { key: "direct", title: "Direct mode", runs: ["wg", "direct"],
    what: "Connects straight to a peer over WireGuard (UDP), or over TLS (TCP) where UDP is blocked. When neither gets through, Sol uses Tor.",
    pros: ["Much faster: full speed of your connection", "Still encrypted, and only paired peers can connect"],
    cons: ["Each machine sees the other's IP address", "Must be on at both ends; a firewall may need the port allowed"] },
  { key: "clearnet", title: "LAN transport (unencrypted)", runs: ["clearnet"],
    what: "Plain TCP on one local network, for testing or a network you fully trust.",
    pros: ["Simple and fast; needs neither Tor nor direct mode"],
    cons: ["Not encrypted: anyone on the network can read the traffic", "Shows your LAN address; only works inside one network"] },
];
const connectKeys = connectChoices.map(c => c.key);
const advancedKeys = ["direct_listen", "direct_advertise", "stun", "clearnet_listen", "panel_port", "node_port"];
const settingOf = key => state.settings.find(o => o.key === key);
const errSlot = key => document.querySelector(`[data-err="${CSS.escape(key)}"]`);

// Errors stay next to their control, also across redraws, until the next try.
const errors = {};
function showSettingError(key, err) {
  errors[key] = err.message || String(err);
  const el = errSlot(key);
  if (!el) return fail(err);
  el.textContent = errors[key];
  el.hidden = false;
}
const errHtml = key => `<p class="field-error" data-err="${esc(key)}" role="alert" ${errors[key] ? "" : "hidden"}>${esc(errors[key] || "")}</p>`;

// What the running node actually does with a transport, which can differ
// from the saved setting until it restarts.
function runningLine(c, o) {
  const ts = c.runs.map(transportNamed).filter(Boolean);
  let html;
  if (!ts.length) html = `<span class="state">Not running</span>`;
  else html = ts.map(t => {
    const cls = t.state === "ready" ? "ok" : t.state === "starting" ? "warn" : t.state === "failed" ? "bad" : "";
    const word = t.state === "ready" ? "working" : t.state === "starting" ? "starting" : t.state === "failed" ? "not working" : "stopped";
    return `<span class="state ${cls}">${esc(pathOf(t.name).label)}: ${word}</span>${t.state === "failed" && t.detail ? ` <span class="muted small">${esc(t.detail)}</span>` : ""}`;
  }).join(" ");
  if (o && o.pending) html += `<p class="warnline small">Saved as <b>${esc(o.value)}</b>; the node still runs with it <b>${esc(o.running)}</b> until it restarts.
    <button class="linkish" data-action="restart-node">Restart now</button></p>`;
  return html;
}

function noteHtml(key) {
  const n = notes[key];
  return n && n.length ? `<ul class="effects small">${n.map(s => `<li>${esc(s)}</li>`).join("")}</ul>` : "";
}

function renderSettings(force = false) {
  const s = state.status;
  if (!s || !loaded) return;
  $("node-id").textContent = s.node_id;
  $("node-name").textContent = s.name;
  $("version").textContent = s.version || "dev";
  $("uptime").textContent = s.uptime;
  const as = s.autostart || {};
  $("autostart").innerHTML = !as.supported ? `<span class="muted">Not on this system yet</span>`
    : as.enabled ? "Yes"
    : as.other ? `No <span class="muted small">· set up for another Sol data folder</span>`
    : `No <span class="muted small">· turn on with <code>sol autostart on</code></span>`;

  const ht = s.help_tor || { state: "off" };
  if (document.activeElement !== $("help-tor") && !busy.help_tor) $("help-tor").checked = !!ht.enabled;
  $("help-tor-state").textContent = ht.state === "running" ? "On" : ht.enabled ? "On, not running" + (ht.detail ? ": " + ht.detail : "") : "Off";
  $("help-tor-state").className = ht.state === "running" ? "okline" : ht.enabled ? "warnline" : "muted";

  const pending = state.settings.filter(o => o.pending);
  $("restart-banner").hidden = !(s.restart_needed || pending.length);
  $("pending-list").innerHTML = pending.map(o =>
    `<li><b>${esc(o.label)}</b>: ${esc(o.running || "(empty)")} → ${esc(o.value || "(empty)")}</li>`).join("");

  // Don't redraw under someone who is typing a value or mid-change.
  const typing = el => el.contains(document.activeElement) && document.activeElement.matches("input:not([type=checkbox]), select, textarea");
  if (!force && (typing($("connect-list")) || typing($("advanced-list")) || Object.values(busy).some(Boolean))) return;

  $("connect-list").innerHTML = connectChoices.map(c => {
    const o = settingOf(c.key);
    if (!o) return "";
    const on = o.value === "on";
    return `<div class="card choice-card ${on ? "on" : ""}">
      <label class="switch"><input type="checkbox" role="switch" data-setting-toggle="${esc(c.key)}" ${on ? "checked" : ""} ${busy[c.key] ? "disabled" : ""}>
        <span class="track" aria-hidden="true"></span>
        <span class="switch-text"><b>${esc(c.title)}</b> <span class="muted">${on ? "on" : "off"}</span></span></label>
      <p>${esc(c.what)}</p>
      <ul class="tradeoffs small">${c.pros.map(x => `<li class="pro">${esc(x)}</li>`).join("")}${c.cons.map(x => `<li class="con">${esc(x)}</li>`).join("")}</ul>
      <div class="running small">${runningLine(c, o)}</div>
      ${noteHtml(c.key)}
      ${errHtml(c.key)}
    </div>`;
  }).join("");

  $("advanced-list").innerHTML = advancedKeys.map(settingOf).filter(Boolean).map(o => `
    <div class="setting">
      <label for="set-${esc(o.key)}"><b>${esc(o.label)}</b>${o.pending ? ` <span class="tag warn">restart to apply</span>` : ""}</label>
      <div class="copyrow"><input id="set-${esc(o.key)}" data-setting="${esc(o.key)}" value="${esc(o.value)}"
          ${o.kind === "port" ? `type="number" inputmode="numeric" min="1" max="65535"` : `placeholder="(none)" autocapitalize="none" spellcheck="false"`}>
        <button class="ghost" data-action="save-setting" data-arg="${esc(o.key)}" ${busy[o.key] ? "disabled" : ""}>Save</button></div>
      <p class="muted small">${esc(o.help)} <code>sol config ${esc(o.key)}</code></p>
      ${noteHtml(o.key)}
      ${errHtml(o.key)}
    </div>`).join("");
}

// impactHtml words an Impact for the confirmation dialog.
function impactHtml(im) {
  let html = "";
  if (im.unreachable.length) html += `<div class="notice bad"><b>${esc(im.unreachable.map(p => p.name).join(", "))}
    ${im.unreachable.length === 1 ? "becomes" : "become"} unreachable.</b></div>`;
  html += `<ul class="effects">${im.effects.map(s => `<li>${esc(s)}</li>`).join("")}</ul>`;
  if (im.restart) html += `<p><span class="tag warn">Needs a restart</span></p>`;
  return html;
}

// changeSetting asks the node what a change would do, confirms it when it
// matters, saves it, and then re-reads everything, so the panel only ever
// shows what the node really has. Any error stays next to the control.
async function changeSetting(key, value) {
  const o = settingOf(key);
  const el = errSlot(key);
  if (el) el.hidden = true;
  delete errors[key];
  busy[key] = true;
  try {
    let im;
    try { im = await api(`/api/settings/${encodeURIComponent(key)}/impact?value=${encodeURIComponent(value)}`); }
    catch (err) { showSettingError(key, err); return; }
    if (!im.changes) return;
    if (im.problem) { showSettingError(key, new Error(im.problem)); return; }
    if (im.confirm || im.unreachable.length) {
      const label = o ? o.label : key;
      const ok = await ask({ title: (value === "on" ? "Turn on " : value === "off" ? "Turn off " : "Change ") + label + "?",
        html: impactHtml(im), danger: im.unreachable.length > 0 || value === "off",
        yes: value === "on" ? "Turn on" : value === "off" ? "Turn off" : "Change it" });
      if (!ok) return;
    }
    try {
      await api("/api/settings/" + encodeURIComponent(key), json("PUT", { value }));
      notes[key] = im.effects;
      toast(im.restart ? "Saved. Restart the node to apply it." : "Saved");
    } catch (err) { showSettingError(key, err); return; }
  } finally {
    busy[key] = false;
    await refresh();
    renderSettings(true);
  }
}
document.addEventListener("change", e => {
  const box = e.target.closest("[data-setting-toggle]");
  if (!box) return;
  const key = box.dataset.settingToggle, value = box.checked ? "on" : "off";
  box.checked = !box.checked; // the switch moves once the node has the new value
  changeSetting(key, value);
});
document.addEventListener("keydown", e => {
  if (e.key === "Enter" && e.target.matches("[data-setting]")) { e.preventDefault(); changeSetting(e.target.dataset.setting, e.target.value); }
});

// Undo puts every changed setting back to what the node is running with.
async function undoSettings() {
  for (const o of state.settings.filter(x => x.pending)) {
    try { await api("/api/settings/" + encodeURIComponent(o.key), json("PUT", { value: o.running })); delete notes[o.key]; }
    catch (err) { fail(err); }
  }
  toast("Changes undone");
  await refresh();
  renderSettings(true);
}

$("help-tor").addEventListener("change", async e => {
  const box = e.target, on = box.checked;
  box.checked = !on;
  $("help-tor-error").hidden = true;
  busy.help_tor = true;
  box.disabled = true;
  try {
    if (on) {
      const im = await api("/api/settings/help_tor/impact?value=on");
      if (im.problem) { toast(im.problem, "bad"); box.checked = !on; return; }
      if (im.changes && im.confirm && !await ask({ title: "Run a Snowflake proxy?", yes: "Turn on", html: impactHtml(im) })) return;
    }
    await api("/api/help-tor", json("PUT", { enabled: on }));
    box.checked = on;
    toast(on ? "Helping the Tor network: Snowflake proxy on" : "Snowflake proxy off");
  } catch (err) {
    $("help-tor-error").textContent = err.message;
    $("help-tor-error").hidden = false;
  } finally {
    busy.help_tor = false;
    box.disabled = false;
    refresh();
  }
});

async function restartNode() {
  const ok = await ask({ title: "Restart the node?", yes: "Restart",
    html: `<p>Open connections drop for a few seconds${transportNamed("tor") || settingOf("tor")?.value === "on" ? ", and Tor takes up to a couple of minutes to be reachable again" : ""}. This page reconnects by itself.</p>` });
  if (!ok) return;
  try { await api("/api/restart", { method: "POST" }); } catch (err) { return fail(err); }
  toast("Restarting…");
  // The panel may come back on another port if panel_port was changed.
  const port = (settingOf("panel_port") || {}).value;
  const next = port && port !== location.port ? `${location.protocol}//${location.hostname}:${port}/#settings` : "";
  for (let i = 0; i < 60; i++) {
    await new Promise(r => setTimeout(r, 500));
    if (next && i > 3) { location.href = next; return; }
    try { await api("/api/status"); toast("The node is back"); return refresh(); } catch { /* still restarting */ }
  }
  toast("The node didn't come back. Start it with `sol` in a terminal.", "bad");
}

// ---------- activity ----------

const eventStyle = {
  pair: ["ok", "paired"], connect: ["ok", "connected"], revoke: ["warn", "removed"], file: ["ok", "file"],
  reject: ["bad", "blocked"], service: ["", "service"], transport: ["", "network"], audit: ["warn", "audit"],
};
const logGroups = { peers: ["pair", "connect", "revoke"], files: ["file"], network: ["transport", "service"], blocked: ["reject", "audit"] };
const logTime = iso => new Date(iso).toLocaleTimeString([], { hour12: false });
function eventItem(e) {
  const [cls, label] = eventStyle[e.kind] || ["", e.kind];
  return `<li class="${cls}"><span class="ev-time" title="${esc(new Date(e.time).toLocaleString())}">${esc(logTime(e.time))}</span>
    <span class="ev-kind">${esc(label)}</span><span class="ev-msg">${esc(e.message)}</span></li>`;
}
function renderEvents() {
  if (!loaded) return;
  const list = logFilter === "all" ? state.events : state.events.filter(e => (logGroups[logFilter] || []).includes(e.kind));
  $("event-list").innerHTML = list.length ? list.map(eventItem).join("")
    : `<li class="muted">${state.events.length ? "Nothing of this kind yet." : "Nothing has happened yet."}</li>`;
}
$("log-filter").addEventListener("click", e => {
  const b = e.target.closest("[data-log]");
  if (!b) return;
  logFilter = b.dataset.log;
  $("log-filter").querySelectorAll("[data-log]").forEach(x => {
    x.classList.toggle("active", x === b);
    x.setAttribute("aria-pressed", String(x === b));
  });
  renderEvents();
});

// ---------- refresh loop ----------

function setLost(lost) {
  document.body.classList.toggle("lost", lost);
  $("lost").hidden = !lost;
  if (lost) for (const [pill, text] of [["status-pill", "status-pill-text"], ["topbar-pill", "topbar-pill-text"]]) {
    $(pill).className = "pill bad";
    $(text).textContent = "no connection to the node";
  }
}

let refreshing = null;
function refresh() {
  if (!refreshing) refreshing = doRefresh().finally(() => { refreshing = null; });
  return refreshing;
}
async function doRefresh() {
  let s;
  try { s = await api("/api/status"); }
  catch { setLost(true); return; }
  setLost(false);
  csrf = s.csrf || csrf;
  try {
    const [services, peers, tunnels, events, files, audit, settings] = await Promise.all([
      api("/api/services"), api("/api/peers"), api("/api/tunnels"), api("/api/events"), api("/api/files"), api("/api/audit"),
      api("/api/settings"),
    ]);
    state = { status: s, services, peers, tunnels, events, files, audit, settings };
    loaded = true;
  } catch (err) {
    state.status = s; // show what we have; the rest comes on the next round
  }
  renderSidebar(); renderHome(); renderServices(); renderPeers(); renderFiles(); renderEvents(); renderSettings();
}

// Placeholders until the first answer.
for (const id of ["peer-list", "service-list", "file-list", "live-list"]) $(id).innerHTML = `<p class="muted loading">Loading…</p>`;

// Refresh every 3 s, but don't re-render a <select> or field the user is about to change.
setInterval(() => { if (!document.querySelector("select:focus, input:not([type=checkbox]):focus, textarea:focus")) refresh(); }, 3000);
if (!takePairHash()) show(location.hash.slice(1) || "home");
