// SPDX-License-Identifier: AGPL-3.0-only

/**
 * The states anything Dockwell runs can be in. Healthy means a check passed: what
 * runs with nothing checking it is running, and what nobody can say anything of is
 * unknown, so neither reads as healthy.
 */
export type Status = "healthy" | "running" | "degraded" | "down" | "deploying" | "stopped" | "unknown";

/** Tones for things that aren't a runtime state, such as callouts and toasts. */
export type Tone = "healthy" | "warning" | "critical" | "info";

/** The status shapes. Shape carries meaning with colour, so state never depends on colour alone. */
export type GlyphShape = "circle" | "triangle" | "square" | "spinner" | "ring" | "dotted-ring" | "dashed-ring" | "info";

export type ToneClass = "healthy" | "warning" | "critical" | "cobalt" | "idle" | "running" | "unknown" | "ink";

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
  running: { tone: "running", shape: "dotted-ring" },
  unknown: { tone: "unknown", shape: "dashed-ring" },
};

/** The glyph and colour each tone uses in callouts and toasts. */
export const TONE: Readonly<Record<Tone, { tone: ToneClass; shape: GlyphShape }>> = {
  healthy: { tone: "healthy", shape: "circle" },
  warning: { tone: "warning", shape: "triangle" },
  critical: { tone: "critical", shape: "square" },
  info: { tone: "cobalt", shape: "info" },
};
