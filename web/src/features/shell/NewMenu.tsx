// SPDX-License-Identifier: AGPL-3.0-only

import { Button, Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type KeyboardEvent, useEffect, useRef, useState } from "react";

/**
 * The top bar's New: a short menu of what can be made from anywhere, each going
 * straight to its own sheet: an app (New app, the catalog first), a database, a
 * domain, a server (#617).
 */
export function NewMenu() {
  const { t } = useLingui();
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    root.current?.querySelector<HTMLAnchorElement>('[role="menuitem"]')?.focus();
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open]);
  const keys = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = [...(root.current?.querySelectorAll<HTMLAnchorElement>('[role="menuitem"]') ?? [])];
    const at = items.indexOf(document.activeElement as HTMLAnchorElement);
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
        <div className="new-menu" role="menu" aria-label={t`New`} onKeyDown={keys}>
          <Link to="/apps/new" role="menuitem" className="new-menu-item" onClick={() => setOpen(false)}>
            <Icon name="service" size={16} />
            <Trans>App</Trans>
          </Link>
          <Link
            to="/databases"
            search={{ new: "postgres" }}
            role="menuitem"
            className="new-menu-item"
            onClick={() => setOpen(false)}
          >
            <Icon name="database" size={16} />
            <Trans>Database</Trans>
          </Link>
          <Link
            to="/domains"
            search={{ add: true }}
            role="menuitem"
            className="new-menu-item"
            onClick={() => setOpen(false)}
          >
            <Icon name="globe" size={16} />
            <Trans>Domain</Trans>
          </Link>
          <Link
            to="/servers"
            search={{ add: true }}
            role="menuitem"
            className="new-menu-item"
            onClick={() => setOpen(false)}
          >
            <Icon name="server" size={16} />
            <Trans>Server</Trans>
          </Link>
        </div>
      ) : null}
    </span>
  );
}
