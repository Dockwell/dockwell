import type * as React from 'react';

export type Status = 'healthy' | 'running' | 'degraded' | 'down' | 'deploying' | 'stopped' | 'unknown';
export type Tone = 'healthy' | 'warning' | 'critical' | 'info';
export type IconName =
  | 'search' | 'check' | 'x' | 'plus' | 'minus' | 'chevron-right' | 'chevron-down' | 'chevron-up-down' | 'arrow-right'
  | 'server' | 'service' | 'globe' | 'database' | 'terminal' | 'branch' | 'commit' | 'undo' | 'clock' | 'shield' | 'lock'
  | 'settings' | 'more' | 'copy' | 'external' | 'play' | 'pause' | 'stop' | 'deploy' | 'refresh' | 'activity' | 'layers'
  | 'folder' | 'org' | 'alert' | 'bell' | 'key' | 'home' | 'archive' | 'cpu' | 'filter' | 'sun' | 'moon'
  | 'browser' | 'link' | 'grid' | 'users' | 'user' | 'card' | 'chat' | 'trash' | 'edit' | 'file' | 'download' | 'eye'
  | 'mail' | 'logout' | 'menu' | 'arrow-left' | 'upload' | 'gauge' | 'network' | 'calendar' | 'question' | 'pin';

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  /** primary = ink fill, one per view. secondary is the default. danger only for destructive actions. */
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger';
  size?: 'sm' | 'md' | 'lg';
  /** Icon before the label; with no children the button becomes square and needs aria-label. */
  icon?: IconName;
  iconAfter?: IconName;
  /** Shortcut hint shown inside the button, e.g. "⌘↵". */
  kbd?: string;
  /** Replaces the icon with a spinner and disables the button; keep the label, change its verb ("Deploying"). */
  loading?: boolean;
}
export declare const Button: React.ForwardRefExoticComponent<ButtonProps & React.RefAttributes<HTMLButtonElement>>;

export interface KbdProps { children: React.ReactNode; className?: string }
export declare function Kbd(props: KbdProps): React.ReactElement;

export interface StatusBadgeProps {
  status: Status;
  /** Defaults to the status word ("Healthy"). Pass a server or app name to label the subject instead. */
  label?: React.ReactNode;
  /** Muted trailing text, e.g. "for 6 min". */
  meta?: React.ReactNode;
  variant?: 'dot' | 'pill';
  title?: string;
  className?: string;
}
export declare function StatusBadge(props: StatusBadgeProps): React.ReactElement;

export interface GlyphProps {
  /** The status shape. Shape carries meaning with colour, so state never depends on colour alone. */
  /** dotted-ring is a ring with a dot in it, running; dashed-ring a dashed ring with a dash across it, unknown. */
  shape?: 'circle' | 'triangle' | 'square' | 'spinner' | 'ring' | 'dotted-ring' | 'dashed-ring' | 'info';
  /** Pixel size of the square glyph. Default 10. Colour comes from `currentColor`. */
  size?: number;
  className?: string;
}
/** The bare status shape used inside StatusBadge, for custom rows and legends. */
export declare function Glyph(props: GlyphProps): React.ReactElement;

export interface LogoProps {
  /** logotype: the full name with the dock-and-water D. mark: the D alone. icon: the app icon tile (fixed colours, for favicons and app stores). */
  variant?: 'logotype' | 'mark' | 'icon';
  /** Pixels. For logotype and mark this is the cap height of the D; for icon, the tile size. Defaults: logotype 20, mark and icon 24. */
  height?: number | string;
  /** One colour: the water takes the ink colour too. Use on photos, in print, or on coloured fills. */
  mono?: boolean;
  /** Accessible name. Default "Dockwell". */
  title?: string;
  className?: string;
}
/** The Dockwell logo. Ink follows currentColor; the water is var(--cobalt), so it adapts to the theme. */
export declare function Logo(props: LogoProps): React.ReactElement;

export interface TagProps {
  /** Key of a key:value selector, e.g. "region". */
  label?: string;
  value?: string;
  tone?: 'cobalt';
  icon?: IconName;
  onRemove?: () => void;
  children?: React.ReactNode;
  className?: string;
}
export declare function Tag(props: TagProps): React.ReactElement;

