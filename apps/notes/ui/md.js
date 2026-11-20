// A small Markdown renderer for Notes: a CommonMark subset (headings, emphasis, code spans and
// blocks, lists, task lists, blockquotes, links, images, tables, rules) plus [[wiki links]] and
// #tags. It has no dependencies and runs in the browser and in node (the tests).
//
// Safety is the point of the design: raw HTML in the source is never passed through. Every piece
// of source text reaches the output through escapeHtml, every URL through safeUrl (http, https,
// mailto, tel or relative only), and the only tags emitted are the ones written in this file.
// Character references such as &copy; are not decoded: the ampersand is shown as typed.
//
// render(src, opts) returns an HTML string; renderInline(src, opts) renders one line of
// inline Markdown (search snippets). Options:
//   href(url, kind)  maps a relative url (kind is "link" or "image") to the real one;
//   wiki(target)     returns false when a [[target]] names no note (it is drawn as missing).
// Task items get <input type="checkbox" data-line="N"> where N is the 0-based source line, and
// top-level blocks carry data-line, so the UI can toggle a task or sync scrolling.

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
/** Escapes text for HTML text and attribute values. */
export const escapeHtml = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

/** Returns the url when it is safe to put in href or src, else null. */
export function safeUrl(u) {
  const t = String(u ?? '').replace(/[\u0000-\u0020\u007f-\u009f\u00ad\u200b-\u200f\u2028\u2029\ufeff]/g, '');
  if (t === '') return null;
  if (/^[a-z][a-z0-9+.-]*:/i.test(t)) return /^(https?|mailto|tel):/i.test(t) ? String(u).trim() : null;
  return String(u).trim();
}

const isSpace = (c) => c === undefined || c === '' || /\s/.test(c);
const isPunct = (c) => c !== undefined && c !== '' && /[!-\/:-@\[-`{-~\p{P}\p{S}]/u.test(c);

// ---------------------------------------------------------------- inline

const TAG = /^#([\p{L}\p{N}_][\p{L}\p{N}_/-]*)/u;

function textHtml(raw, ctx, atStart) {
  // Plain text with #tags turned into tag links. A tag needs a space, "(" or the start before it.
  if (ctx.noLinks || !raw.includes('#')) return escapeHtml(raw);
  let out = '';
  let last = 0;
  for (let i = 0; i < raw.length; i++) {
    if (raw[i] !== '#') continue;
    const prev = i === 0 ? (atStart ? ' ' : 'x') : raw[i - 1];
    if (!(isSpace(prev) || prev === '(' || prev === '[')) continue;
    const m = TAG.exec(raw.slice(i));
    if (!m) continue;
    const name = m[1].replace(/[/-]+$/, '');
    if (!name || /^\p{N}+$/u.test(name)) continue;
    out += escapeHtml(raw.slice(last, i)) + `<a class="tag" href="#" data-tag="${escapeHtml(name.toLowerCase())}">#${escapeHtml(name)}</a>`;
    last = i + 1 + name.length;
    i = last - 1;
  }
  return out + escapeHtml(raw.slice(last));
}

/** Finds the ']' that closes the '[' at s[i]; -1 if none. Skips escapes and code spans. */
function closeBracket(s, i) {
  let depth = 0;
  for (let j = i; j < s.length; j++) {
    const c = s[j];
    if (c === '\\') { j++; continue; }
    if (c === '`') {
      let n = 0;
      while (s[j + n] === '`') n++;
      const end = s.indexOf('`'.repeat(n), j + n);
      if (end >= 0) j = end + n - 1; else j += n - 1;
      continue;
    }
    if (c === '[') depth++;
    else if (c === ']' && --depth === 0) return j;
  }
  return -1;
}

