# Images to supply

These are **placeholders**. Replace each file in `public\images/` with real art for
actionscommunity/multirunner. Keep the same filename (or update the reference in the site) and aim for
the listed dimensions. Delete this file once every image is real.

| File | Purpose | Recommended size |
| --- | --- | --- |
| `logo.svg` | Site logo / wordmark (top bar, hero). | 512×512 |
| `og.svg` | Open Graph / Twitter share image. Replace with a raster og.png (scrapers ignore SVG) and update the meta tag. | 1200×630 |
| `favicon.svg` | Browser tab icon. | 64×64 |
| `hero-terminal.svg` | Screenshot/GIF of the CLI in action (hero). | 1280×720 |

Tips:
- Export at 2× for crisp display on high-DPI screens, then keep the file reasonably small.
- PNG for screenshots/photos, SVG for logos/diagrams. The social card (`og.*`) must be a raster (PNG/JPG) — most scrapers ignore SVG.
- A placeholder left in place still deploys fine; it just looks like a placeholder.
