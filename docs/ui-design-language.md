# Dashboard UI design language ("command center")

Introduced with the Overview redesign (`templates/overview.html`, Sept 2026). Every
dashboard page follows the same structure so the app reads as one product. The
shared CSS lives at the bottom of `static/style.css` under `cc-*`; the Overview
page has its own `ov3-*` copy of the same patterns and is the visual reference.

## Page skeleton

```
<div class="cc-wrap">
  <div class="cc-top">
    <div>
      <div class="cc-eyebrow">SECTION · context · <span class="cc-live"><span class="dot"></span>Live</span></div>
      <h1 class="cc-h1">Headline that states a <span class="hl warn">verdict</span></h1>
      <div class="cc-sub">One sentence with the live numbers in <b>bold</b>.</div>
    </div>
    <div class="cc-actions">
      <a class="cc-btn primary" href="…"><svg …/>Primary action</a>
      <a class="cc-btn" href="…"><svg …/>Secondary<span class="n warn">3</span></a>
    </div>
  </div>

  [optional] <div class="cc-hero is-ok|is-warn|is-danger" style="grid-template-columns:auto 1fr 300px">
               <div class="cc-hero-cell">…ring / big number…</div>
               <div class="cc-hero-cell">…facts chips (.cc-facts > a.cc-fact)…</div>
               <div class="cc-hero-cell">…composition / mini stats (.cc-mini)…</div>
             </div>

  [optional] <div class="cc-kpis"> <a class="cc-kpi has-spark" href="…">…</a> ×N </div>

  <div class="cc-grid">            (or .cc-grid.even / .cc-grid.rail / plain stack)
    <div class="cc-col"> <div class="cc-card">…</div> … </div>
    <div class="cc-col"> … </div>
  </div>
</div>
```

## Rules

1. **Headline is a verdict, not a label.** "3 sites need a look today", "All rollouts
   landed", "2 policies cover 18 of 26 devices". The page name goes in the eyebrow.
   Colour the key phrase with `.hl.ok|warn|danger` matching the state.
2. **Actions live top-right on the same row as the headline** (`.cc-actions`, never
   wrapping under the title on desktop). One `.primary` at most. Buttons are
   `.cc-btn` with a 14px stroke icon.
3. **Numbers first.** If the page has counts, show a `.cc-kpis` strip (auto-fit
   tiles) or a hero with `.cc-big` / `.cc-ring`. Colour numbers by state
   (`.ok .warn .bad`), never decoratively. Zero is neutral, not green.
4. **Every card has an icon chip header**: `<div class="cc-ch"><span class="ic ok"><svg/></span><h3>Title</h3><span class="m">meta</span><a class="r">Link →</a></div>`.
   Card body either `.cc-cb` (padded) or a list of `.cc-row` (flush rows) or a
   `.cc-table`.
5. **Empty states are designed**: `.cc-empty` with an icon and a bold first line
   that says what would appear here and how to make it appear. Use `.cc-empty.tall`
   when it is the whole card.
6. **Chips over prose.** State summaries are `.cc-fact` chips that link to the
   filtered list. Tags on rows are `.cc-tag`.
7. **Progress is a bar.** Rollouts, coverage, adoption: `.cc-meter` (single) or
   `.cc-stack` (segments, `.cc-c1…c6`) with a `.cc-legend`.
8. **Filter chips** are `.cc-seg` (pill group, `.on` for active). Keep existing
   filter semantics and URLs; only restyle.
9. **Theme**: only CSS variables from `:root` (`--surface --border --muted --ok
   --warn --danger --accent --accent-text --accent-2 --info …`). Never hard-code a
   hex outside chart JS, and chart JS reads `getComputedStyle(document.body)`.
10. **Responsive**: grids collapse at 980px; the top bar stacks at 1000px. Tables
    scroll inside their card, never the page.
11. **Keep behaviour.** htmx attributes, ids used by JS, form names, hx-targets,
    polling regions, `data-diff-guard`, `js-local-time`, and every route stay
    exactly as they are. Restyle around them. If a JS block selects by class,
    keep that class in place (add the cc-class alongside).
12. **Page-specific CSS** goes in a `<style>` at the top of the template with a
    short page prefix (`fl-`, `ac-`, `al-`, `rl-`…). Reuse `cc-*` first; add
    page CSS only for what is unique. Do not edit `layout.html`.
13. **Copy**: short, plain, no exclamation marks. Sentence case. "→" only on links.

## Mixed-fleet conventions (Sept 2026)

Two axes, tagged the same way on every surface (Fleet cards, device ribbon,
inbox rows, Actions console rows):

- **Kind** (how the device is managed): `Firmware` (our hardware, system-app
  client, fleet key) or `DPC agent` (stock Android, Device-Owner agent, per-device
  key). Firmware gets a plain `.cc-tag`, DPC gets `.cc-tag.info`. Both kinds are
  tagged — never tag only the "odd" one.
- **Class** (what it is for): Tablet, Panel, Kiosk, mPOS, POS, Other. A plain
  `.cc-tag`. Firmware devices derive it from the product; DPC devices get it from
  the enrollment profile, overridable on the device page.
- **Lifecycle**: `New · needs a site` (`.cc-tag.warn`, links to Fleet › Enroll) while
  `onboarded_at` is NULL; `Retired` / `Wiped` (`.cc-tag.bad`). Active devices show no
  lifecycle tag.

Controls are offered by capability, not by kind: templates ask
`supports .Device "screenshot"` / `degraded …` (see `internal/product/caps.go`).
An unsupported item renders disabled with a reason; a degraded one renders with a
`limited` tag. Never branch on `IsDPC` for a control.

The **onboarding inbox** row pattern (`.en-row`): serial + kind/product/enrolled-ago
on the left, inline site + class selects and a `Done` button on the right. The
Enroll page and the Overview widget share it.

## Icons

Inline 24-viewbox stroke SVGs (fill none, stroke currentColor, width 2, round
caps). Reuse the set in `overview.html`: check, wifi-off, battery, thermometer,
crash (star burst), bell, zap (send action), download (deploy), map pin, trend,
building (sites), tag (release), lock (kiosk), activity (pulse).
