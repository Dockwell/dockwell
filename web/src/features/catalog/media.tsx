// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { monogram } from "./useCatalog";

import "./AppIcon.css";

/** Catalog artwork is served by the control plane, from what it read of the catalog, never loaded from an entry's host. */
export function mediaPath(path: string): string | undefined {
  return /^\/catalog-media\/[a-z0-9]+(?:[-_/][a-z0-9]+)*\.(?:svg|png|jpg|jpeg|webp|avif)$/.test(path)
    ? path
    : undefined;
}

/** External project links are navigations, and cannot contain executable schemes or credentials. */
export function projectLink(address: string): string | undefined {
  try {
    const url = new URL(address);
    return url.protocol === "https:" && !url.username && !url.password && !/[\s]/u.test(address) ? url.href : undefined;
  } catch {
    return undefined;
  }
}

/**
 * What a logo's tile is, from its own pixels: light under a mark that's mostly dark or
 * coloured, as most are drawn for a light ground; dark under one that's mostly light,
 * as a white mark is; none under one that brings its own square, which is drawn edge to
 * edge and clipped by the tile; neutral, with a border, where its pixels can't be read; and
 * empty where it draws nothing at all, as an SVG whose drawing was cleaned out does, which
 * the entry's monogram stands in for.
 */
export type Ground = "light" | "dark" | "bleed" | "neutral" | "empty";

/** The side of the square a logo is read at. */
export const groundSide = 32;

/**
 * groundOf reads a logo drawn into a square of side pixels as RGBA, as the tile would
 * hold it: its opaque pixels' share of the square and of its edges, and how many of them
 * are light or dark.
 */
export function groundOf(rgba: ArrayLike<number>, side: number): Ground {
  const alpha = (i: number) => rgba[i * 4 + 3] ?? 0;
  const opaque = (x: number, y: number) => alpha(y * side + x) >= 160;
  // Its light pixels' share of those at least as opaque as least.
  const share = (least: number) => {
    let seen = 0;
    let light = 0;
    for (let i = 0; i < side * side; i++) {
      if (alpha(i) < least) continue;
      seen++;
      const lum =
        (0.2126 * (rgba[i * 4] ?? 0) + 0.7152 * (rgba[i * 4 + 1] ?? 0) + 0.0722 * (rgba[i * 4 + 2] ?? 0)) / 255;
      if (lum >= 0.75) light++;
    }
    return { seen, light: seen ? light / seen : 0 };
  };
  const solid = share(160);
  if (solid.seen === 0) {
    // A logo drawn only in faint pixels is read by them; one that draws nothing is empty.
    const faint = share(16);
    if (faint.seen === 0) return "empty";
    return faint.light >= 0.55 ? "dark" : "light";
  }
  const seen = solid.seen;
  // Its own square reaches each edge of the tile, near enough, where a rounded corner
  // leaves the corners themselves out.
  const inset = Math.round(side * 0.06);
  const mid = side >> 1;
  const far = side - 1 - inset;
  if (seen >= side * side * 0.75 && opaque(mid, inset) && opaque(mid, far) && opaque(inset, mid) && opaque(far, mid)) {
    return "bleed";
  }
  return solid.light >= 0.55 ? "dark" : "light";
}

/** Where a logo's artwork lies in the square it's drawn in: its width and height as shares of it, and its centre's offset from the square's. */
export interface Artwork {
  w: number;
  h: number;
  x: number;
  y: number;
}

/**
 * artworkOf finds what a logo drawn into a square of side pixels draws, past its
 * transparent margins: the box around its ink, weighed by how opaque each pixel is,
 * less the outermost 2% of it each way, so a stray dot far from the artwork doesn't set
 * its size.
 */
export function artworkOf(rgba: ArrayLike<number>, side: number): Artwork | undefined {
  const cols = new Array<number>(side).fill(0);
  const rows = new Array<number>(side).fill(0);
  let total = 0;
  for (let y = 0; y < side; y++) {
    for (let x = 0; x < side; x++) {
      const a = rgba[(y * side + x) * 4 + 3] ?? 0;
      if (a < 16) continue;
      cols[x] = (cols[x] ?? 0) + a;
      rows[y] = (rows[y] ?? 0) + a;
      total += a;
    }
  }
  if (total === 0) return undefined;
  const span = (mass: number[]) => {
    let lo = 0;
    let hi = side - 1;
    for (let seen = 0; lo < side && seen + (mass[lo] ?? 0) <= total * 0.02; lo++) seen += mass[lo] ?? 0;
    for (let seen = 0; hi > lo && seen + (mass[hi] ?? 0) <= total * 0.02; hi--) seen += mass[hi] ?? 0;
    return [lo, hi] as const;
  };
  const [x0, x1] = span(cols);
  const [y0, y1] = span(rows);
  return {
    w: (x1 - x0 + 1) / side,
    h: (y1 - y0 + 1) / side,
    x: (x0 + x1 + 1) / 2 / side - 0.5,
    y: (y0 + y1 + 1) / 2 / side - 0.5,
  };
}

