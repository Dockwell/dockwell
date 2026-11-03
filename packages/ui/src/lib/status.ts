// SPDX-License-Identifier: AGPL-3.0-only

/** The five states anything Dockwell runs can be in. */
export type Status = "healthy" | "degraded" | "down" | "deploying" | "stopped";

/** Tones for things that aren't a runtime state, such as callouts and toasts. */
export type Tone = "healthy" | "warning" | "critical" | "info";

/** The status shapes. Shape carries meaning with colour, so state never depends on colour alone. */
export type GlyphShape = "circle" | "triangle" | "square" | "spinner" | "ring" | "info";

export type ToneClass = "healthy" | "warning" | "critical" | "cobalt" | "idle";

interface StatusSpec {
  tone: ToneClass;
  shape: GlyphShape;
}

/**
 * Every state is a shape, a word and a colour. These pairings of shape and
 * colour are fixed: never recolour a shape or reuse one for another meaning.
 * The words live in UIStrings (lib/strings.tsx), so they can be translated.
 */
export const STATUS: Readonly<Record<Status, StatusSpec>> = {
  healthy: { tone: "healthy", shape: "circle" },
  degraded: { tone: "warning", shape: "triangle" },
  down: { tone: "critical", shape: "square" },
  deploying: { tone: "cobalt", shape: "spinner" },
  stopped: { tone: "idle", shape: "ring" },
};

/** The glyph and colour each tone uses in callouts and toasts. */
export const TONE: Readonly<Record<Tone, { tone: ToneClass; shape: GlyphShape }>> = {
  healthy: { tone: "healthy", shape: "circle" },
  warning: { tone: "warning", shape: "triangle" },
  critical: { tone: "critical", shape: "square" },
  info: { tone: "cobalt", shape: "info" },
};
