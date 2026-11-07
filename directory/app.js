// Sol directory: static page over a signed hosts.json. No backend.
"use strict";
let HOSTS = [];
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
        .map(([k, v]) => `<div class="mono">${k}: ${v}</div>`).join("");
      const tags = (h.tags || []).map(t => `<span class="tag">${t}</span>`).join("");
      const st = h.status || "down";
      return `<li class="host"><b>${h.name}</b>
        <span class="dot ${st}"></span><span class="muted">${h.kind}</span>
        <div>${h.description || ""}</div>${addrs}<div>${tags}</div></li>`;
    }).join("") || "<li class='muted'>No matching hosts.</li>";
}
document.getElementById("kind").onchange = render;
document.getElementById("net").onchange = render;
load();