/** The least share of its tile a logo on a ground is drawn to fill, along its artwork's longer side. */
export const leastFill = 0.7;
/** The share of its tile's width a wide wordmark may be drawn across, past the margin other logos keep. */
export const wordmarkFill = 0.92;
/** The share of its tile a logo on a ground is drawn in (AppIcon.css). */
const drawnIn = 0.84;

/**
 * enlarge is how much a logo on a ground is drawn larger, so small artwork fills
 * leastFill of its tile, and a wide wordmark, held to its tile's width, crosses
 * wordmarkFill of it: never smaller, and for a raster image never past its own pixels
 * at twice the screen's, which would blur it. natural is a raster image's longer side
 * in pixels, and undefined for an SVG, which draws sharp at any size.
 */
export function enlarge(art: Artwork | undefined, tile: number, natural: number | undefined): number {
  if (!art) return 1;
  const long = Math.max(art.w, art.h);
  const short = Math.min(art.w, art.h);
  if (long <= 0) return 1;
  const drawn = drawnIn * long;
  const target = drawn < leastFill ? leastFill : short / long < 0.4 ? wordmarkFill : drawn;
  let scale = target / drawn;
  if (natural !== undefined) scale = Math.min(scale, natural / (tile * drawnIn * 2));
  return Math.max(1, scale);
}

/** What a logo's pixels say of how to draw it: its ground, its artwork, and its longer side if it's a raster image. */
interface Reading {
  ground: Ground;
  art?: Artwork;
  natural?: number;
}

/** Each logo's reading, once a page, by its path. */
const readings = new Map<string, Reading>();

/** readLogo draws a loaded logo as the tile holds it, contained, and reads it. */
function readLogo(img: HTMLImageElement, raster: boolean): Reading {
  try {
    const canvas = document.createElement("canvas");
    canvas.width = groundSide;
    canvas.height = groundSide;
    const g = canvas.getContext("2d", { willReadFrequently: true });
    if (!g) return { ground: "neutral" };
    const w = img.naturalWidth || groundSide;
    const h = img.naturalHeight || groundSide;
    const scale = Math.min(groundSide / w, groundSide / h);
    const dw = w * scale;
    const dh = h * scale;
    g.drawImage(img, (groundSide - dw) / 2, (groundSide - dh) / 2, dw, dh);
    const rgba = g.getImageData(0, 0, groundSide, groundSide).data;
    const art = artworkOf(rgba, groundSide);
    return {
      ground: groundOf(rgba, groundSide),
      ...(art ? { art } : {}),
      ...(raster ? { natural: Math.max(w, h) } : {}),
    };
  } catch {
    return { ground: "neutral" };
  }
}

/** The project's own mark, with a quiet fallback for entries that have no artwork. */
export function AppIcon({ name, path, large = false }: { name: string; path: string; large?: boolean }) {
  const [failed, setFailed] = useState<string>();
  const src = mediaPath(path);
  const [read, setRead] = useState<{ src: string; reading: Reading }>();
  const reading = src === undefined ? undefined : (readings.get(src) ?? (read?.src === src ? read.reading : undefined));
  const ground = reading?.ground;
  const art = src !== undefined && failed !== src && ground !== "empty";
  // Small artwork on a ground is drawn larger about its own centre.
  const tile = large ? 72 : 48;
  const grow = reading && ground !== "bleed" ? enlarge(reading.art, tile, reading.natural) : 1;
  const style =
    grow > 1 && reading?.art
      ? {
          transform: `scale(${grow.toFixed(3)}) translate(${(-reading.art.x * 100).toFixed(2)}%, ${(-reading.art.y * 100).toFixed(2)}%)`,
        }
      : undefined;
  return (
    <span
      className={cx("catalog-icon", large && "is-large", art && "has-art")}
      data-ground={art ? (ground ?? "reading") : undefined}
      aria-hidden="true"
    >
      {art ? (
        <img
          src={src}
          alt=""
          width={tile}
          height={tile}
          style={style}
          onLoad={(e) => {
            if (readings.has(src)) return;
            const found = readLogo(e.currentTarget, !src.endsWith(".svg"));
            readings.set(src, found);
            setRead({ src, reading: found });
          }}
          onError={() => setFailed(src)}
        />
      ) : (
        monogram(name)
      )}
    </span>
  );
}

/** A real screenshot. A missing image says so rather than showing a broken image or an invented preview. */
export function AppScreenshot({ path, caption }: { path: string; caption: string }) {
  const { t } = useLingui();
  const [failed, setFailed] = useState<string>();
  const src = mediaPath(path);
  if (!src || failed === src) {
    return (
      <div className="catalog-preview-missing">
        <Icon name="browser" size={24} />
        <Trans>Preview unavailable</Trans>
      </div>
    );
  }
  return (
    <a href={src} target="_blank" rel="noreferrer" aria-label={t`Open full size: ${caption}`}>
      <img src={src} alt={caption} loading="lazy" decoding="async" onError={() => setFailed(src)} />
      <span className="catalog-preview-open">
        <Icon name="external" size={14} />
        <Trans>Open full size</Trans>
      </span>
    </a>
  );
}
