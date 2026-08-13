# K8squad — Logo & Brand Marks

Vector logo system for **K8squad** (capital `K`, numeral `8`, lowercase `squad` — mirrors
`K8s` casing). SVG sources are **self-contained**: the wordmark is outlined to vector paths
(Geist Sans / Geist Mono, OFL) and the marks are pure geometry, so every file renders
identically with no font installed.

## The system — one idea, two zoom levels

| | Concept | Home |
|---|---|---|
| **8-Crest** (lead symbol) | The shared `8` of `K8s`/`K8squad` as two stacked rounded-square squad-containers, pinched at a bright **coordinator** node. Lineage + name fused in one glyph. | Avatar · favicon · app icon |
| **Squad Formation** (expanded) | CRD-square agent nodes in a right-pointing flying wedge — lead in front, squad falling in behind, thin A2A connectors. "A team that moves as one." | Banner mark · illustrations |
| **Helm, Re-crewed** (heritage, optional) | Hub + **5** spokes (a squad — deliberately fewer than k8s' seven), each ending in an agent node. **No rim** — deliberately *not* the Kubernetes ship-wheel. | Heritage lockup only |

The 8-Crest *is* a two-row squad formation — the two marks are the same idea at different zoom.

## Palette (azure-mono — no status hues)

| Role | Hex |
|---|---|
| Squad Azure (hero) | `#3D7DFF` |
| Lead / highlight tint | `#93B7FF` |
| Recede tint | `#16244A` (depth mid `#2E4E8C`) |
| On-dark ground / ink | `#0B1220` |
| Knockout | `#E8EEF9` |

The mark is **single-hue azure family** on purpose — nodes read as *one team, distinct members*.
Green / amber / rose / violet are reserved product-status semantics and never appear in the brand.

## Typography

- **Wordmark:** Geist Sans. `K8` heavier, the **`8` emphasised** in Squad Azure (the numeronym
  hinge), `squad` set lighter and slightly dimmer to pop the hinge.
- **Terminal lockup (alt):** Geist Mono — nods to the `kubectl`/terminal-UI sibling vibe.

## Files

```
svg/                          — self-contained vector source (edit these)
png/                          — raster exports (512px marks, 1180px banners, 1280px readme)
favicon/                      — favicon.svg + 16/32/48/64px PNG (16px verified legible)
```

Variants per mark: `on-dark` (`#0B1220`) · `on-light` (white) · `mono-dark` / `mono-light`
(1-colour) · `reversed` (white knockout on Squad Azure, for avatar rings & stickers).

- **Avatar / GitHub org icon:** `svg/mark-8crest-on-dark.svg`
- **Favicon:** `favicon/favicon.svg` (or the PNG sizes)
- **README / social banner:** `png/readme-banner.png`
- **Horizontal lockup:** `svg/banner-on-dark.svg` · `banner-on-light.svg` · `banner-terminal-mono.svg`

## Taglines (banner options)

1. Kubernetes-native agent squads. *(clearest)*
2. Your agents, in formation. *(shortest, on-metaphor)*
3. Orchestrate agent squads on Kubernetes. *(verb-forward)*

## Regenerating / resizing

Marks and wordmark are defined parametrically. To re-export at new sizes, re-render any
`svg/` file with an SVG rasteriser (e.g. `resvg`, `cairosvg`, or a browser) — no fonts required.

## License

Geist Sans & Geist Mono are used under the SIL Open Font License 1.1.