export interface TextFieldProps extends Omit<React.InputHTMLAttributes<HTMLInputElement>, 'size' | 'prefix'> {
  label?: React.ReactNode;
  hint?: React.ReactNode;
  /** Replaces the hint. Say what is wrong and how to fix it. */
  error?: React.ReactNode;
  prefix?: React.ReactNode;
  suffix?: React.ReactNode;
  icon?: IconName;
  /** Mono font for paths, hosts, commands, tags. */
  mono?: boolean;
  optional?: boolean;
  size?: 'sm' | 'md' | 'lg';
  /** A trailing control rendered inside the field, e.g. a small ghost Button. */
  action?: React.ReactNode;
}
export declare function TextField(props: TextFieldProps): React.ReactElement;

export interface SwitchProps {
  checked?: boolean;
  defaultChecked?: boolean;
  onChange?: (checked: boolean) => void;
  label?: React.ReactNode;
  description?: React.ReactNode;
  disabled?: boolean;
  className?: string;
}
export declare function Switch(props: SwitchProps): React.ReactElement;

export interface SegmentedOption { value: string; label: React.ReactNode; icon?: IconName; count?: number }
export interface SegmentedControlProps {
  options: SegmentedOption[];
  value?: string;
  defaultValue?: string;
  onChange?: (value: string) => void;
  /** Accessible name of the group. */
  label?: string;
  size?: 'sm' | 'md';
  className?: string;
}
export declare function SegmentedControl(props: SegmentedControlProps): React.ReactElement;

export interface TabItem { id: string; label: React.ReactNode; icon?: IconName; count?: number; countTone?: 'warning' | 'critical' | 'cobalt' }
export interface TabsProps { tabs: TabItem[]; value?: string; defaultValue?: string; onChange?: (id: string) => void; label?: string; className?: string }
export declare function Tabs(props: TabsProps): React.ReactElement;

export interface ScopeBarProps {
  org?: string;
  project?: string;
  environment?: string;
  /** Shows a lock after the environment when changes need approval. */
  protected?: boolean;
  onSelect?: (level: 'org' | 'project' | 'environment') => void;
  className?: string;
}
export declare function ScopeBar(props: ScopeBarProps): React.ReactElement;

export interface CommandItem { id: string; label: string; group?: string; icon?: IconName; hint?: string; kbd?: string }
export interface CommandBarProps {
  items: CommandItem[];
  /** Overlay mode: render only while open. */
  open?: boolean;
  onClose?: () => void;
  onSelect?: (item: CommandItem) => void;
  placeholder?: string;
  defaultQuery?: string;
  /** Footer text naming where commands apply, e.g. "Acme / storefront". */
  scope?: string;
  /** Render the panel in place, without scrim or fixed positioning. */
  inline?: boolean;
  autoFocus?: boolean;
  className?: string;
}
export declare function CommandBar(props: CommandBarProps): React.ReactElement | null;

export interface SparklineProps {
  data: number[];
  width?: number | string;
  height?: number;
  tone?: 'ink' | 'cobalt' | 'healthy' | 'warning' | 'critical' | 'idle';
  min?: number;
  max?: number;
  /** Dashed warning line at this value. */
  threshold?: number;
  area?: boolean;
  label?: string;
  className?: string;
}
export declare function Sparkline(props: SparklineProps): React.ReactElement;

export interface MetricProps {
  label: React.ReactNode;
  value: React.ReactNode;
  unit?: React.ReactNode;
  icon?: IconName;
  delta?: React.ReactNode;
  deltaTone?: 'healthy' | 'warning' | 'critical';
  data?: number[];
  tone?: SparklineProps['tone'];
  threshold?: number;
  min?: number;
  max?: number;
  caption?: React.ReactNode;
  className?: string;
}
export declare function Metric(props: MetricProps): React.ReactElement;

export interface TableColumn<Row> {
  key: string;
  header: React.ReactNode;
  width?: number | string;
  align?: 'left' | 'right';
  mono?: boolean;
  muted?: boolean;
  render?: (row: Row) => React.ReactNode;
}
export interface TableProps<Row = Record<string, unknown>> {
  columns: TableColumn<Row>[];
  rows: Row[];
  rowKey?: string;
  density?: 'comfortable' | 'compact';
  onRowClick?: (row: Row) => void;
  selectedKey?: unknown;
  empty?: React.ReactNode;
  caption?: string;
  maxHeight?: number | string;
  className?: string;
}
export declare function Table<Row = Record<string, unknown>>(props: TableProps<Row>): React.ReactElement;

