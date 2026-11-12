// Sol directory: static page over a signed hosts.json. No backend.
"use strict";
let HOSTS = [];
// Every value from hosts.json goes through esc() before it is put into HTML:
// entries are written by strangers.
function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, c =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
const STATES = ["up", "slow", "down"]; // the classes style.css knows
async function load() {
  const r = await fetch("hosts.json");
  const data = await r.json();
  HOSTS = data.hosts || [];   // data is signed by the directory key; verification is a v0.2 client step
  render();
}
function render() {
  const kind = document.getElementById("kind").value;
  const net = document.getElementById("net").value;
  const ul = document.getElementById("hosts");
  ul.innerHTML = HOSTS.filter(h =>
      (!kind || h.kind === kind) &&
      (!net || (h.addresses && h.addresses[net])))
    .map(h => {
      const addrs = Object.entries(h.addresses || {})
        .map(([k, v]) => `<div class="mono">${esc(k)}: ${esc(v)}</div>`).join("");
      const tags = (h.tags || []).map(t => `<span class="tag">${esc(t)}</span>`).join("");
      const st = STATES.includes(h.status) ? h.status : "down";
      return `<li class="host"><b>${esc(h.name)}</b>
        <span class="dot ${st}"></span><span class="muted">${esc(h.kind)}</span>
        <div>${esc(h.description)}</div>${addrs}<div>${tags}</div></li>`;
    }).join("") || "<li class='muted'>No matching hosts.</li>";
}
document.getElementById("kind").onchange = render;
document.getElementById("net").onchange = render;
load();
