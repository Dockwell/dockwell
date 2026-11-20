// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { type KeyboardEvent, useEffect, useId, useMemo, useRef, useState } from "react";

import { browserTimeZone, formatClock, formatTimeZoneOffset, timeZoneOffsetMinutes } from "../../lib/format";

/** Turns an IANA zone ID into readable words: "America/Los_Angeles" -> "America/Los Angeles". */
export function formatZoneName(zone: string): string {
  return zone.replaceAll("_", " ");
}

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

/** Searchable offset strings so "+9", "+09", "+09:00", "UTC+9", "GMT+9", "-8", etc. all match. */
function offsetSearchTokens(minutes: number): string {
  if (minutes === 0) return "utc utc+0 utc+00 utc+00:00 utc-0 +0 +00 +00:00 +0000 gmt gmt+0";
  const sign = minutes < 0 ? "-" : "+";
  const abs = Math.abs(minutes);
  const h = Math.floor(abs / 60);
  const m = abs % 60;
  const hh = pad2(h);
  const mm = pad2(m);
  const base = [
    `utc${sign}${hh}:${mm}`,
    `utc${sign}${h}:${mm}`,
    `gmt${sign}${hh}:${mm}`,
    `gmt${sign}${h}:${mm}`,
    `${sign}${hh}:${mm}`,
    `${sign}${h}:${mm}`,
    `${sign}${hh}${mm}`,
  ];
  if (m === 0) {
    base.push(`utc${sign}${hh}`, `utc${sign}${h}`, `gmt${sign}${hh}`, `gmt${sign}${h}`, `${sign}${hh}`, `${sign}${h}`);
  }
  return base.join(" ");
}

interface ZoneEntry {
  value: string;
  label: string;
  detail?: string;
  offset: string;
  search: string;
}

function supportedZones(selected: string): string[] {
  const raw = typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : ["UTC"];
  const seen = new Set<string>();
  const list: string[] = [];
  const add = (z: string) => {
    if (!z || seen.has(z)) return;
    seen.add(z);
    list.push(z);
  };
  add("UTC");
  for (const z of raw) add(z);
  if (selected) add(selected);
  return list;
}

export interface TimeZonePickerProps {
  value: string;
  onChange: (zone: string) => void;
  disabled?: boolean;
  error?: string;
}

/**
 * Searchable IANA time zone picker for Profile (#834): "This browser's" always
 * comes first with the zone it resolves to beside it, and searching filters by
 * city, region, or UTC offset.
 */
