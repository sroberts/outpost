---
name: Outpost
description: The gateway portal, drawn as a service line at night; one lamp, one line, a station per app.
colors:
  night: "#141a21"
  enamel-hi: "#222c36"
  rule: "#26313b"
  porcelain: "#f3f1e9"
  porcelain-press: "#d9d6cc"
  dim: "#a3abb1"
  field-edge: "#6f7c87"
  beam: "#79a59d"
  lamp: "#f1c191"
  lamp-core: "#fff3df"
  down: "#d9583b"
  down-plate: "#b8402a"
  down-ink: "#f29a85"
typography:
  headline:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "28px"
    fontWeight: 750
    lineHeight: 1.1
    letterSpacing: "-0.015em"
  headline-wide:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "32px"
    fontWeight: 750
    lineHeight: 1.1
    letterSpacing: "-0.015em"
  station-name:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "21px"
    fontWeight: 650
    lineHeight: 1.15
  title:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "19px"
    fontWeight: 680
    lineHeight: 1.1
    letterSpacing: "-0.015em"
  body:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "16px"
    fontWeight: 400
    lineHeight: 1.4
  field-label:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "15px"
    fontWeight: 550
  meta:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "14px"
    fontWeight: 400
  hint:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "13px"
    fontWeight: 400
  status-label:
    fontFamily: "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"
    fontSize: "11px"
    fontWeight: 650
    letterSpacing: "0.06em"
  mono-host:
    fontFamily: "ui-monospace, SF Mono, SFMono-Regular, Menlo, Consolas, monospace"
    fontSize: "13px"
    fontFeature: "tnum"
  mono-small:
    fontFamily: "ui-monospace, SF Mono, SFMono-Regular, Menlo, Consolas, monospace"
    fontSize: "12px"
    fontFeature: "tnum"
rounded:
  label: "3px"
  chip: "4px"
  control: "6px"
  pill: "999px"
  disc: "50%"
spacing:
  line-width: "6px"
  station: "26px"
  hub-column: "44px"
  column-gap: "14px"
  gutter: "20px"
  gutter-wide: "28px"
  measure: "40rem"
  door-measure: "26rem"
  station-row: "72px"
  tap-min: "44px"
  control-height: "46px"
components:
  button-primary:
    backgroundColor: "{colors.porcelain}"
    textColor: "{colors.night}"
    rounded: "{rounded.control}"
    padding: "0 1.1rem"
    height: "46px"
  button-primary-hover:
    backgroundColor: "{colors.porcelain-press}"
    textColor: "{colors.night}"
  button-destructive:
    backgroundColor: "transparent"
    textColor: "{colors.down-ink}"
    rounded: "{rounded.control}"
    padding: "0 1.1rem"
    height: "46px"
  pill:
    backgroundColor: "transparent"
    textColor: "{colors.porcelain}"
    rounded: "{rounded.pill}"
    padding: "0 0.95rem"
    height: "44px"
  pill-quiet:
    backgroundColor: "transparent"
    textColor: "{colors.dim}"
    rounded: "{rounded.pill}"
    padding: "0 0.95rem"
    height: "44px"
  input:
    backgroundColor: "{colors.night}"
    textColor: "{colors.porcelain}"
    rounded: "{rounded.control}"
    padding: "0.6rem 0.75rem"
    height: "46px"
  station-mark-up:
    backgroundColor: "{colors.porcelain}"
    rounded: "{rounded.disc}"
    size: "26px"
  station-mark-starting:
    backgroundColor: "{colors.night}"
    rounded: "{rounded.disc}"
    size: "26px"
  station-mark-down:
    backgroundColor: "{colors.night}"
    rounded: "{rounded.disc}"
    size: "26px"
  state-label-down:
    backgroundColor: "{colors.down-plate}"
    textColor: "{colors.porcelain}"
    typography: "{typography.status-label}"
    rounded: "{rounded.label}"
    padding: "2px 6px"
  state-label-starting:
    backgroundColor: "transparent"
    textColor: "{colors.beam}"
    typography: "{typography.status-label}"
    rounded: "{rounded.label}"
    padding: "2px 6px"
  tag:
    backgroundColor: "transparent"
    textColor: "{colors.dim}"
    typography: "{typography.status-label}"
    rounded: "{rounded.label}"
    padding: "2px 6px"
  command:
    backgroundColor: "{colors.enamel-hi}"
    textColor: "{colors.porcelain}"
    typography: "{typography.mono-small}"
    rounded: "{rounded.chip}"
    padding: "3px 7px"
  lamp:
    backgroundColor: "{colors.lamp}"
    rounded: "{rounded.disc}"
    size: "44px"