/** Parses "(dest "title")" starting at s[i] === '('. Returns {dest, title, end} or null. */
function parseDest(s, i) {
  let j = i + 1;
  while (s[j] === ' ' || s[j] === '\n') j++;
  let dest = '';
  if (s[j] === '<') {
    const e = s.indexOf('>', j);
    if (e < 0 || s.slice(j, e).includes('\n')) return null;
    dest = s.slice(j + 1, e);
    j = e + 1;
  } else {
    let depth = 0;
    const st = j;
    for (; j < s.length; j++) {
      const c = s[j];
      if (c === '\\') { j++; continue; }
      if (/\s/.test(c)) break;
      if (c === '(') depth++;
      else if (c === ')') { if (depth === 0) break; depth--; }
    }
    dest = s.slice(st, j).replace(/\\([!-\/:-@\[-`{-~])/g, '$1');
  }
  let title = '';
  while (s[j] === ' ' || s[j] === '\n') j++;
  if (s[j] === '"' || s[j] === "'" || s[j] === '(') {
    const close = s[j] === '(' ? ')' : s[j];
    const e = s.indexOf(close, j + 1);
    if (e < 0) return null;
    title = s.slice(j + 1, e);
    j = e + 1;
    while (s[j] === ' ' || s[j] === '\n') j++;
  }
  return s[j] === ')' ? { dest, title, end: j + 1 } : null;
}

function inline(s, ctx) {
  const toks = [];
  let buf = '';
  let bufStart = true;
  const flush = () => {
    if (buf) toks.push({ t: 'text', v: buf, start: bufStart });
    buf = '';
    bufStart = false;
  };
  const html = (v) => { flush(); toks.push({ t: 'html', v }); };
  const n = s.length;
  for (let i = 0; i < n;) {
    const c = s[i];
    if (c === '\\' && i + 1 < n) {
      if (s[i + 1] === '\n') { html('<br>'); i += 2; continue; }
      if (/[!-\/:-@\[-`{-~]/.test(s[i + 1])) { buf += s[i + 1]; i += 2; continue; }
    }
    if (c === '`') {
      let k = 0;
      while (s[i + k] === '`') k++;
      // The closing run must be exactly k backticks long.
      let e = -1;
      for (let j = i + k; j < n;) {
        if (s[j] !== '`') { j++; continue; }
        let r = j;
        while (s[r] === '`') r++;
        if (r - j === k) { e = j; break; }
        j = r;
      }
      if (e >= 0) {
        let code = s.slice(i + k, e).replace(/\n/g, ' ');
        if (code.length > 2 && code.startsWith(' ') && code.endsWith(' ') && code.trim()) code = code.slice(1, -1);
        html(`<code>${escapeHtml(code)}</code>`);
        i = e + k;
        continue;
      }
      buf += '`'.repeat(k);
      i += k;
      continue;
    }
    if (c === '<') {
      const m = /^<((?:https?:\/\/|mailto:)[^\s<>]*)>/i.exec(s.slice(i));
      if (m && !ctx.noLinks) {
        html(`<a href="${escapeHtml(m[1])}" target="_blank" rel="noopener noreferrer">${escapeHtml(m[1])}</a>`);
        i += m[0].length;
        continue;
      }
    }
    if (c === '[' && s[i + 1] === '[') {
      const m = /^\[\[([^\[\]\n]+?)\]\]/.exec(s.slice(i));
      if (m) {
        const [target, alias] = m[1].split('|');
        const name = target.split('#')[0].trim();
        const label = (alias ?? target).trim();
        if (ctx.noLinks || !name) buf += label;
        else {
          const missing = ctx.opts.wiki && ctx.opts.wiki(name) === false;
          html(`<a class="wikilink${missing ? ' missing' : ''}" href="#" data-wiki="${escapeHtml(name)}">${escapeHtml(label)}</a>`);
        }
        i += m[0].length;
        continue;
      }
    }
    if (c === '[' || (c === '!' && s[i + 1] === '[')) {
      const img = c === '!';
      const open = img ? i + 1 : i;
      const close = closeBracket(s, open);
      const dest = close >= 0 && s[close + 1] === '(' ? parseDest(s, close + 1) : null;
      if (dest) {
        const label = s.slice(open + 1, close);
        const url = safeUrl(dest.dest);
        const title = dest.title ? ` title="${escapeHtml(dest.title)}"` : '';
        if (img) {
          const alt = label.replace(/[*_`~\[\]!\\]/g, '');
          if (url === null) buf += alt;
          else {
            const src = ctx.opts.href ? ctx.opts.href(url, 'image') : url;
            html(`<img src="${escapeHtml(src)}" alt="${escapeHtml(alt)}"${title} loading="lazy">`);
          }
        } else if (ctx.noLinks || url === null) {
          html(inline(label, { ...ctx, noLinks: true }));
        } else {
          const external = /^(https?:|mailto:|tel:)/i.test(url);
          const href = external ? url : (ctx.opts.href ? ctx.opts.href(url, 'link') : url);
          html(`<a href="${escapeHtml(href)}"${title}${external ? ' target="_blank" rel="noopener noreferrer"' : ''}>${inline(label, { ...ctx, noLinks: true })}</a>`);
        }
        i = dest.end;
        continue;
      }
      if (img) { buf += '!'; i++; continue; }
      buf += '[';
      i++;
      continue;
    }
    if (c === 'h' && !ctx.noLinks && !/[\p{L}\p{N}]/u.test(s[i - 1] ?? ' ')) {
      const m = /^https?:\/\/[^\s<]+/i.exec(s.slice(i));
      if (m) {
        let url = m[0].replace(/[.,;:!?'"*_~]+$/, '');
        while (url.endsWith(')') && (url.match(/\)/g) || []).length > (url.match(/\(/g) || []).length) url = url.slice(0, -1);
        if (url.length > 8) {
          html(`<a href="${escapeHtml(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(url)}</a>`);
          i += url.length;
          continue;
        }
      }
    }
    if (c === '*' || c === '_' || c === '~') {
      let k = 0;
      while (s[i + k] === c) k++;
      if (c === '~' && k < 2) { buf += c.repeat(k); i += k; continue; }
      const prev = i === 0 ? '' : s[i - 1];
      const next = s[i + k];
      const left = !isSpace(next) && (!isPunct(next) || isSpace(prev) || isPunct(prev));
      const right = !isSpace(prev) && (!isPunct(prev) || isSpace(next) || isPunct(next));
      const canOpen = c === '_' ? left && (!right || isPunct(prev)) : left;
      const canClose = c === '_' ? right && (!left || isPunct(next)) : right;
      flush();
      toks.push({ t: 'delim', ch: c, n: k, canOpen, canClose });
      i += k;
      continue;
    }
    if (c === '\n') {
      const m = /( +)$/.exec(buf);
      if (m && m[1].length >= 2) { buf = buf.slice(0, -m[1].length); html('<br>'); } else buf = buf.replace(/ +$/, '') + '\n';
      i++;
      while (s[i] === ' ') i++;
      continue;
    }
    buf += c;
    i++;
  }
  flush();

  // Emphasis: match each closing run with the nearest opening run of the same character.
  const wrap = { '*': ['em', 'strong'], _: ['em', 'strong'], '~': [null, 'del'] };
  const render1 = (t) => (t.t === 'text' ? textHtml(t.v, ctx, t.start) : t.t === 'html' ? t.v : t.ch.repeat(t.n));
  for (let ci = 0; ci < toks.length; ci++) {
    const cl = toks[ci];
    if (cl.t !== 'delim' || !cl.canClose) continue;
    let oi = ci - 1;
    while (oi >= 0 && !(toks[oi].t === 'delim' && toks[oi].ch === cl.ch && toks[oi].canOpen && toks[oi].n > 0)) oi--;
    if (oi < 0) continue;
    const op = toks[oi];
    if (cl.ch === '~' && (op.n < 2 || cl.n < 2)) continue;
    const use = cl.ch === '~' ? 2 : op.n >= 2 && cl.n >= 2 ? 2 : 1;
    const tag = wrap[cl.ch][use - 1];
    const inner = toks.slice(oi + 1, ci).map(render1).join('');
    op.n -= use;
    cl.n -= use;
    toks.splice(oi + 1, ci - oi - 1, { t: 'html', v: `<${tag}>${inner}</${tag}>` });
    ci = oi + 1; // the closer is now right after the new node
    if (cl.n > 0) ci--; // try it again against earlier openers
  }
  return toks.filter((t) => !(t.t === 'delim' && t.n === 0)).map(render1).join('');
}

// ---------------------------------------------------------------- blocks

const FENCE = /^ {0,3}(`{3,}|~{3,})[ \t]*([^`\s]*)[^`]*$/;
const ATX = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/;
const HR = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const QUOTE = /^ {0,3}>/;
const BULLET = /^( {0,3})([-*+])( +|$)/;
const ORDERED = /^( {0,3})(\d{1,9})([.)])( +|$)/;
const SETEXT = /^ {0,3}(=+|-+)[ \t]*$/;
const TABLESEP = /^[ \t]*\|?[ \t]*:?-+:?[ \t]*(\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/;

const blank = (t) => t.trim() === '';
const indentOf = (t) => t.length - t.trimStart().length;

function startsBlock(t) {
  if (FENCE.test(t) || ATX.test(t) || HR.test(t) || QUOTE.test(t)) return true;
  let m = BULLET.exec(t);
  if (m && t.slice(m[0].length).trim()) return true;
  m = ORDERED.exec(t);
  return !!(m && m[2] === '1' && t.slice(m[0].length).trim());
}

function splitRow(t) {
  let s = t.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const cells = [];
  let cur = '';
  for (let i = 0; i < s.length; i++) {
    if (s[i] === '\\' && s[i + 1] === '|') { cur += '|'; i++; } else if (s[i] === '`') {
      const e = s.indexOf('`', i + 1);
      if (e >= 0) { cur += s.slice(i, e + 1); i = e; } else cur += '`';
    } else if (s[i] === '|') { cells.push(cur.trim()); cur = ''; } else cur += s[i];
  }
  cells.push(cur.trim());
  return cells;
}

function itemMatch(t, ordered, marker) {
  const m = ordered ? ORDERED.exec(t) : BULLET.exec(t);
  if (!m || (ordered ? m[3] : m[2]) !== marker) return null;
  const markerLen = (ordered ? m[2].length + 1 : 1);
  const rest = t.slice(m[0].length);
  const spaces = m[ordered ? 4 : 3].length;
  const contentIndent = !rest.trim() || spaces > 4 ? m[1].length + markerLen + 1 : m[0].length;
  return { indent: m[1].length, contentIndent, rest: !rest.trim() ? '' : spaces > 4 ? t.slice(m[1].length + markerLen + 1) : rest };
}

function blocks(lines, ctx) {
  const out = [];
  const top = ctx.depth === 0;
  const at = (n) => (top ? ` data-line="${n}"` : '');
  let i = 0;
  while (i < lines.length) {
    const L = lines[i];
    const t = L.t;
    if (blank(t)) { i++; continue; }

    let m = FENCE.exec(t);
    if (m) {
      const fence = m[1];
      const lang = m[2].replace(/[^\w+#.-]/g, '');
      const body = [];
      i++;
      const closer = new RegExp('^ {0,3}' + (fence[0] === '`' ? '`' : '~') + '{' + fence.length + ',}[ \\t]*$');
      while (i < lines.length && !closer.test(lines[i].t)) body.push(lines[i++].t);
      i++;
      out.push(`<pre${at(L.n)}><code${lang ? ` class="language-${escapeHtml(lang)}"` : ''}>${escapeHtml(body.join('\n'))}${body.length ? '\n' : ''}</code></pre>`);
      continue;
    }
    if (HR.test(t)) { out.push(`<hr${at(L.n)}>`); i++; continue; }
    m = ATX.exec(t);
    if (m) {
      const lv = m[1].length;
      out.push(`<h${lv}${at(L.n)}>${inline((m[2] || '').trim(), ctx)}</h${lv}>`);
      i++;
      continue;
    }
    if (QUOTE.test(t)) {
      const inner = [];
      while (i < lines.length) {
        const q = lines[i].t;
        if (QUOTE.test(q)) inner.push({ t: q.replace(/^ {0,3}> ?/, ''), n: lines[i].n });
        else if (!blank(q) && inner.length && !blank(inner[inner.length - 1].t) && !startsBlock(q)) inner.push({ t: q, n: lines[i].n });
        else break;
        i++;
      }
      out.push(`<blockquote${at(L.n)}>${blocks(inner, { ...ctx, depth: ctx.depth + 1, tight: false })}</blockquote>`);
      continue;
    }
    const bm = BULLET.exec(t);
    const om = ORDERED.exec(t);
    if (bm || om) {
      const r = list(lines, i, ctx, !!om, om ? om[3] : bm[2], om ? +om[2] : 1, at(L.n));
      out.push(r.html);
      i = r.next;
      continue;
    }
    if (indentOf(t) >= 4) {
      const body = [];
      while (i < lines.length && (blank(lines[i].t) || indentOf(lines[i].t) >= 4)) body.push(lines[i++].t.slice(4));
      while (body.length && blank(body[body.length - 1])) body.pop();
      out.push(`<pre${at(L.n)}><code>${escapeHtml(body.join('\n'))}\n</code></pre>`);
      continue;
    }
    if (t.includes('|') && i + 1 < lines.length && TABLESEP.test(lines[i + 1].t) && lines[i + 1].t.includes('-')) {
      const head = splitRow(t);
      const seps = splitRow(lines[i + 1].t);
      if (head.length === seps.length && (t.includes('|') && (lines[i + 1].t.includes('|') || head.length === 1))) {
        const al = seps.map((c) => (c.startsWith(':') && c.endsWith(':') ? 'c' : c.endsWith(':') ? 'r' : c.startsWith(':') ? 'l' : ''));
        const cell = (tag, c, k) => `<${tag}${al[k] ? ` class="al-${al[k]}"` : ''}>${inline(c, ctx)}</${tag}>`;
        const rows = [];
        i += 2;
        while (i < lines.length && !blank(lines[i].t) && !startsBlock(lines[i].t)) {
          const cells = splitRow(lines[i].t);
          rows.push('<tr>' + head.map((_, k) => cell('td', cells[k] ?? '', k)).join('') + '</tr>');
          i++;
        }
        out.push(`<div class="table-wrap"${at(L.n)}><table><thead><tr>${head.map((c, k) => cell('th', c, k)).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table></div>`);
        continue;
      }
    }
    // Paragraph (or setext heading).
    const para = [L];
    i++;
    let setext = 0;
    while (i < lines.length && !blank(lines[i].t)) {
      const q = lines[i].t;
      const s = SETEXT.exec(q);
      if (s) { setext = s[1][0] === '=' ? 1 : 2; i++; break; }
      if (startsBlock(q)) break;
      para.push(lines[i++]);
    }
    const text = para.map((p) => p.t.trimStart()).join('\n').trimEnd();
    if (setext) out.push(`<h${setext}${at(L.n)}>${inline(text, ctx)}</h${setext}>`);
    else if (ctx.tight) out.push(inline(text, ctx));
    else out.push(`<p${at(L.n)}>${inline(text, ctx)}</p>`);
  }
  return out.join(ctx.tight ? '' : '\n');
}

function list(lines, i, ctx, ordered, marker, start, attr) {
  const items = [];
  let loose = false;
  while (i < lines.length) {
    const im = itemMatch(lines[i].t, ordered, marker);
    if (!im || HR.test(lines[i].t)) break;
    const inner = [{ t: im.rest, n: lines[i].n }];
    let j = i + 1;
    let pending = [];
    while (j < lines.length) {
      const t = lines[j].t;
      if (blank(t)) { pending.push(lines[j]); j++; continue; }
      const ind = indentOf(t);
      const nested = ind >= 2 && ind > im.indent && (BULLET.test(t.trimStart()) || ORDERED.test(t.trimStart()));
      if (ind >= im.contentIndent || nested) {
        if (pending.length) { loose = loose || !FENCE.test(inner[inner.length - 1].t); for (const p of pending) inner.push({ t: '', n: p.n }); }
        pending = [];
        inner.push({ t: t.slice(Math.min(ind, im.contentIndent)), n: lines[j].n });
        j++;
      } else if (!pending.length && !blank(inner[inner.length - 1].t) && !startsBlock(t) && !BULLET.test(t) && !ORDERED.test(t)) {
        inner.push({ t: t.trim(), n: lines[j].n });
        j++;
      } else break;
    }
    if (pending.length && j < lines.length && itemMatch(lines[j].t, ordered, marker)) loose = true;
    items.push(inner);
    i = j;
  }
  const tag = ordered ? 'ol' : 'ul';
  const lis = items.map((inner) => {
    let task = null;
    const tm = /^\[([ xX])\](?:[ \t]+|$)(.*)$/.exec(inner[0].t);
    if (tm) { task = { checked: tm[1] !== ' ', n: inner[0].n }; inner[0] = { t: tm[2], n: inner[0].n }; }
    const body = blocks(inner, { ...ctx, depth: ctx.depth + 1, tight: !loose });
    const box = task ? `<input type="checkbox" data-line="${task.n}"${task.checked ? ' checked' : ''}> ` : '';
    return `<li${task ? ' class="task"' : ''}>${box}${body}</li>`;
  });
  const startAttr = ordered && start !== 1 ? ` start="${start}"` : '';
  return { html: `<${tag}${startAttr}${attr}>${lis.join('')}</${tag}>`, next: i };
}

const expandTabs = (t) => t.replace(/^[ \t]+/, (w) => w.replace(/\t/g, '    '));

/** Renders Markdown to safe HTML. */
export function render(src, opts = {}) {
  let lines = String(src ?? '').replace(/\r\n?/g, '\n').split('\n').map((t, n) => ({ t: expandTabs(t), n }));
  if (lines.length && lines[0].t.trim() === '---') {
    const end = lines.findIndex((l, k) => k > 0 && (l.t.trim() === '---' || l.t.trim() === '...'));
    if (end > 0) lines = lines.slice(end + 1);
  }
  return blocks(lines, { opts, depth: 0, tight: false, noLinks: false });
}

/** Renders one line of inline Markdown (a search snippet, a title). */
export function renderInline(src, opts = {}) {
  return inline(String(src ?? '').replace(/\s+/g, ' ').trim(), { opts, noLinks: false });
}
