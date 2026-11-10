// SPDX-License-Identifier: AGPL-3.0-only

// The Dockwell design system as typed React components. Import
// "@dockwell/ui/styles.css" once at the root of an app; each component
// brings its own stylesheet, except Panel, whose rules come with the
// foundations there, as pages put dw-panel on their own elements too.
// Guidelines for every component live in design/system/components/<Name>/README.md.

export { Advanced, type AdvancedProps } from "./components/Advanced/Advanced";
export { Button, ButtonLink, type ButtonLinkProps, type ButtonProps } from "./components/Button/Button";
export { Callout, type CalloutProps } from "./components/Callout/Callout";
export { CodeField, type CodeFieldProps } from "./components/CodeField/CodeField";
export { CommandBar, type CommandBarProps, type CommandItem } from "./components/CommandBar/CommandBar";
export { CopyButton, type CopyButtonProps } from "./components/CopyButton/CopyButton";
export { DnsRecord, type DnsRecordProps } from "./components/DnsRecord/DnsRecord";
export { FleetGrid, type FleetGridProps, type FleetGroup, type FleetServer } from "./components/FleetGrid/FleetGrid";
export { type FolderListing, FolderPicker, type FolderPickerProps } from "./components/FolderPicker/FolderPicker";
export { Glyph, type GlyphProps } from "./components/Glyph/Glyph";
export { Icon, type IconProps } from "./components/Icon/Icon";
export { type IconName, iconNames } from "./components/Icon/icons";
export { Kbd, type KbdProps } from "./components/Kbd/Kbd";
export { Logo, type LogoProps } from "./components/Logo/Logo";
export { parseAnsi, plainText } from "./components/LogStream/ansi";
export {
  type LogLevel,
  type LogLine,
  type LogLines,
  LogStream,
  type LogStreamHandle,
  type LogStreamProps,
} from "./components/LogStream/LogStream";
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
export { type PortCheck, PortField, type PortFieldProps, validPortNumber } from "./components/PortField/PortField";
export { ScopeBar, type ScopeBarProps, type ScopeLevel } from "./components/ScopeBar/ScopeBar";
export {
  SegmentedControl,
  type SegmentedControlProps,
  type SegmentedOption,
} from "./components/SegmentedControl/SegmentedControl";
export { SelectField, type SelectFieldProps, type SelectOption } from "./components/SelectField/SelectField";
export {
  Sheet,
  type SheetProps,
  SheetSwap,
  SheetView,
  type SheetViewProps,
  useSheetViewAway,
} from "./components/Sheet/Sheet";
export { Sparkline, type SparklineProps, type SparklineTone } from "./components/Sparkline/Sparkline";
export { StatusBadge, type StatusBadgeProps } from "./components/StatusBadge/StatusBadge";
export { Switch, type SwitchProps } from "./components/Switch/Switch";
export { type CellKey, type RowKey, Table, type TableColumn, type TableProps } from "./components/Table/Table";
export {
  type TabItem,
  TabLink,
  type TabLinkProps,
  TabNav,
  type TabNavProps,
  Tabs,
  type TabsProps,
} from "./components/Tabs/Tabs";
export { Tag, type TagProps } from "./components/Tag/Tag";
export { TextField, type TextFieldProps } from "./components/TextField/TextField";
export { Timeline, type TimelineEvent, type TimelineKind, type TimelineProps } from "./components/Timeline/Timeline";
export { Toast, type ToastAction, type ToastProps } from "./components/Toast/Toast";
export { cx } from "./lib/cx";
export { type Off, useOff, useOffReasons } from "./lib/off";
export { type GlyphShape, STATUS, type Status, TONE, type Tone, type ToneClass } from "./lib/status";
export { englishStrings, type UIStrings, UIStringsProvider, useUIStrings } from "./lib/strings";
export { PHONE_QUERY, useMediaQuery } from "./lib/useMediaQuery";
export {
  applyThemePreference,
  followThemeAcrossTabs,
  type ResolvedTheme,
  readThemePreference,
  setThemePreference,
  THEME_STORAGE_KEY,
  type ThemePreference,
  useResolvedTheme,
  useThemePreference,
} from "./theme/theme";
