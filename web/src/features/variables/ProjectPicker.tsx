// SPDX-License-Identifier: AGPL-3.0-only

import { Icon, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type KeyboardEvent, useEffect, useId, useMemo, useRef, useState } from "react";

import { countKinds } from "../projects/contents";
import type { ProjectView } from "../projects/useProjects";

/** A variable or secret found by its name, in any project, from the picker's box. */
export interface Found {
  project: ProjectView;
  environment: string;
  name: string;
  secret: boolean;
}

/** The most names the picker lists for a search, as the rest are a sharper search away. */
const FOUND_AT_MOST = 8;

/**
 * Chooses a project on the Variables page, in place of a switch with every environment in it
 * (ADR 0074): a button that says which, and a list to search, with each project's counts, those
 * with nothing running apart and marked. What's typed also finds variables and secrets by name
 * in every project, one step away for whoever knows the name and not where it is.
 */
export function ProjectPicker({
  views,
  chosen,
  onChoose,
  onFind,
}: {
  views: readonly ProjectView[];
  chosen: ProjectView | undefined;
  onChoose: (view: ProjectView) => void;
  onFind: (found: Found) => void;
}) {
  const { t } = useLingui();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const listId = useId();

  // A click outside or Escape closes it, and focus goes back to what opened it.
  useEffect(() => {
    if (!open) return;
    const away = (event: MouseEvent) => {
      if (root.current && !root.current.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", away);
    return () => document.removeEventListener("mousedown", away);
  }, [open]);

  const q = query.trim().toLowerCase();
  const matches = (view: ProjectView) => q === "" || view.label.toLowerCase().includes(q) || view.name.includes(q);
  const running = views.filter((v) => (!v.idle || v.isDefault) && matches(v));
  const idle = views.filter((v) => v.idle && !v.isDefault && matches(v));
  const found = useMemo<Found[]>(() => {
    if (q.length < 2) return [];
    const out: Found[] = [];
    for (const view of views) {
      for (const r of view.inside) {
        const kind = r.resource?.kind;
        if ((kind === "variable" || kind === "secret") && r.resource?.name.toLowerCase().includes(q)) {
          out.push({ project: view, environment: r.resource.scope, name: r.resource.name, secret: kind === "secret" });
        }
      }
    }
    return out.sort((a, b) => a.name.localeCompare(b.name) || a.project.label.localeCompare(b.project.label));
  }, [views, q]);

  const close = () => {
    setOpen(false);
    setQuery("");
    trigger.current?.focus();
  };
  const choose = (view: ProjectView) => {
    onChoose(view);
    close();
  };
  const find = (f: Found) => {
    onFind(f);
    close();
  };
  // Up and down move between what's listed; the box keeps typing.
  const onKey = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      close();
      return;
    }
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    const items = [...(root.current?.querySelectorAll<HTMLButtonElement>(".variables-pick-item") ?? [])];
    if (items.length === 0) return;
    event.preventDefault();
    const at = items.indexOf(document.activeElement as HTMLButtonElement);
    const next = event.key === "ArrowDown" ? (at + 1) % items.length : at <= 0 ? items.length - 1 : at - 1;
    items[next]?.focus();
  };

  const counts = (view: ProjectView) => {
    const kinds = countKinds(view.inside);
    return [
      plural(kinds.get("variable") ?? 0, { one: "# variable", other: "# variables" }),
      plural(kinds.get("secret") ?? 0, { one: "# secret", other: "# secrets" }),
    ].join(" · ");
  };
  const item = (view: ProjectView) => (
    <li key={view.id}>
      <button
        type="button"
        className="variables-pick-item"
        aria-current={view.id === chosen?.id ? "true" : undefined}
        onClick={() => choose(view)}
      >
        <span className="variables-pick-name">{view.label}</span>{" "}
        <span className="variables-pick-sub">{counts(view)}</span>
        {view.id === chosen?.id ? <Icon name="check" size={14} className="variables-pick-check" /> : null}
      </button>
    </li>
  );

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: Escape and the arrows are the list's, wherever focus is in it.
    <div className="variables-pick" ref={root} onKeyDown={onKey}>
      <button
        ref={trigger}
        type="button"
        className="variables-pick-trigger"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-label={chosen ? t`Project ${chosen.label}, ${counts(chosen)}` : t`Choose a project`}
        onClick={() => setOpen((was) => !was)}
      >
        <Icon name="folder" size={16} />
        <span className="variables-pick-label">{chosen?.label ?? <Trans>Choose a project</Trans>}</span>
        <Icon name="chevron-down" size={14} />
      </button>
      {open ? (
        <div className="variables-pick-pop" id={listId} role="dialog" aria-label={t`Choose a project`}>
          <TextField
            autoFocus
            icon="search"
            type="search"
            size="sm"
            aria-label={t`Find a project, or a variable by name`}
            placeholder={t`Project, or a variable's name`}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
          <div className="variables-pick-lists">
            {running.length > 0 ? (
              <section aria-label={t`Projects`}>
                <ul className="variables-pick-list">{running.map(item)}</ul>
              </section>
            ) : null}
            {idle.length > 0 ? (
              <section aria-label={t`Nothing running`}>
                <p className="variables-pick-head" aria-hidden="true">
                  <Trans>Nothing running</Trans>
                </p>
                <ul className="variables-pick-list">{idle.map(item)}</ul>
              </section>
            ) : null}
            {found.length > 0 ? (
              <section aria-label={t`Found by name`}>
                <p className="variables-pick-head" aria-hidden="true">
                  <Trans>Found by name</Trans>
                </p>
                <ul className="variables-pick-list">
                  {found.slice(0, FOUND_AT_MOST).map((f) => (
                    <li key={`${f.environment}/${f.name}`}>
                      <button type="button" className="variables-pick-item" onClick={() => find(f)}>
                        <span className="variables-pick-name">
                          <code>{f.name}</code>
                          {f.secret ? <Icon name="lock" size={12} /> : null}
                        </span>{" "}
                        <span className="variables-pick-sub">{f.project.label}</span>
                      </button>
                    </li>
                  ))}
                </ul>
              </section>
            ) : null}
            {running.length + idle.length + found.length === 0 ? (
              <p className="variables-pick-none">
                <Trans>No project or variable by that name.</Trans>
              </p>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}
