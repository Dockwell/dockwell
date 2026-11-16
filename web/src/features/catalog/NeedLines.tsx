// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type CatalogInstall, CatalogService, type Engine, type Entry } from "@dockwell/api";
import { Button } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { environmentOf, imageOf, nameOf } from "../apps/app";
import { usePlaces, useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { tagOf } from "../changes/upgrade";
import type { CardRow } from "./CardRows";
import { NeedBackups } from "./NeedBackups";
import { engineName } from "./needs";

/**
 * What meets each of an install's needs, on its app's card, as rows (#722): the need,
 * what meets it, and Use another, which lists the servers Dockwell runs that take it,
 * each one plan away (ADR 0084). A database it backs up alone has a row for its
 * backups. A server it shares is named where it is now, with the way to it, or as in
 * a project the reader can't see.
 */
export function useNeedRows(entry: Entry | undefined, install: CatalogInstall): CardRow[] {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const places = usePlaces();
  const services = useServices().data ?? [];
  const [open, setOpen] = useState<string>();
  const [backups, setBackups] = useState<string>();
  const id = entry?.id ?? "";
  const options = useQuery(CatalogService.method.getCatalogEntry, { id }, { enabled: (entry?.needs.length ?? 0) > 0 })
    .data?.needs;
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay()(CatalogService.method.setCatalogNeed, { project: install.projectId });
  if (!entry) return [];
  const needs = entry.needs.filter((n) => install.pin?.needs[n.name]);
  const app = install.name || entry.name;
  const use = (need: string, choice: string, startEmpty = false) =>
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(CatalogService, transport).setCatalogNeed({ id: install.projectId, need, choice, startEmpty }),
    });
  const rows: CardRow[] = [];
  for (const need of needs) {
    const pinned = install.pin?.needs[need.name];
    const what = need.label.toLowerCase();
    const engine = engineName(Number(pinned?.engine ?? 0) as Engine);
    const shared = pinned?.environment ?? "";
    const place = shared ? places.place(shared) : undefined;
    const meets = pinned?.meets ?? "";
    const server = services.find((s) => environmentOf(s) === shared && nameOf(s) === meets);
    // The version the database runs now, as an upgrade of it moves it on; the pin keeps
    // the version it was installed at.
    const image = server ? imageOf(server) : "";
    const version = image ? tagOf(image) : (pinned?.version ?? "");
    const runs = `${engine} ${version === "latest" ? (pinned?.version ?? "") : version}`.trim();
    const where = place?.label ?? "";
    let value: ReactNode;
    if (!shared) {
      value = (
        <Trans>
          Its own {runs}, beside {app}
        </Trans>
      );
    } else if (!place) {
      // Where it is now isn't the reader's to see: its project is someone else's.
      value = <Trans>Shares {runs} in a project you can't see</Trans>;
    } else {
      const named = server ? (
        <Link to="/apps/$serviceId" params={{ serviceId: server.id }}>
          {meets}
        </Link>
      ) : (
        <code>{meets}</code>
      );
      value = (
        <>
          <Trans>
            Shares {named} in the project {where}
          </Trans>{" "}
          <span className="card-row-quiet">· {runs}</span>
        </>
      );
    }
    const elsewhere = (options ?? []).filter(
      (o) =>
        o.need === need.name &&
        o.service !== "" &&
        o.why === "" &&
        !(shared && o.project === place?.project && o.service === meets),
    );
    rows.push({
      key: `need-${need.name}`,
      label: need.label,
      value,
      action: (
        <Button
          variant="ghost"
          size="sm"
          aria-expanded={open === need.name}
          onClick={() => setOpen(open === need.name ? undefined : need.name)}
          {...(elsewhere.length === 0 ? { disabled: true, title: t`Nothing else Dockwell runs takes it` } : may.off)}
        >
          <Trans>Use another {what}</Trans>
        </Button>
      ),
      under:
        open === need.name ? (
          <ul className="catalog-need-line-ways" aria-label={t`Where else it can run`}>
            {elsewhere.map((o) => {
              const there = o.service;
              const project = o.project;
              // Its data comes along where both run the same engine; otherwise starting
              // empty is a choice of its own, never a default (ADR 0084).
              const copies = Number(o.engine) === Number(pinned?.engine ?? 0);
              return (
                <li key={o.choice}>
                  <span className="catalog-need-line-way">
                    <span>
                      <Trans>
                        {there} in {project}
                      </Trans>
                    </span>
                    <span className="catalog-need-line-now">
                      {copies ? <Trans>Copies its data</Trans> : <Trans>Its data can't be copied there</Trans>}
                    </span>
                  </span>
                  <code>{`${engineName(o.engine)} ${o.version}`.trim()}</code>
                  <Button variant="secondary" size="sm" onClick={() => use(need.name, o.choice, !copies)} {...may.off}>
                    {copies ? <Trans>Use it</Trans> : <Trans>Start empty on it</Trans>}
                  </Button>
                </li>
              );
            })}
          </ul>
        ) : undefined,
    });
    if (sqlEngines.includes(engine)) {
      rows.push({
        key: `backups-${need.name}`,
        label: <Trans>Backups</Trans>,
        value: <Trans>Of its {what} alone</Trans>,
        action: (
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={backups === need.name}
            onClick={() => setBackups(backups === need.name ? undefined : need.name)}
          >
            <Trans>Its backups</Trans>
          </Button>
        ),
        under: backups === need.name ? <NeedBackups install={install} need={need.name} /> : undefined,
      });
    }
  }
  return rows;
}

/** The engines whose one database an app backs up and restores alone (ADR 0084). */
const sqlEngines = ["PostgreSQL", "MySQL", "MariaDB"];
