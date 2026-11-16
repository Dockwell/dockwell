// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type CatalogInstall, CatalogService, type Entry } from "@dockwell/api";
import { Button, Glyph, Panel, STATUS, TONE } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { type CardRow, CardRows } from "./CardRows";
import { useCatalog } from "./useCatalog";
import "./Catalog.css";

/**
 * What to call a service of an install: its companion's label, such as Adminer or
 * phpMyAdmin, or else its own name, as a header names what's beside a database.
 */
export function useServiceLabel(install: CatalogInstall | undefined): (service: string) => string {
  const id = install?.pin?.entry ?? "";
  const entry = useCatalog({ ids: [id] }, { enabled: id !== "" }).data?.entries.find((e) => e.id === id);
  return (service) => entry?.companions.find((c) => c.service === service)?.label ?? service;
}

/**
 * The tools an install may run beside its app, such as Adminer beside PostgreSQL (ADR
 * 0076), as rows: what it is, whether it runs, and the action that changes that, a
 * plan (The interface does the work; one way in for each task).
 */
export function useCompanionRows(entry: Entry | undefined, install: CatalogInstall): CardRow[] {
  const transport = useTransport();
  const sheet = useChangeSheet();
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay()(CatalogService.method.setCatalogCompanion, { project: install.projectId });
  if (!entry) return [];
  const set = (service: string, on: boolean) =>
    sheet.open({
      kind: "plan",
      make: () => createClient(CatalogService, transport).setCatalogCompanion({ id: install.projectId, service, on }),
    });
  return entry.companions.map((c) => {
    const on = install.companions[c.service] ?? false;
    const label = c.label;
    return {
      key: `companion-${c.service}`,
      label,
      value: (
        <span className="catalog-companion-text">
          <span className={on ? "catalog-companion-state is-on" : "catalog-companion-state"}>
            <Glyph shape={on ? TONE.healthy.shape : STATUS.stopped.shape} size={9} />
            {on ? <Trans context="companion">Installed</Trans> : <Trans context="companion">Not installed</Trans>}
          </span>
          <span className="catalog-companion-purpose">{c.purpose}</span>
        </span>
      ),
      action: (
        <Button variant={on ? "ghost" : "secondary"} size="sm" onClick={() => set(c.service, !on)} {...may.off}>
          {on ? <Trans>Remove {label}</Trans> : <Trans>Add {label}</Trans>}
        </Button>
      ),
    };
  });
}

/** The tools an install may run beside its app, as rows of their own. */
export function CompanionLines({ entry, install }: { entry: Entry; install: CatalogInstall }) {
  const { t } = useLingui();
  return <CardRows rows={useCompanionRows(entry, install)} label={t({ message: "Tools", context: "companions" })} />;
}

/** The tools beside a database, on its page, as a panel of their own. */
export function CompanionsPanel({ install }: { install: CatalogInstall | undefined }) {
  const { t } = useLingui();
  const id = install?.pin?.entry ?? "";
  const entry = useQuery(CatalogService.method.getCatalogEntry, { id }, { enabled: id !== "" }).data?.entry;
  if (!install || !entry || entry.companions.length === 0) return null;
  return (
    <Panel title={t({ message: "Tools", context: "companions" })} className="catalog-companions-panel">
      <CompanionLines entry={entry} install={install} />
    </Panel>
  );
}
