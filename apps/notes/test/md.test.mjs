// Tests for ui/md.js. Run with: node --test apps/notes/test
import test from 'node:test';
import assert from 'node:assert/strict';
import { render, renderInline, safeUrl, escapeHtml } from '../ui/md.js';

const has = (h, s) => assert.ok(h.includes(s), `expected ${JSON.stringify(s)} in ${h}`);
const no = (h, s) => assert.ok(!h.includes(s), `did not expect ${JSON.stringify(s)} in ${h}`);

test('structure', () => {
  const h = render('# T\n\ntext **b** *i* ~~d~~ `c`\n\n- a\n- b\n\n1. x\n2. y\n\n> q\n\n---\n\n```js\nlet a = 1 < 2;\n```\n');
  for (const s of ['<h1', '<strong>b</strong>', '<em>i</em>', '<del>d</del>', '<code>c</code>', '<ul', '<ol', '<blockquote', '<hr', '<pre', 'language-js', '1 &lt; 2']) has(h, s);
});

test('tables, links, images, tags, wiki links', () => {
  const h = render('| A | B |\n|---|--:|\n| 1 | 2 |\n\n[x](https://e.com) ![alt](a%20b.png) #tag [[Note|alias]]', { wiki: (t) => t !== 'Note' });
  for (const s of ['<table>', 'al-r', 'href="https://e.com"', 'rel="noopener noreferrer"', '<img src="a%20b.png"', 'data-tag="tag"', 'class="wikilink missing"', '>alias<']) has(h, s);
});

test('task lists carry their source line', () => {
  const h = render('intro\n\n- [ ] one\n- [x] two\n  - [ ] sub\n');
  has(h, '<input type="checkbox" data-line="2">');
  has(h, '<input type="checkbox" data-line="3" checked>');
  has(h, '<input type="checkbox" data-line="4">');
});

test('code is never interpreted', () => {
  const h = render('`**no** #tag [[x]]`\n\n```\n# not a heading <b>\n```');
  no(h, '<strong>'); no(h, 'data-tag'); no(h, 'wikilink'); no(h, '<b>'); no(h, '<h1');
});

const XSS = [
  '<script>alert(1)</script>',
  '<img src=x onerror=alert(1)>',
  '<a href="javascript:alert(1)">x</a>',
  '[x](javascript:alert(1))',
  '[x](JaVaScRiPt:alert(1))',
  '[x]( javascript:alert(1))',
  '[x](&#106;avascript:alert(1))',
  '[x](data:text/html;base64,PHNjcmlwdD4=)',
  '[x](vbscript:msgbox(1))',
  '![x](javascript:alert(1))',
  '![x"onerror="alert(1)](a.png)',
  '[x](a.png "t\\" onmouseover=\\"alert(1)")',
  '[[a"><script>alert(1)</script>]]',
  '[[x|<img src=x onerror=alert(1)>]]',
  '#tag"><script>alert(1)</script>',
  '<https://e.com/"onmouseover="alert(1)>',
  'https://e.com/"onmouseover="alert(1)',
  '```"><script>alert(1)</script>\ncode\n```',
  '```js"onclick="alert(1)\ncode\n```',
  '| <script> | <img src=x onerror=1> |\n|---|---|\n| <b> | `<i>` |',
  '> <iframe src=javascript:alert(1)>',
  '- <svg onload=alert(1)>',
  '<!-- c --><style>*{}</style>',
  '*<b>x</b>* __<u>y</u>__',
];
test('raw HTML is escaped and no active content comes out', () => {
  for (const src of XSS) {
    const h = render(src, { href: (u) => u });
    // Whatever was typed, no tag other than the ones the renderer writes may appear, and no
    // attribute may be injected: every "<" in the output begins a known tag.
    for (const m of h.matchAll(/<\/?([a-z0-9]+)/gi)) {
      assert.ok(['p', 'h1', 'h2', 'h3', 'em', 'strong', 'del', 'code', 'pre', 'a', 'img', 'ul', 'ol', 'li', 'blockquote', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'div', 'hr', 'br', 'input'].includes(m[1].toLowerCase()), `${src} -> ${h}`);
    }
    for (const bad of ['<script', '<iframe', '<style', '<svg', '<b>', '<u>', '<i>']) no(h, bad);
    assert.ok(!/<[^>]*\son[a-z]+\s*=/i.test(h.replace(/&quot;|&#39;/g, '')), `handler attribute in ${h}`);
    assert.ok(!/(href|src)="\s*(javascript|data|vbscript):/i.test(h), `unsafe url in ${h}`);
    renderInline(src);
  }
});

test('attribute values cannot break out', () => {
  const h = render('[a](https://e.com/"x) ![b"c](p.png) [[q"r]]');
  assert.ok(!/ onmouseover|"onerror/.test(h));
  for (const m of h.matchAll(/="([^"]*)"/g)) assert.ok(!m[1].includes('"'));
});

test('safeUrl', () => {
  for (const u of ['javascript:x', ' JAVASCRIPT:x', 'java\tscript:x', 'data:text/html,x', 'vbscript:x', 'file:///etc/passwd', '']) assert.equal(safeUrl(u), null, u);
  for (const u of ['https://a.b', 'http://a', 'mailto:a@b', 'tel:1', 'rel/path.png', '#x', '../a.md']) assert.equal(safeUrl(u), u.trim(), u);
});

test('escapeHtml and renderInline', () => {
  assert.equal(escapeHtml(`<>&"'`), '&lt;&gt;&amp;&quot;&#39;');
  const h = renderInline('a *b* <b>c</b>\n d');
  has(h, '<em>b</em>'); has(h, '&lt;b&gt;'); no(h, '<b>');
});

test('hard breaks, nesting and loose lists', () => {
  has(render('a  \nb'), '<br>');
  has(render('- a\n  - b\n    - c'), '<li>a<ul><li>b<ul><li>c');
  has(render('- a\n\n- b'), '<p');
});
