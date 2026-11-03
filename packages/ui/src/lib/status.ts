// SPDX-License-Identifier: AGPL-3.0-only

/** The five states anything Dockwell runs can be in. */
export type Status = "healthy" | "degraded" | "down" | "deploying" | "stopped";

/** Tones for things that aren't a runtime state, such as callouts and toasts. */
export type Tone = "healthy" | "warning" | "critical" | "info";

/** The status shapes. Shape carries meaning with colour, so state never depends on colour alone. */
export type GlyphShape = "circle" | "triangle" | "square" | "spinner" | "ring" | "info";

export type ToneClass = "healthy" | "warning" | "critical" | "cobalt" | "idle";

interface StatusSpec {
  /** The status word, in sentence case. */
  label: string;
  tone: ToneClass;
  shape: GlyphShape;
}

/**
 * Every state is a shape, a word and a colour. These pairings are fixed:
 * never recolour a shape or reuse one for another meaning.
 */
export const STATUS: Readonly<Record<Status, StatusSpec>> = {
  healthy: { label: "Healthy", tone: "healthy", shape: "circle" },
  degraded: { label: "Degraded", tone: "warning", shape: "triangle" },
  down: { label: "Down", tone: "critical", shape: "square" },
  deploying: { label: "Deploying", tone: "cobalt", shape: "spinner" },
  stopped: { label: "Stopped", tone: "idle", shape: "ring" },
};

/** The glyph and colour each tone uses in callouts and toasts. */
export const TONE: Readonly<Record<Tone, { tone: ToneClass; shape: GlyphShape }>> = {
  healthy: { tone: "healthy", shape: "circle" },
  warning: { tone: "warning", shape: "triangle" },
  critical: { tone: "critical", shape: "square" },
  info: { tone: "cobalt", shape: "info" },
};