---

# Design System: Outpost

## Overview

**Creative North Star: "The Service Line"**

The Outpost header made into the working page. Service health is drawn as one transit line that leaves a lit lamp and runs down the left edge through a station per app. A running app is a solid porcelain station on an unbroken line; an app on its way back is a half-lit station; an app that is down is a hollow red station with the line broken through its stretch. The operator reads the state of the whole place from the shape of the line before reading a word.

The world is night only. The dark ground is not a theme of something lighter; there is no light rendition and none is planned. Everything sits on one flat enamel-night plane, divided by thin rules rather than boxed into cards, with the lamp as the single source of glow. The density is a phone held in one hand: generous 72px station rows, 44px-plus tap targets, a single narrow column capped at 40rem.

It is embedded in the binary and served under a strict CSP (`default-src 'self'`), so the system is built from what one stylesheet, one small script and one raster can carry: system fonts, CSS shapes, no fetched assets, nothing inline. It refuses the homelab icon-tile grid and the bare link list.

**Key Characteristics:**
- One continuous line (6px, beam teal) from the lamp to the last running station; status is drawn, not badged alone.
- Night ground only; porcelain ink; a single amber lamp.
- Rules, not cards; flat, with the lamp glow as the only shadow.
- System sans for everything people read; monospace only for machine strings.
- Phone first: 72px station rows, 44px minimum controls, one column.
- Still: the line never animates.

## Colors

A cold enamel night lit by exactly two warm things: the amber lamp and, when something is wrong, a red station.

### Primary
- **Beam Teal** (beam): the line itself, every station ring that is up, the hub tally, app addresses, link underlines, the notice text and hover borders on pills and inputs. It is the colour of "working".

