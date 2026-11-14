// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type CatalogInstall, CatalogService, type Engine, type Entry } from "@dockwell/api";
import { Button } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { NeedBackups } from "./NeedBackups";
import { engineName } from "./needs";

/**
 * What meets each of an install's needs, on its app's page, with Use another database:
 * the servers Dockwell runs that take it, each one plan away (ADR 0084). For now a need
 * moves to a server you run; a new one beside the app comes once Dockwell copies data.
 */
export function NeedLines({ entry, install }: { entry: Entry; install: CatalogInstall }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const places = usePlaces();
  const [open, setOpen] = useState<string>();
  const [backups, setBackups] = useState<string>();
  const options = useQuery(CatalogService.method.getCatalogEntry, { id: entry.id }, { enabled: entry.needs.length > 0 })
    .data?.needs;
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay()(CatalogService.method.setCatalogNeed, { project: install.projectId });
  const needs = entry.needs.filter((n) => install.pin?.needs[n.name]);
  if (needs.length === 0) return null;
  const app = install.name || entry.name;
  const use = (need: string, choice: string, startEmpty = false) =>
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(CatalogService, transport).setCatalogNeed({ id: install.projectId, need, choice, startEmpty }),
    });
  return (
    <ul className="catalog-need-lines" aria-label={t`What it needs`}>
      {needs.map((need) => {
        const pinned = install.pin?.needs[need.name];
        const what = need.label.toLowerCase();
        const engine = engineName(Number(pinned?.engine ?? 0) as Engine);
        const version = pinned?.version ?? "";
        const project = pinned?.environment ? (places.place(pinned.environment)?.project ?? "") : "";
        const meets = pinned?.meets ?? "";
        const now = pinned?.environment
          ? t`Shares ${meets} in ${project}`
          : version
            ? t`New ${engine} ${version} beside ${app}`
            : t`New ${engine} beside ${app}`;
        const elsewhere = (options ?? []).filter(
          (o) =>
            o.need === need.name &&
            o.service !== "" &&
            o.why === "" &&
            !(pinned?.environment && o.project === project && o.service === meets),
        );
        return (
          <li key={need.name} className="catalog-need-line">
            <span className="catalog-need-line-text">
              <span className="catalog-need-line-name">{need.label}</span>
              <span className="catalog-need-line-now">{now}</span>
            </span>
            {sqlEngines.includes(engine) ? (
              <Button
                variant="ghost"
                size="sm"
                aria-expanded={backups === need.name}
                onClick={() => setBackups(backups === need.name ? undefined : need.name)}
              >
                <Trans>Its backups</Trans>
              </Button>
            ) : null}
            <Button
              variant="ghost"
              size="sm"
              aria-expanded={open === need.name}
              onClick={() => setOpen(open === need.name ? undefined : need.name)}
              {...(elsewhere.length === 0
                ? { disabled: true, title: t`Nothing else Dockwell runs takes it` }
                : may.off)}
            >
              <Trans>Use another {what}</Trans>
            </Button>
            {open === need.name ? (
              <ul className="catalog-need-line-ways">
                {elsewhere.map((o) => {
                  const server = o.service;
                  const where = o.project;
                  // Its data comes along where both run the same engine; otherwise starting
                  // empty is a choice of its own, never a default (ADR 0084).
                  const copies = Number(o.engine) === Number(pinned?.engine ?? 0);
                  return (
                    <li key={o.choice}>
                      <span className="catalog-need-line-way">
                        <span>
                          <Trans>
                            {server} in {where}
                          </Trans>
                        </span>
                        <span className="catalog-need-line-now">
                          {copies ? <Trans>Copies its data</Trans> : <Trans>Its data can't be copied there</Trans>}
                        </span>
                      </span>
                      <code>{`${engineName(o.engine)} ${o.version}`.trim()}</code>
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() => use(need.name, o.choice, !copies)}
                        {...may.off}
                      >
                        {copies ? <Trans>Use it</Trans> : <Trans>Start empty on it</Trans>}
                      </Button>
                    </li>
                  );
                })}
              </ul>
            ) : null}
            {backups === need.name ? <NeedBackups install={install} need={need.name} /> : null}
          </li>
        );
      })}
    </ul>
  );
}

/** The engines whose one database an app backs up and restores alone (ADR 0084). */
const sqlEngines = ["PostgreSQL", "MySQL", "MariaDB"];
