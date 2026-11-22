# Sol kit: tokens and hooks (the theme contract)

Everything here is public and stable. Apps style only through these tokens and kit components;
a theme is any CSS that sets these tokens, so one theme restyles every app, including third-party
ones. Names are never repurposed: new tokens are added, old ones are deprecated before removal.
See `design/UI.md` for the design intent.

Files: `tokens.css` (tokens), `kit.css` (components), `kit.js` (custom elements and helpers),
`icons.js`, `fonts/` (Inter, OFL), `themes/` (built-in themes). Load order in a page:
`tokens.css`, `kit.css`, the app's own CSS, then `/_sol/theme.css` (added last by `kit.js`).

## Colour scheme

Plain custom properties plus `color-scheme`; no `light-dark()` (Chrome 123+ only, and Android
WebViews are often older: the kit targets Chromium 100+ / Safari 15.4+). Light values live in
`:root`; dark values live in two selectors, `@media (prefers-color-scheme: dark) {
:root:not([data-theme="light"]) { ... } }` and `:root[data-theme="dark"] { ... }`. Auto follows the
system; `<html data-theme="light|dark">` forces one. Themes set both the same way (see the theme
format below). Modern CSS in the kit always has a solid fallback declared first or lives in an
`@supports` block; a Go test enforces it.

## Colour tokens

| Token | Light | Dark | Meaning |
|---|---|---|---|
| `--canvas` | `#F5F5F7` | `#000000` | window background |
| `--surface` | `#FFFFFF` | `#1C1C1E` | grouped lists, cards, sheets |
| `--surface-2` | `#F2F2F7` | `#2C2C2E` | fields, segmented controls, hover |
| `--glass` | white 72% | `#1C1C1E` 68% | sidebar, toolbars, tab bar, menus, sheets |
| `--glass-filter` | `blur(24px) saturate(180%)` | same | `backdrop-filter` for glass; `none` when glass is off |
| `--glass-edge` | white 50% | white 8% | 0.5px inner highlight on glass |
| `--label` / `--label-2` / `--label-3` | `#1D1D1F` / 60% / 30% | `#F5F5F7` / 60% / 30% | primary, secondary, placeholder text |
| `--separator` | `rgba(60,60,67,.18)` | `rgba(84,84,88,.6)` | hairlines |
| `--scrim` | black 32% | black 60% | behind sheets and dialogs |
| `--tint` | `#E8A100` | `#FFB524` | the accent (Sol gold): primary buttons, selection, toggles, focus |
| `--green` `--red` `--blue` `--orange` | iOS system colours | iOS dark variants | online/ok, destructive/error, info, warning. Status only |

### Accent derivation

Set `--tint` and nothing else; these follow. Where `color-mix()` exists they are computed live
from it (an `@supports` block in `tokens.css`), so they follow any theme or accent setting; older
browsers get solid values precomputed for the default tint (a theme can ship its own under
`@supports not (color: color-mix(in srgb, red, blue))`, as the built-ins do):

| Token | Derived as |
|---|---|
| `--tint-hover`, `--tint-pressed` | `--tint` moved 9% / 18% toward `--tint-mix` (black in light, white in dark) |
| `--tint-subtle`, `--tint-subtle-hover` | `--tint` at 16% / 24% over transparent (tinted button fill, selected sidebar row) |
| `--tint-text` | `--tint` mixed 38% toward `--label`: accent used as text on `--surface` |
| `--on-tint` | black or white chosen from the accent's lightness (OKLCH); falls back to `#1D1D1F` |
| `--focus-ring` | `--tint` at 70% |
| `--surface-hover`, `--surface-press` | `--surface-2` moved 7% / 5% toward `--label` (button hover, pressed rows) |

Also derived: `--red-subtle`, `--green-subtle`, `--orange-subtle`. A theme may override any
derived token directly if it wants an exact shade.

## Type

`--font-ui` (default `-apple-system, BlinkMacSystemFont, "SF Pro Text", "Inter Sol", system-ui,
Roboto, sans-serif`), `--font-mono`. Eight styles, each with `--fs-*` size, `--lh-*` line height,
`--fw-*` weight, `--tr-*` tracking: `large-title`, `title-1`, `title-2`, `headline`, `body`,
`subhead`, `footnote`, `caption`. Desktop values by default; at 760px and narrower the iOS sizes
apply (large title 34/41, body 17/22, ...). Classes `.t-large-title ... .t-caption` apply a style.
`"Inter Sol"` is the vendored variable Inter (`fonts/`), used when the system has no SF.

## Shape, spacing, motion

| Token | Default | Meaning |
|---|---|---|
| `--radius-control` | 8px (phone 10px) | buttons, fields, segmented |
| `--radius-group` | 12px | list groups, cards, menus |
| `--radius-sheet` | 16px | sheets, dialogs, widgets |
| `--radius-pill`, `--radius-icon` | 999px, 22.37% | pills; the app icon squircle |
| `--space-1..8` | 4, 8, 12, 16, 24, 32, 48, 72px | 4px grid |
| `--control-h`, `--row-h`, `--row-pad` | 28 / 32 / 12px (phone 44 / 44 / 16px) | density-sensitive sizes |
| `--bar-h`, `--tab-h`, `--sidebar-w` | 52px (phone 44), 50px, 220px | shell chrome |
| `--hairline` | 0.5px | separators |
| `--shadow-1`, `--shadow-float` | soft | floating things only |
| `--ease-spring` | `cubic-bezier(.32,.72,0,1)` | sheets, windows, toggles |
| `--dur-press`, `--dur-fade`, `--dur-sheet`, `--press-scale` | 120ms, 200ms, 400ms, .97 | all go to 0 / 1 under `prefers-reduced-motion` |

