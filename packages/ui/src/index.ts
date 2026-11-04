// SPDX-License-Identifier: AGPL-3.0-only

// The Dockwell design system as typed React components. Import
// "@dockwell/ui/styles.css" once at the root of an app; each component
// brings its own stylesheet. Guidelines for every component live in
// design/system/components/<Name>/README.md.

export { Button, type ButtonProps } from "./components/Button/Button";
export { Callout, type CalloutProps } from "./components/Callout/Callout";
export { CommandBar, type CommandBarProps, type CommandItem } from "./components/CommandBar/CommandBar";
export { FleetGrid, type FleetGridProps, type FleetGroup, type FleetServer } from "./components/FleetGrid/FleetGrid";
export { Glyph, type GlyphProps } from "./components/Glyph/Glyph";
export { Icon, type IconProps } from "./components/Icon/Icon";
export { type IconName, iconNames } from "./components/Icon/icons";
export { Kbd, type KbdProps } from "./components/Kbd/Kbd";
export { Logo, type LogoProps } from "./components/Logo/Logo";
export { type LogLevel, type LogLine, LogStream, type LogStreamProps } from "./components/LogStream/LogStream";
export { Metric, type MetricProps } from "./components/Metric/Metric";
export { Panel, type PanelProps } from "./components/Panel/Panel";
export {
  type PlanAction,
  type PlanChange,
  PlanDiff,
  type PlanDiffProps,
  type PlanKind,
  type PlanResource,
  type PlanValue,
} from "./components/PlanDiff/PlanDiff";
export { ScopeBar, type ScopeBarProps, type ScopeLevel } from "./components/ScopeBar/ScopeBar";
export {
  SegmentedControl,
  type SegmentedControlProps,
  type SegmentedOption,
} from "./components/SegmentedControl/SegmentedControl";
export { Sheet, type SheetProps } from "./components/Sheet/Sheet";
export { Sparkline, type SparklineProps, type SparklineTone } from "./components/Sparkline/Sparkline";
export { StatusBadge, type StatusBadgeProps } from "./components/StatusBadge/StatusBadge";
export { Switch, type SwitchProps } from "./components/Switch/Switch";
export { type CellKey, type RowKey, Table, type TableColumn, type TableProps } from "./components/Table/Table";
export { type TabItem, Tabs, type TabsProps } from "./components/Tabs/Tabs";
export { Tag, type TagProps } from "./components/Tag/Tag";
export { TextField, type TextFieldProps } from "./components/TextField/TextField";
export { Timeline, type TimelineEvent, type TimelineKind, type TimelineProps } from "./components/Timeline/Timeline";
export { Toast, type ToastAction, type ToastProps } from "./components/Toast/Toast";
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
