// SPDX-License-Identifier: AGPL-3.0-only

// The Dockwell design system as typed React components. Import
// "@dockwell/ui/styles.css" once at the root of an app; each component
// brings its own stylesheet. Guidelines for every component live in
// design/system/components/<Name>/README.md.

export { Button, type ButtonProps } from "./components/Button/Button";
export { Glyph, type GlyphProps } from "./components/Glyph/Glyph";
export { Icon, type IconProps } from "./components/Icon/Icon";
export { type IconName, iconNames } from "./components/Icon/icons";
export { Kbd, type KbdProps } from "./components/Kbd/Kbd";
export { Logo, type LogoProps } from "./components/Logo/Logo";
export { Panel, type PanelProps } from "./components/Panel/Panel";
export { StatusBadge, type StatusBadgeProps } from "./components/StatusBadge/StatusBadge";
export { Tag, type TagProps } from "./components/Tag/Tag";
export { cx } from "./lib/cx";
export { type GlyphShape, STATUS, type Status, TONE, type Tone } from "./lib/status";
export { englishStrings, type UIStrings, UIStringsProvider, useUIStrings } from "./lib/strings";
export {
  applyThemePreference,
  type ResolvedTheme,
  readThemePreference,
  setThemePreference,
  THEME_STORAGE_KEY,
  type ThemePreference,
  useResolvedTheme,
  useThemePreference,
} from "./theme/theme";
