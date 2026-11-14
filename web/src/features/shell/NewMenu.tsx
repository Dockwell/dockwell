// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, ServerService } from "@dockwell/api";
import { Button, Icon, type IconName } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, type LinkProps } from "@tanstack/react-router";
import { type FocusEvent, type KeyboardEvent, type ReactNode, useEffect, useRef, useState } from "react";

import { useMay } from "../auth/may";

/**
 * The top bar's New: a short menu of what can be made from anywhere, each going
 * straight to its own sheet: an app (New app, the catalog first), a database, a
 * domain, a server (#617). What the reader may not make stays in it, off, with why
 * (ADR 0059).
 */
export function NewMenu() {
  const { t } = useLingui();
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange);
  const mayJoin = may(ServerService.method.createJoinTicket);
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    root.current?.querySelector<HTMLElement>('[role="menuitem"]:not([aria-disabled="true"])')?.focus();
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open]);
  const keys = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = [...(root.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
    const at = items.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      e.preventDefault();
      setOpen(false);
      root.current?.querySelector<HTMLButtonElement>("button")?.focus();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      items[(at + step + items.length) % items.length]?.focus();
    }
  };
  // Focus leaving the menu and its button, as a Tab past the last item, closes it.
  const left = (e: FocusEvent<HTMLDivElement>) => {
    if (!root.current?.contains(e.relatedTarget as Node | null)) setOpen(false);
  };
  const item = (
    icon: IconName,
    label: ReactNode,
    link: Pick<LinkProps, "to" | "search">,
    off: { off?: string; disabled?: true },
  ) =>
    off.off || off.disabled ? (
      <span role="menuitem" tabIndex={-1} aria-disabled="true" className="new-menu-item is-off" title={off.off}>
        <Icon name={icon} size={16} />
        {label}
      </span>
    ) : (
      <Link {...link} role="menuitem" className="new-menu-item" onClick={() => setOpen(false)}>
        <Icon name={icon} size={16} />
        {label}
      </Link>
    );
  return (
    <span className="new-menu-anchor" ref={root}>
      <Button
        variant="primary"
        icon="plus"
        className="topbar-new"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        <Trans>New</Trans>
      </Button>
      {open ? (
        <div className="new-menu" role="menu" aria-label={t`New`} onKeyDown={keys} onBlur={left}>
          {item("service", <Trans>App</Trans>, { to: "/apps/new" }, mayPlan.off)}
          {item("database", <Trans>Database</Trans>, { to: "/databases", search: { new: "postgres" } }, mayPlan.off)}
          {item("globe", <Trans>Domain</Trans>, { to: "/domains", search: { add: true } }, mayPlan.off)}
          {item("server", <Trans>Server</Trans>, { to: "/servers", search: { add: true } }, mayJoin.off)}
        </div>
      ) : null}
    </span>
  );
}