export interface FleetServer { id: string; name: string; status: Status; cpu?: number | null; mem?: number | null; ip?: string; note?: string }
export interface FleetGroup { label?: string; meta?: string; servers: FleetServer[]; /** @deprecated use servers */ nodes?: FleetServer[] }
export interface FleetGridProps {
  groups?: FleetGroup[];
  servers?: FleetServer[];
  /** @deprecated use servers */
  nodes?: FleetServer[];
  selectedId?: string;
  onSelect?: (server: FleetServer) => void;
  size?: 'sm' | 'md';
  legend?: boolean;
  readout?: boolean;
  hint?: string;
  className?: string;
}
export declare function FleetGrid(props: FleetGridProps): React.ReactElement;

export interface LogLine { id?: string | number; time: string; source: string; level?: 'info' | 'warn' | 'error'; text: string }
export interface LogStreamProps {
  lines: LogLine[];
  title?: React.ReactNode;
  live?: boolean;
  follow?: boolean;
  onFollowChange?: (follow: boolean) => void;
  height?: number;
  toolbar?: React.ReactNode;
  className?: string;
}
export declare function LogStream(props: LogStreamProps): React.ReactElement;

export type PlanValue = string | number | { secret: true };
export interface PlanChange { path: string; from?: PlanValue; to?: PlanValue }
export interface PlanResource {
  id: string;
  action: 'create' | 'update' | 'replace' | 'destroy';
  kind: 'app' | 'service' | 'domain' | 'secret' | 'database' | 'job' | 'volume' | 'backup' | 'package' | 'server' | 'firewall' | string;
  kindLabel?: string;
  name: string;
  /** Replaces the default action word on the right, e.g. "Rolling update". */
  note?: string;
  changes?: PlanChange[];
  expanded?: boolean;
  /** Replaced as it was, a restart: counts as "to restart", apart from a replace. */
  restart?: boolean;
}
export interface PlanDiffProps { resources: PlanResource[]; impact?: React.ReactNode; className?: string }
export declare function PlanDiff(props: PlanDiffProps): React.ReactElement;

export interface TimelineEvent {
  id: string | number;
  kind: 'deploy' | 'config' | 'alert' | 'rollback' | 'server' | 'plan' | 'backup';
  title: React.ReactNode;
  tone?: 'healthy' | 'warning' | 'critical' | 'cobalt';
  status?: Status;
  /** The state in words of its own, such as "Unhealthy" for degraded, where the status's word isn't it. */
  statusLabel?: string;
  revision?: string;
  actor?: string;
  meta?: React.ReactNode;
  time: string;
  datetime?: string;
  undoable?: boolean;
  undoLabel?: string;
}
export interface TimelineProps { events: TimelineEvent[]; onUndo?: (event: TimelineEvent) => void; className?: string }
export declare function Timeline(props: TimelineProps): React.ReactElement;

export interface CalloutProps { tone?: Tone; title?: React.ReactNode; children?: React.ReactNode; actions?: React.ReactNode; className?: string }
export declare function Callout(props: CalloutProps): React.ReactElement;

export interface ToastProps {
  tone?: Tone;
  title: React.ReactNode;
  description?: React.ReactNode;
  action?: { label: string; onClick?: () => void; icon?: IconName; kbd?: string };
  onClose?: () => void;
  /** Auto-dismiss time in ms; draws a timer that pauses on hover. The caller removes the toast. */
  duration?: number;
  className?: string;
}
export declare function Toast(props: ToastProps): React.ReactElement;

export interface SheetProps {
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  open?: boolean;
  onClose?: () => void;
  footer?: React.ReactNode;
  children?: React.ReactNode;
  width?: number | string;
  inline?: boolean;
  className?: string;
}
export declare function Sheet(props: SheetProps): React.ReactElement | null;

export interface IconProps { name: IconName; size?: number; label?: string; strokeWidth?: number; className?: string }
export declare function Icon(props: IconProps): React.ReactElement;

declare global {
  interface Window {
    Dockwell: {
      Button: typeof Button; Kbd: typeof Kbd; StatusBadge: typeof StatusBadge; Tag: typeof Tag; TextField: typeof TextField;
      Switch: typeof Switch; SegmentedControl: typeof SegmentedControl; Tabs: typeof Tabs; ScopeBar: typeof ScopeBar;
      CommandBar: typeof CommandBar; Metric: typeof Metric; Sparkline: typeof Sparkline; Table: typeof Table;
      FleetGrid: typeof FleetGrid; LogStream: typeof LogStream; PlanDiff: typeof PlanDiff; Timeline: typeof Timeline;
      Callout: typeof Callout; Toast: typeof Toast; Sheet: typeof Sheet; Icon: typeof Icon & { names: IconName[] };
    };
  }
}
