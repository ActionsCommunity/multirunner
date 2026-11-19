---
name: multirunner
description: Industrial editorial design for one host coordinating many disposable runners.
colors:
  primary-cobalt: "#174FC4"
  signal-orange: "#C6400C"
  cool-paper: "#F3F7FA"
  deep-ink: "#101C2A"
  structural-steel: "#596978"
typography:
  display:
    fontFamily: "Archivo Variable, Arial Narrow, sans-serif"
    fontSize: "clamp(3.2rem, 7.4vw, 6rem)"
    fontWeight: 760
    lineHeight: 1
    letterSpacing: "-0.035em"
  headline:
    fontFamily: "Archivo Variable, Arial Narrow, sans-serif"
    fontSize: "clamp(2.15rem, 4.5vw, 3.6rem)"
    fontWeight: 720
    lineHeight: 1
    letterSpacing: "-0.035em"
  title:
    fontFamily: "Archivo Variable, Arial Narrow, sans-serif"
    fontSize: "1.28rem"
    fontWeight: 700
    lineHeight: 1
    letterSpacing: "-0.02em"
  lead:
    fontFamily: "IBM Plex Sans Variable, Segoe UI, sans-serif"
    fontSize: "clamp(1.08rem, 2vw, 1.28rem)"
    fontWeight: 400
    lineHeight: 1.65
    letterSpacing: "normal"
  body:
    fontFamily: "IBM Plex Sans Variable, Segoe UI, sans-serif"
    fontSize: "1rem"
    fontWeight: 400
    lineHeight: 1.65
    letterSpacing: "normal"
  mono:
    fontFamily: "IBM Plex Mono, Consolas, monospace"
    fontSize: "0.88rem"
    fontWeight: 500
    lineHeight: 1.6
    letterSpacing: "normal"
spacing:
  xs: "4px"
  sm: "8px"
  md: "16px"
  lg: "24px"
  xl: "32px"
  xxl: "48px"
  section-sm: "64px"
  section-lg: "96px"
components:
  button-primary:
    backgroundColor: "{colors.primary-cobalt}"
    textColor: "{colors.cool-paper}"
    typography: "{typography.body}"
    padding: "11px 18px"
    height: "46px"
  button-secondary:
    backgroundColor: "{colors.cool-paper}"
    textColor: "{colors.deep-ink}"
    typography: "{typography.body}"
    padding: "11px 18px"
    height: "46px"
  code-panel:
    backgroundColor: "{colors.deep-ink}"
    textColor: "{colors.cool-paper}"
    typography: "{typography.mono}"
    padding: "24px"
---

# Design System: multirunner

## Overview

**Creative North Star: "The Runner Yard"**

multirunner looks like an industrial technical artifact built by people who
operate machines. Cool paper, hard ink, cobalt runner lanes, and one scarce
orange state marker make parallel work visible without turning the site into a
terminal costume or a generic developer dashboard.

The generated host illustration carries the metaphor. Interface chrome stays
flat, square, and legible so the product remains more important than the visual
effect.

**Key Characteristics:**

- Cool white field with hard dark rules
- Cobalt carries operation and navigation
- Orange marks one meaningful state at a time
- Generated imagery explains the one-host, many-jobs model
- Square controls and structural offset shadows

## Colors

The palette behaves like spot-color printing on cool technical paper.

### Primary

- **Operational Cobalt:** Primary actions, links, active lanes, and large section
  fields. It carries most of the system's color.

### Secondary

- **Signal Orange:** Selected job modules, focus rings, and scarce structural
  accents. It is never used as small decorative text.

### Neutral

- **Cool Paper:** Page background and light control surface.
- **Deep Ink:** Body text, outlines, navigation, code panels, and dark sections.
- **Structural Steel:** Secondary text, inactive lanes, dividers, and hardware.

### Named Rules

**The Four-to-One Rule.** Cobalt occupies roughly four times the visual area of
orange. Orange marks a state; it does not decorate a section.

