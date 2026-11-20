// <sol-qr value="sol://join?..."> renders a QR code as an inline SVG, for pairing and joining
// links. It uses the vendored qrcode-generator (ui/kit/vendor/qrcode.js, MIT), loaded once as a
// classic script because it defines a global rather than exporting a module. Modules are
// drawn as one <path>, coloured with currentColor on a --surface background, so themes apply.
//
// Attributes: value (the text to encode), ecc (L|M|Q|H, default M), label (accessible name).

let loading = null;

function loadEncoder() {
  if (globalThis.qrcode) return Promise.resolve(globalThis.qrcode);
  if (!loading) {
    loading = new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = new URL('./vendor/qrcode.js', import.meta.url).href;
      s.onload = () => (globalThis.qrcode ? resolve(globalThis.qrcode) : reject(new Error('qrcode.js did not load')));
      s.onerror = () => reject(new Error('qrcode.js failed to load'));
      document.head.append(s);
    });
  }
  return loading;
}

/** Returns the module grid (array of rows of booleans) for text, or throws if it can't fit. */
export async function qrMatrix(text, ecc = 'M') {
  const qrcode = await loadEncoder();
  const qr = qrcode(0, ecc); // 0 = smallest version that fits
  qr.addData(text, 'Byte');
  qr.make();
  const n = qr.getModuleCount();
  const rows = [];
  for (let r = 0; r < n; r++) {
    const row = [];
    for (let c = 0; c < n; c++) row.push(qr.isDark(r, c));
    rows.push(row);
  }
  return rows;
}

/** Builds the SVG path data for a module grid with a 4-module quiet zone. */
export function qrPath(rows) {
  let d = '';
  rows.forEach((row, r) => row.forEach((dark, c) => { if (dark) d += `M${c + 4} ${r + 4}h1v1h-1z`; }));
  return d;
}

class SolQR extends HTMLElement {
  static get observedAttributes() { return ['value', 'ecc', 'label']; }
  connectedCallback() { this.draw(); }
  attributeChangedCallback() { if (this.isConnected) this.draw(); }
  async draw() {
    const value = this.getAttribute('value') || '';
    const svgNS = 'http://www.w3.org/2000/svg';
    this.replaceChildren();
    if (!value) return;
    try {
      const rows = await qrMatrix(value, this.getAttribute('ecc') || 'M');
      const size = rows.length + 8;
      const svg = document.createElementNS(svgNS, 'svg');
      svg.setAttribute('viewBox', `0 0 ${size} ${size}`);
      svg.setAttribute('role', 'img');
      svg.setAttribute('aria-label', this.getAttribute('label') || 'QR code');
      svg.setAttribute('shape-rendering', 'crispEdges');
      svg.style.display = 'block';
      svg.style.width = '100%';
      svg.style.height = 'auto';
      const bg = document.createElementNS(svgNS, 'rect');
      bg.setAttribute('width', size);
      bg.setAttribute('height', size);
      bg.setAttribute('fill', '#fff'); // QR scanners need light quiet zone in every theme
      const p = document.createElementNS(svgNS, 'path');
      p.setAttribute('d', qrPath(rows));
      p.setAttribute('fill', '#000');
      svg.append(bg, p);
      this.append(svg);
    } catch (e) {
      const msg = document.createElement('p');
      msg.textContent = `Can't show a QR code: ${e.message}`;
      this.append(msg);
    }
  }
}

if (!customElements.get('sol-qr')) customElements.define('sol-qr', SolQR);
