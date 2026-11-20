# UI direction

Every app looks and feels like a well-made Apple app: macOS on desktop, iOS on phones, and the
Sol app's home screen is in the spirit of umbrelOS (a wallpaper, app icons, a few widgets).
Clean, calm, generous, fast. The one thing that is Sol's own: a **gold accent** (the sun) and the
**dusk wallpaper** on the home screen. Everything else stays quiet and disciplined.

`ui/kit` implements all of this; apps use its tokens and components and add almost no CSS.

## Colour

Semantic tokens, iOS-style, with real dark mode (true black canvas, elevated greys).

| Token | Light | Dark | Use |
|---|---|---|---|
| `--canvas` | `#F5F5F7` | `#000000` | window background |
| `--surface` | `#FFFFFF` | `#1C1C1E` | grouped lists, cards, sheets |
| `--surface-2` | `#F2F2F7` | `#2C2C2E` | fields, segmented controls, hover |
| `--glass` | `rgba(255,255,255,.72)` + blur 24px, saturate 180% | `rgba(28,28,30,.68)` + same | sidebar, toolbars, tab bar, menus, sheets over content |
| `--label` | `#1D1D1F` | `#F5F5F7` | primary text |
| `--label-2` | `rgba(60,60,67,.60)` | `rgba(235,235,245,.60)` | secondary text |
| `--label-3` | `rgba(60,60,67,.30)` | `rgba(235,235,245,.30)` | placeholder, disabled |
| `--separator` | `rgba(60,60,67,.18)` | `rgba(84,84,88,.6)` | hairlines (0.5px on 2x screens) |
| `--tint` | `#E8A100` | `#FFB524` | Sol gold: primary buttons, selection, links, focus, toggles on |
| `--on-tint` | `#1D1D1F` | `#000000` | text on gold |
| `--green` / `--red` / `--blue` | `#34C759` / `#FF3B30` / `#007AFF` | `#30D158` / `#FF453A` / `#0A84FF` | online, destructive, info only |

Shadows are layered and soft, used only on things that float (menus, sheets, popovers, the dock):
`0 1px 2px rgba(0,0,0,.06), 0 8px 24px rgba(0,0,0,.12)` (dark: higher opacity, plus a 0.5px
inner highlight on glass).

## Type

The system font stack, so it is SF Pro on Apple devices and Roboto on Android; on Linux and
Windows the kit ships **Inter** (OFL, variable `woff2`, vendored in `ui/kit/fonts/` with its
licence and checksum) as the fallback, because it is the closest open match to SF.

`font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "Inter Sol", system-ui, Roboto, sans-serif`

| Style | Phone | Desktop | Weight | Tracking |
|---|---|---|---|---|
| Large title | 34/41 | 26/32 | 700 | -0.02em |
| Title 1 | 28/34 | 22/28 | 700 | -0.015em |
| Title 2 | 22/28 | 17/22 | 600 | -0.01em |
| Headline | 17/22 | 13/16 | 600 | -0.005em |
| Body | 17/22 | 13/18 | 400 | 0 |
| Subhead | 15/20 | 12/16 | 400 | 0 |
| Footnote / caption | 13/18, 12/16 | 11/14 | 400 | 0 |

Sentence case everywhere. No all-caps labels, no eyebrows above headings.

## Shape and spacing

- 4px grid. Radii by role, not one radius for everything: controls 8px (desktop) / 10px
  (phone), list groups and cards 12px, sheets 16px top corners, app icons a **squircle**
  (`border-radius: 22.37%`, with `clip-path` superellipse where supported), pills 999px.
- Touch targets 44px on phones; desktop rows 28–32px like macOS lists.

## Layout

**Desktop (macOS):**
```
┌─────────────┬────────────────────────────────────────────┐
│ ● ● ●       │  ‹ ›  Title                   🔍  ⊕  ⋯     │  unified glass toolbar
│ (glass      ├────────────────────────────────────────────┤
│  sidebar)   │                                            │
│  Recents    │   content                                  │
│  Shared     │                                            │
│  People     │                                            │
└─────────────┴────────────────────────────────────────────┘
```
A translucent source-list sidebar (collapsible), a unified toolbar with the title and actions,
content on `--canvas` or `--surface`. Optional right inspector.

**Phone (iOS):**
```
┌────────────────────────┐
│ Files            ⊕  ⋯ │  large title; collapses into a glass nav bar on scroll
│ 🔍 Search              │
│ ┌────────────────────┐ │
│ │ Notes            › │ │  inset grouped list
│ │ Photos           › │ │
│ └────────────────────┘ │
│                        │
├────────────────────────┤
│  ▣     ◷     👥    ⚙  │  glass tab bar
└────────────────────────┘
```
Bottom sheets with a grabber for share, details and pickers; swipe actions on list rows;
context menus on long-press (desktop: right-click).

**Sol app home (umbrelOS spirit):** the dusk wallpaper (a CSS gradient: deep blue sky into a
warm gold horizon, a low sun; light mode is morning, dark mode is night), a large greeting and
time, a row of glass widgets (people online, devices, storage), the app grid of squircle icons,
and on desktop a glass dock. Apps open as full windows (desktop) or full screens (phone) with a
spring transition from their icon.

## Components (in `ui/kit`)

App shell (sidebar + toolbar / large title + tab bar, switching at 760px), inset grouped list
with chevrons, swipe actions and selection, search field, segmented control, toggle switch (iOS),
buttons (filled gold, tinted, plain, destructive), context menu, popover, bottom sheet / dialog,
banner toast (slides from the top), avatar with presence ring, app icon (squircle, gradient,
glyph), widget card, progress ring, empty state, skeleton (only for remote data).

Icons are in the spirit of SF Symbols: 1.75px rounded strokes on a 24px grid, drawn by us.

## Motion

Only in answer to the person: sheets and windows use a spring-like curve
`cubic-bezier(.32,.72,0,1)` at 350–450ms; presses scale to 0.97; lists insert and remove with a
short height and fade. The home screen has one orchestrated moment: the wallpaper's sun rises a
few pixels and the icons settle in on first load. All of it switches off under
`prefers-reduced-motion`.

## Writing

Plain, short, sentence case, active verbs. A button says what happens ("Share", "Move to Trash");
the toast after it uses the same word ("Shared"). Empty states invite an action ("Drop files
here or tap + to add some"). Errors say what happened and what to do, without apologising.