**The Blank Module Rule.** Job blocks in imagery remain geometric and unmarked.
No operating-system, GitHub, checkmark, or fake terminal glyph belongs on them.

## Typography

**Display Font:** Archivo Variable

**Body Font:** IBM Plex Sans Variable

**Label/Mono Font:** IBM Plex Mono

Archivo makes headlines compact and mechanical without using a novelty stencil.
IBM Plex keeps explanatory and technical content calm at long reading lengths.

### Hierarchy

- **Display:** Heavy, tightly spaced, and capped at 6rem for first-view headings.
- **Headline:** Archivo at 2.15rem to 3.6rem for section decisions.
- **Title:** Archivo around 1.28rem for feature and backend names.
- **Body:** IBM Plex Sans at 1rem and 1.65 line height, limited to roughly 66
  characters where prose is sustained.
- **Label:** IBM Plex Mono only for commands, flags, measurements, and platform
  metadata.

### Named Rules

**The Tool, Not Terminal Rule.** Monospace identifies real code or machine
metadata. It never replaces body or display typography.

## Layout

The site uses a 1200px maximum content width and an eight-step spacing scale.
Sections are separated by hard two-pixel rules. Desktop compositions pair one
decision-sized heading with evidence or runnable code. Mobile layouts stack in
reading order and preserve the hero image immediately after the primary action.

The hero reserves its left field for copy and gives the generated host chassis
the right field. At narrow widths, only the essential CLI navigation remains.

## Elevation & Depth

The system is flat by default. Buttons, code panels, and the hero terminal use
small structural offset shadows to feel printed and assembled, not floating.
There are no ambient cards, glass layers, or soft panel stacks.

### Shadow Vocabulary

- **Control offset:** `4px 4px 0 #101C2A` for actionable buttons.
- **Panel offset:** `6px 6px 0 #596978` for code and command surfaces.

### Named Rules

**The Structural Shadow Rule.** A shadow must explain a tactile layer or state.
It never appears as a glow.

## Shapes

Corners are square. Two-pixel ink borders define major surfaces; one-pixel steel
rules separate rows. The logo and hero repeat the same lane-and-host geometry.
Small chamfers may exist inside generated hardware art, but not in interface
containers.

## Components

### Buttons

- **Shape:** Square with a two-pixel ink border.
- **Primary:** Cobalt field, cool-paper text, 11px by 18px padding, and a hard
  ink offset.
- **Hover / Focus:** Hover changes the field to orange and shortens the offset.
  Focus uses a three-pixel orange outline with four-pixel clearance.
- **Secondary:** Cool-paper field with ink text; hover inverts to ink and paper.

### Chips

- **Style:** Cool-paper field, one-pixel steel rule, and IBM Plex Mono.
- **State:** Informational only. Chips do not pretend to be buttons.

### Cards / Containers

Feature content uses divided rows, not a repeated rounded-card grid. Major code
panels use deep ink, cool-paper text, square corners, and a structural offset.

### Navigation

Navigation uses IBM Plex Sans with a two-pixel active underline. At narrow
effective widths, optional links progressively disappear while the brand and
CLI reference remain.

### Runner Host Illustration

One cool-field isometric chassis contains parallel blank modules. Cobalt
indicates active system flow; a single orange module marks current state.

## Do's and Don'ts

### Do:

- **Do** use the five named colors for every durable visual role.
- **Do** reserve orange for focus, selection, or one focal job state.
- **Do** keep technical imagery blank, geometric, and mechanically plausible.
- **Do** use hard rules and divided lists instead of generic cards.
- **Do** keep every generated image useful at desktop and mobile crops.

### Don't:

- **Don't** introduce beige, cream, warm paper, or nostalgic aging effects.
- **Don't** add gradients, neon, glass, pill-heavy controls, or soft SaaS cards.
- **Don't** put logos, letters, numbers, checkmarks, or fake terminal text inside
  generated job modules.
- **Don't** use monospace as a general visual theme.
- **Don't** alternate cobalt and orange evenly; orange must remain scarce.