### Secondary
- **Lamp Amber** (lamp, with lamp-core at the disc's hot centre): the hub lamp, and the operator's own instruments: the focus ring, the text caret, the text selection and the skip link. Nothing else.

### Tertiary
- **Signal Red** (down): the ring of a down station. Nothing else.
- **Signal Red Plate** (down-plate): the fill behind the porcelain DOWN label, darkened from Signal Red so porcelain text on it stays legible (about 4.9:1).
- **Signal Red Ink** (down-ink): readable red text on night: a down station's reason, form error messages, the border of an invalid field, and the destructive ghost button.

### Neutral
- **Enamel Night** (night): the ground of every page, the input fill, and the hollow centre of starting and down stations. Also the browser `theme-color`.
- **Raised Enamel** (enamel-hi): the command chip's fill, the only lifted surface.
- **Rule** (rule): hairline dividers between stations and panels, the rail's bottom edge, pill and tag outlines, the scrollbar.
- **Porcelain** (porcelain): primary ink, station fills, the primary button.
- **Porcelain Pressed** (porcelain-press): primary button hover only.
- **Dim Porcelain** (dim): secondary text: the host in the rail, descriptions, ledes, hints, the quiet pill, the hidden tag.
- **Field Edge** (field-edge): input borders at rest (about 4.1:1 on night).

### Named Rules
**The One Lamp Rule.** Amber belongs to the lamp and to the operator's own instruments (focus ring, caret, selection, skip link). It never marks a status, a link, a heading or a button.

**The Red Means Down Rule.** Signal Red is spent only on a down station and on destructive actions; its ink variant also carries form errors and invalid fields. A screen with nothing down and nothing destructive shows no red.

**The Night Only Rule.** There is one rendition, dark, declared with `color-scheme: dark`. Do not add a light theme or a `prefers-color-scheme` switch.

## Typography

**Display Font:** none; headings use the system sans.
**Body Font:** system-ui (with -apple-system, Segoe UI, Roboto, sans-serif)
**Label/Mono Font:** ui-monospace (with SF Mono, SFMono-Regular, Menlo, Consolas, monospace), tabular figures

**Character:** The platform's own humanist sans at a few fixed label sizes, heavy where it names something (650 to 750) and plain everywhere else; monospace appears only where the text is something a machine reads back.

### Hierarchy
- **Headline** (750, 28px, rising to 32px at 42rem, line-height 1.1, -0.015em): the portal name in the hub and each page's h1. Balanced wrapping.
- **Station name** (650, 21px, 1.15): each app's name on the line; wraps anywhere rather than overflowing.
- **Title** (680, 19px, 1.1): panel headings (Sign in, Change password).
- **Body** (400, 16px, 1.4): running prose, ledes, messages.
- **Field label** (550, 15px): form labels, stacked above their inputs.
- **Meta** (400, 14px): station descriptions, down reasons, pill text, the filter label.
- **Hint** (400, 13px): field hints and footnotes.
- **Status label** (650, 11px, 0.06em, uppercase): only the station state (DOWN, RESTARTING) and the HIDDEN tag.
- **Mono** (13px for the rail host; 12px for addresses, the tally, and commands): tabular figures.

### Named Rules
**The Machine Strings Rule.** Monospace is for the host, app addresses, the hub tally, the footer's build, and `outpost` commands or usernames in `code`. Names, descriptions and prose are never mono.

**The Status Label Rule.** Uppercase tracked text is reserved for the state label and the hidden tag sitting beside a station name. It is never used as a heading or a label above a heading.

## Layout

A single column, phone first. Pages are capped at a 40rem measure plus gutters; the "door" pages (sign in, setup, messages) narrow to 26rem. The gutter is 20px, widening to 28px at the one breakpoint (42rem), where the h1 also steps to 32px and the page's top padding grows from 1.75rem to 2.25rem.

The hub and every station share a two-column grid: a 44px left column that holds the lamp and carries the line, and the content column, 14px away. That shared 44px column is what makes the line run straight from the lamp's centre down through every station's centre.

Rhythm: station rows are at least 72px with 13px vertical padding; panels open 1.5 to 1.75rem below what precedes them with 1.5rem of padding under their rule; form fields stack on a 1rem gap with 0.35rem between label and field. The station's meta row puts the description left and the address right-aligned, wrapping as needed.

The rail across the top holds the host (left, truncating with an ellipsis) and the account pill and sign-out pill (right, never shrinking), under a single rule.

## Elevation & Depth

Flat. Every surface sits on the same night plane; separation comes from 1px rules and from the line. The only depth in the system is the lamp's glow, and the only lifted fill is the command chip's slightly raised enamel.

### Shadow Vocabulary
- **Lamp glow** (`box-shadow: 0 0 0 6px rgba(241, 193, 145, 0.16), 0 0 30px 2px rgba(241, 193, 145, 0.4)`): the hub lamp only, over a radial fill from lamp-core to lamp.

### Named Rules
**The One Glow Rule.** Nothing casts a shadow except the lamp. Cards, buttons, inputs and stations are flat.

## Shapes

Round where the world is round, square-ish where the user works. The lamp and every station are perfect discs; the line is a 6px bar whose final stretch ends in a 3px rounded cap at the last running station. Rail controls are full pills (999px). Buttons, inputs and the skip link have gently rounded 6px corners; the command chip and the focus ring 4px; status labels and tags 3px. Borders are 1px hairlines everywhere except the station ring, which is the line's own 6px width so a station reads as a knot in the line.

The sign-in plate is the lighthouse from the Outpost header, full-bleed past the gutters and masked into the night on all four edges with linear gradients (12% / 88% horizontally, 20% / 65% vertically), so it has no visible frame.

## Components

### Buttons
Plain and certain; one filled button per form.
- **Shape:** gently rounded (6px), at least 46px tall.
- **Primary:** porcelain fill, night text, 680 weight, 0 1.1rem padding. Hover darkens to Porcelain Pressed. No transition.
- **Focus:** the global amber ring (2px, offset 3px).
- **Destructive ghost:** transparent with Signal Red Ink text and a 1px Signal Red Ink border at 60% alpha; hover lays a 12% Signal Red wash. Used for "Sign out everywhere".
- **Link as button:** a link that continues a flow (the onward link on message pages) takes the primary button style.

### Pills (rail controls)
- **Style:** full pill (999px), transparent, 1px rule outline, 14px porcelain text, 44px tall.
- **Quiet:** dim text, porcelain on hover (sign out).
- **Hover:** outline turns beam teal; the fill stays empty.

### Inputs / Fields
- **Style:** night fill, porcelain text, 1px field-edge border, 6px radius, 46px tall, 0.6rem 0.75rem padding. Labels stack above in field-label type.
- **Hover:** border turns beam teal.
- **Focus:** border turns lamp amber, with the amber ring tightened to a 1px offset.
- **Error:** border in Signal Red Ink when `aria-invalid`, with the error message above the form in Signal Red Ink, 550 weight, announced as an alert.

### Navigation
The rail: a full-width bar with a 1px rule beneath. Left, the host in 13px dim monospace (porcelain on hover), padded to a tall tap area and truncated with an ellipsis. Right, the account pill and the quiet sign-out pill. A skip link ("Skip to apps") sits above the rail off-screen and drops in as an amber tab on focus.

### The Hub (signature)
The lamp (a 44px glowing amber disc) beside the portal name in headline type, with the tally beneath in 12px beam monospace ("3 running · 1 down · 1 stopped"). The lamp sits in the same 44px column as the line, which starts just below it. On door pages without the line the hub may drop the lamp (sign-in, which carries the plate instead).

### The Line and its Stations (signature)
An ordered list; each station draws the stretch of line above it, so the line ends exactly at the last running station.
- **Up:** a 26px porcelain disc in a 6px beam ring on a solid line. The whole row is one link to the app. Name, then description with the address right-aligned beneath. Hover underlines the name in beam (2px, 4px offset) and scales the station to 1.15.
- **Starting / restarting:** the disc's centre goes night with a beam core inset 3px, "half lit"; the RESTARTING label is outlined in beam; the reason is dim; the inspect command follows in a command chip; the address remains as an "open anyway" link.
- **Down:** the ring turns Signal Red around a hollow night centre; the line through the row becomes a broken stretch (beam dashes, 7px on, 7px off) that runs on past the station to the end of its row; the DOWN label is porcelain on Signal Red Plate; the reason is in Signal Red Ink; then the command chip and the address.
- **Stopped on purpose:** a hollow night centre in a Field Edge ring on the same broken line; the STOPPED label is dim on a rule outline and the reason is dim. Someone chose this, so it spends no red. Start sits in the row, not behind Manage.
- **Order:** running first, then starting, then down, then stopped, so the broken stretches gather at the end of the line.
- **Hidden:** admins see a HIDDEN tag (dim text, rule outline) beside the name.
- **Solo:** a message page can show a single station on a short line to explain why an app is unavailable.

### Command chip
`outpost status <app>` in 12px porcelain monospace on Raised Enamel, 1px rule border, 4px radius, selectable as a whole with one tap.

### Filter
Once the line is long, a "Filter apps" search field appears above it; it is present in the markup hidden and revealed by the one external script, so the page works without it.

## Do's and Don'ts

### Do:
- **Do** draw service state on the line: solid for up, half-lit for starting, a hollow red station and a broken stretch for down.
- **Do** keep every interactive control at least 44px tall (pills 44px, buttons and inputs 46px, station rows 72px); inline links in rows keep at least a 24px target.
- **Do** separate content with 1px rules in the rule colour, never with boxes.
- **Do** put the address, host, tally and commands in monospace with tabular figures, and nothing else.
- **Do** give every destructive action the red ghost button and every primary action the porcelain button.
- **Do** keep motion to the station's hover scale (0.2s, cubic-bezier(0.16, 1, 0.3, 1)) and turn it off under `prefers-reduced-motion: reduce`.
- **Do** ship styles and scripts as files from the binary, versioned by a hash of their content, so the CSP stays `default-src 'self'`.

### Don't:
- **Don't** use amber for anything but the lamp, the focus ring, the caret, the selection and the skip link.
- **Don't** use red for anything but a down station, destructive actions, and form errors.
- **Don't** animate the line: no pulsing stations, flowing dashes or loading shimmer.
- **Don't** put apps in cards or icon tiles; an app is a station on the line.
- **Don't** add shadows other than the lamp's glow.
- **Don't** write inline `style` attributes or inline `<script>`; the CSP forbids them.
- **Don't** fetch fonts, icons or images from anywhere; everything is embedded.
- **Do** draw icons from the one sprite in the page: 16-unit grid, 1.5 stroke, round caps, in the colour of the text beside them, always with the word for a screen reader. Never a Unicode glyph or emoji.
- **Don't** add a light theme.
