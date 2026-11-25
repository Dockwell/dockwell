// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { type TerminalPreferences, TerminalPreferencesSchema } from "@dockwell/api";
import { Button, SegmentedControl, SelectField, Sheet, Switch } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useRef } from "react";

import {
  DEFAULT_SCROLLBACK,
  DESKTOP_FONT,
  FONT_PRESETS,
  type FontFamilyKey,
  LINE_HEIGHT_PRESETS,
  MAX_FONT_SIZE,
  MIN_FONT_SIZE,
  PHONE_FONT,
  type ResolvedDisplaySettings,
  SCROLLBACK_PRESETS,
  type XtermCursorStyle,
  xtermToCursorStyle,
} from "./preferences";

export interface SettingsPopoverProps {
  open: boolean;
  phone: boolean;
  resolved: ResolvedDisplaySettings;
  raw: TerminalPreferences | undefined;
  saving: boolean;
  error?: string;
  onOpenChange: (open: boolean) => void;
  onChange: (next: TerminalPreferences) => void;
  onReset: () => void;
}

const FONT_SIZES = [10, 11, 12, 12.5, 13, 14, 15, 16, 17, 18, 20, 22, 24] as const;

export function TerminalSettingsControl({
  open,
  phone,
  resolved,
  raw,
  saving,
  error,
  onOpenChange,
  onChange,
  onReset,
}: SettingsPopoverProps) {
  const { t } = useLingui();
  const root = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    if (!open || phone) return;
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) onOpenChange(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        onOpenChange(false);
      }
    };
    document.addEventListener("pointerdown", away);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("pointerdown", away);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open, phone, onOpenChange]);

  const patch = (updates: Partial<TerminalPreferences>) => {
    const base = create(TerminalPreferencesSchema, {
      fontSize: raw?.fontSize ?? 0,
      fontFamily: raw?.fontFamily ?? "",
      lineHeight: raw?.lineHeight ?? 0,
      cursorStyle: raw?.cursorStyle ?? resolved.cursorStyleEnum,
      cursorBlink: raw?.cursorBlink ?? resolved.cursorBlink,
      scrollback: raw?.scrollback ?? 0,
      ...updates,
    });
    onChange(base);
  };

  const fontLabel = (key: FontFamilyKey): string => {
    switch (key) {
      case "default":
        return t`JetBrains Mono (panel mono)`;
      case "system-mono":
        return t`System monospace`;
      case "fira-code":
        return t`Fira Code`;
      case "ibm-plex-mono":
        return t`IBM Plex Mono`;
      case "source-code-pro":
        return t`Source Code Pro`;
    }
  };

  const defaultFontPx = phone ? PHONE_FONT : DESKTOP_FONT;
  const currentFontSetting = raw?.fontSize ?? 0;

  const content = (
    <div className="terminal-settings-body">
      <div className="terminal-settings-row">
        <div className="terminal-settings-field">
          <SelectField
            label={t`Font size`}
            size="sm"
            value={String(currentFontSetting)}
            options={[
              { value: "0", label: t`Default (${defaultFontPx} px)` },
              ...FONT_SIZES.map((sz) => ({ value: String(sz), label: t`${sz} px` })),
            ]}
            onChange={(e) => {
              const n = Number(e.target.value);
              if (n === 0 || (n >= MIN_FONT_SIZE && n <= MAX_FONT_SIZE)) {
                patch({ fontSize: n });
              }
            }}
          />
        </div>
        <div className="terminal-settings-field">
          <SelectField
            label={t`Line height`}
            size="sm"
            value={String(resolved.lineHeightSetting)}
            options={LINE_HEIGHT_PRESETS.map((lh) => ({
              value: String(lh),
              label: lh === 0 ? t`Default` : String(lh),
            }))}
            onChange={(e) => patch({ lineHeight: Number(e.target.value) })}
          />
        </div>
      </div>

      <SelectField
        label={t`Font family`}
        size="sm"
        value={resolved.fontFamilyKey}
        options={FONT_PRESETS.map((p) => ({ value: p.key, label: fontLabel(p.key) }))}
        onChange={(e) => patch({ fontFamily: e.target.value === "default" ? "" : e.target.value })}
      />

      <div className="terminal-settings-group">
        <span className="terminal-settings-label">
          <Trans>Cursor style</Trans>
        </span>
        <SegmentedControl<XtermCursorStyle>
          label={t`Cursor style`}
          size="sm"
          value={resolved.cursorStyle}
          options={[
            { value: "block", label: t`Block` },
            { value: "underline", label: t`Underline` },
            { value: "bar", label: t`Bar` },
          ]}
          onChange={(style) => patch({ cursorStyle: xtermToCursorStyle(style) })}
        />
      </div>

      <div className="terminal-settings-switch">
        <Switch
          label={t`Blink cursor`}
          checked={resolved.cursorBlink}
          onChange={(checked) => patch({ cursorBlink: checked })}
        />
      </div>

      <SelectField
        label={t`Scrollback lines`}
        size="sm"
        value={String(raw?.scrollback || DEFAULT_SCROLLBACK)}
        options={SCROLLBACK_PRESETS.map((n) => ({
          value: String(n),
          label: n === DEFAULT_SCROLLBACK ? t`${n.toLocaleString()} (default)` : n.toLocaleString(),
        }))}
        onChange={(e) => {
          const n = Number(e.target.value);
          patch({ scrollback: n === DEFAULT_SCROLLBACK ? 0 : n });
        }}
      />

      {error ? (
        <p className="terminal-settings-error" role="alert">
          {error}
        </p>
      ) : null}

      <div className="terminal-settings-foot">
        <Button variant="ghost" size="sm" onClick={onReset} disabled={saving}>
          <Trans>Reset to defaults</Trans>
        </Button>
        <span className="terminal-settings-status">{saving ? t`Saving…` : t`Applied live`}</span>
      </div>
    </div>
  );

  return (
    <span className="terminal-menu-anchor" ref={root}>
      <Button
        variant="ghost"
        size="sm"
        icon="settings"
        aria-label={t`Terminal settings`}
        aria-expanded={open}
        aria-haspopup={phone ? "dialog" : "true"}
        onClick={() => onOpenChange(!open)}
      />
      {open && !phone ? (
        <section className="terminal-settings-popover" aria-label={t`Terminal settings`}>
          <div className="terminal-settings-head">
            <h2 className="terminal-settings-title">
              <Trans>Display settings</Trans>
            </h2>
            <Button
              variant="ghost"
              size="sm"
              icon="x"
              aria-label={t`Close settings`}
              onClick={() => onOpenChange(false)}
            />
          </div>
          {content}
        </section>
      ) : null}
      {phone ? (
        <Sheet open={open} onClose={() => onOpenChange(false)} title={t`Terminal settings`}>
          {content}
        </Sheet>
      ) : null}
    </span>
  );
}