export function TimeZonePicker({ value, onChange, disabled, error }: TimeZonePickerProps) {
  const { t, i18n } = useLingui();
  const triggerId = useId();
  const listboxId = useId();
  const hintId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const optionRefs = useRef<Array<HTMLDivElement | null>>([]);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  // Snapshot "now" on mount and each open so offsets and the preview clock stay consistent while browsing.
  const [now, setNow] = useState(() => new Date());

  const browser = useMemo(() => browserTimeZone(), []);
  const browserOffset = useMemo(() => formatTimeZoneOffset(browser, now), [browser, now]);
  const selectedZone = value || browser;
  const selectedOffset = useMemo(() => formatTimeZoneOffset(selectedZone, now), [selectedZone, now]);
  const liveClock = useMemo(() => formatClock(now, i18n.locale, selectedZone), [now, i18n.locale, selectedZone]);

  const browserOptionLabel = t`This browser's`;
  const entries = useMemo<ZoneEntry[]>(() => {
    const browserMinutes = timeZoneOffsetMinutes(browser, now);
    const browserFormatted = formatZoneName(browser);
    const browserEntry: ZoneEntry = {
      value: "",
      label: browserOptionLabel,
      detail: browserFormatted,
      offset: browserOffset,
      search:
        `${browserOptionLabel} this browser's browser default ${browser} ${browserFormatted} ${offsetSearchTokens(browserMinutes)}`.toLowerCase(),
    };
    const rest = supportedZones(value).map((z): ZoneEntry => {
      const display = formatZoneName(z);
      const minutes = timeZoneOffsetMinutes(z, now);
      const offset = formatTimeZoneOffset(z, now);
      return {
        value: z,
        label: display,
        offset,
        search: `${z} ${display} ${offsetSearchTokens(minutes)}`.toLowerCase(),
      };
    });
    return [browserEntry, ...rest];
  }, [browser, browserOffset, browserOptionLabel, now, value]);

  const filtered = useMemo(() => {
    const trimmed = query.trim().toLowerCase();
    if (!trimmed) return entries;
    const tokens = trimmed.split(/\s+/).filter(Boolean);
    return entries.filter((entry) => tokens.every((tok) => entry.search.includes(tok)));
  }, [entries, query]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(event.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onPointerDown);
    return () => document.removeEventListener("mousedown", onPointerDown);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    searchRef.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const el = optionRefs.current[activeIndex];
    if (el && typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "nearest" });
  }, [activeIndex, open]);

  const openPicker = () => {
    if (disabled) return;
    setNow(new Date());
    setQuery("");
    const currentIdx = entries.findIndex((e) => e.value === value);
    setActiveIndex(currentIdx >= 0 ? currentIdx : 0);
    setOpen(true);
  };

  const closePicker = (restoreFocus = false) => {
    setOpen(false);
    setQuery("");
    if (restoreFocus) triggerRef.current?.focus();
  };

  const pick = (next: string) => {
    onChange(next);
    closePicker(true);
  };

  const onSearchKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      closePicker(true);
      return;
    }
    if (event.key === "ArrowDown") {
      event.preventDefault();
      if (filtered.length > 0) setActiveIndex((i) => (i + 1) % filtered.length);
      return;
    }
    if (event.key === "ArrowUp") {
      event.preventDefault();
      if (filtered.length > 0) setActiveIndex((i) => (i - 1 + filtered.length) % filtered.length);
      return;
    }
    if (event.key === "Home") {
      event.preventDefault();
      if (filtered.length > 0) setActiveIndex(0);
      return;
    }
    if (event.key === "End") {
      event.preventDefault();
      if (filtered.length > 0) setActiveIndex(filtered.length - 1);
      return;
    }
    if (event.key === "Enter") {
      event.preventDefault();
      const chosen = filtered[activeIndex];
      if (chosen) pick(chosen.value);
    }
  };

  const safeActive = filtered.length > 0 ? Math.min(activeIndex, filtered.length - 1) : -1;
  const activeOptionId = safeActive >= 0 ? `${listboxId}-opt-${safeActive}` : undefined;
  const browserDisplay = formatZoneName(browser);

  return (
    <div ref={rootRef} className="dw-field tz-picker">
      <label className="dw-field-label" htmlFor={triggerId}>
        {t`Time zone`}
      </label>
      <button
        ref={triggerRef}
        id={triggerId}
        type="button"
        className={cx("tz-picker-trigger", open && "is-open", error && "is-invalid")}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listboxId : undefined}
        aria-describedby={hintId}
        disabled={disabled}
        onClick={() => (open ? closePicker(false) : openPicker())}
      >
        <span className="tz-picker-value">
          <span className="tz-picker-primary">{value === "" ? browserOptionLabel : formatZoneName(value)}</span>
          <span className="tz-picker-meta">
            {value === "" ? `${browserDisplay} · ${browserOffset}` : selectedOffset}
          </span>
        </span>
        <Icon name={open ? "chevron-up" : "chevron-down"} size={14} className="tz-picker-chevron" />
      </button>
      {open ? (
        <div className="tz-picker-panel">
          <div className="tz-picker-search-wrap">
            <Icon name="search" size={14} className="tz-picker-search-icon" />
            <input
              ref={searchRef}
              type="search"
              role="combobox"
              className="tz-picker-search"
              aria-label={t`Search time zones by city or offset`}
              aria-autocomplete="list"
              aria-expanded={true}
              aria-controls={listboxId}
              aria-activedescendant={activeOptionId}
              placeholder={t`Search by city or offset, e.g. Tokyo or +09:00`}
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                setActiveIndex(0);
              }}
              onKeyDown={onSearchKeyDown}
            />
          </div>
          {filtered.length > 0 ? (
            <div id={listboxId} role="listbox" aria-label={t`Time zones`} className="tz-picker-list">
              {filtered.map((entry, idx) => {
                const selected = entry.value === value;
                const active = idx === safeActive;
                return (
                  <div
                    key={entry.value || "__browser__"}
                    id={`${listboxId}-opt-${idx}`}
                    ref={(el) => {
                      optionRefs.current[idx] = el;
                    }}
                    role="option"
                    tabIndex={-1}
                    aria-selected={selected}
                    className={cx("tz-picker-option", selected && "is-selected", active && "is-active")}
                    onMouseDown={(event) => event.preventDefault()}
                    onMouseEnter={() => setActiveIndex(idx)}
                    onClick={() => pick(entry.value)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault();
                        pick(entry.value);
                      }
                    }}
                  >
                    <span className="tz-picker-option-main">
                      <span className="tz-picker-option-label">{entry.label}</span>
                      {entry.detail ? <span className="tz-picker-option-detail">{entry.detail}</span> : null}
                    </span>
                    <span className="tz-picker-option-end">
                      <span className="tz-picker-option-offset">{entry.offset}</span>
                      {selected ? <Icon name="check" size={14} className="tz-picker-option-check" /> : null}
                    </span>
                  </div>
                );
              })}
            </div>
          ) : (
            <p id={listboxId} className="tz-picker-empty">
              {t`No time zones match “${query.trim()}”.`}
            </p>
          )}
        </div>
      ) : null}
      {error ? (
        <span id={hintId} className="dw-field-error" role="alert">
          {error}
        </span>
      ) : (
        <span id={hintId} className="dw-field-hint">
          {t`Currently ${liveClock} (${selectedOffset}). Used for times across Dockwell and new schedules.`}
        </span>
      )}
    </div>
  );
}