## Settings (data-attributes on `<html>`)

Set by the Sol app (or `kit.js applySettings()` / `sol.js applyThemeSettings()`):

| Attribute | Values | Effect |
|---|---|---|
| `data-theme` | `light`, `dark`, absent = auto | colour scheme |
| `data-density` | `compact`; absent = comfortable | smaller `--control-h`, `--row-h`, spacing |
| `data-radius` | `square`; absent = rounded | `--radius-control/group/sheet` shrink to 2-4px |
| `data-glass` | `off`; absent = on | `--glass` becomes `--surface`, no blur. `prefers-reduced-transparency` does the same |
| `data-font` | `inter`, `mono` (`system` sets nothing: the theme's own font) | `--font-ui` |
| inline `--tint` | any colour | the accent setting |

The `theme` event payload may carry them: `{"settings": {"scheme": "dark", "accent": "#b58900",
"density": "compact", "radius": "square", "glass": "off", "font": "inter"}}`.

## Class and data hooks (for deep theming)

Elements: `<sol-app-shell>` (children `nav[slot=nav]`, `main`; generated `.shell-bar`
with `.bar-title`, `.bar-actions`, `.sidebar-toggle`), `<sol-nav>` (the Sol home button; the shell adds one, plus a `.sol-group` of `.nav-sol` links atop the sidebar; `[data-nonav]` on the shell means no sidebar; `.nav-sheet-item` in the phone sheet), `<sol-list>`, `<sol-dialog>`/`<sol-sheet>`
(`.grabber`, `.dlg-head`, `.dlg-body`, `.dlg-actions`), `<sol-toast>` (`.toast[data-kind]`),
`<sol-avatar online>`, `<sol-app-icon glyph hue>`, `<sol-ring value>`, `<sol-icon name>`.

Classes: `.glass`, `.card`, `.list`, `.group-title`, `.group-footer`, `.nav-item[aria-current=page]`,
`.large-title`, `.toolbar`, `.row-title`, `.row-sub`, `.row-value`, `.row-icon`, `.swipe-actions`, `.segmented`,
`.chips`, `.badge(.tint|.ok|.warn|.err)`, `.banner`, `.empty`, `.skeleton`, `.menu`, `.popover`,
`.widget(-title|-value|-sub)`, buttons `.primary .tinted .plain .ghost .danger .icon-btn .large`,
switch `input[type=checkbox][role=switch]`, `.muted`, `.grow`.

Data hooks: `[data-item]` (a list row), `[data-chevron]` (disclosure arrow), `[aria-selected=true]`,
`[aria-pressed=true]`, `[data-collapsed]` and `[data-sidebar=hidden]` on the shell, `[data-swipe]`
on a row, `[data-app="id"]` on `<html>` or `<body>` for per-app rules (kit.js sets it from the app origin).

Deprecated aliases from kit v1 (`--bg`, `--ink`, `--primary`, `--sun`, `--line`, `--raised`,
`--text-*`, `--radius-sm/md/lg`, ...) still resolve, mapped onto the tokens above. Do not use them.

## Theme file format

A theme is one `.css` file in `SolDrive/.sol/themes/`. The header comment is read by the Sol app
for the theme list:

```css
/* @theme    Name shown in settings
   @author   Who made it
   @supports light dark */      /* any of: light, dark */
```

Rules: CSS only (no scripts, no `@import` of remote URLs: fonts and images load from the app
origin and `data:` only). Override light tokens in `:root` and dark tokens in the two dark selectors
(`:root:not([data-theme="light"])` inside `@media (prefers-color-scheme: dark)`, and
`:root[data-theme="dark"]`); never `light-dark()`. A light-only or dark-only theme puts its values
in `:root` and only overrides the other scheme under `[data-theme]`; see `themes/paper.css` and
`themes/terminal.css`.
Anything else (component rules, `[data-app="music"] .now-playing { ... }`) is allowed.
Themes load after the kit, so equal specificity wins.

### Your own theme

```css
/* @theme   Solarvim
   @author  APV
   @supports light dark */
:root {
  --tint: #b58900;                     /* hover, pressed, subtle, on-tint all follow */
  --font-ui: "Iosevka", monospace;
  --radius-control: 2px;
  --canvas: #fdf6e3;
  --surface: #eee8d5;
  --label: #073642;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) { --canvas: #002b36; --surface: #073642; --label: #eee8d5; }
}
:root[data-theme="dark"] { --canvas: #002b36; --surface: #073642; --label: #eee8d5; }
/* deep hacking, optional */
[data-app="notes"] .md a { text-decoration: underline; }
```

Built-ins live in `themes/`: `sol.css`, `graphite.css`, `terminal.css`, `paper.css`.
The demo (`demo.html`) switches between them.
