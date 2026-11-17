// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import { CatalogInstallSchema, CatalogService, type ServiceInfo } from "@dockwell/api";
import { Button, Panel } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { environmentOf } from "../apps/app";
import { useUpgrade } from "../apps/upgrade";
import { useServices } from "../apps/useServices";
import { SignInCard, useInstallFacts } from "./access";
import { type CardRow, CardRows } from "./CardRows";
import { useCompanionRows } from "./Companions";
import { HeldRoutes } from "./HeldRoutes";
import { useNeedRows } from "./NeedLines";
import { OwnerCallout, OwnerCard, ownerStatus } from "./Owner";
import { installIn, useInstalls, useUpgradeUnavailable } from "./useCatalog";
import "./Catalog.css";

/**
 * What the catalog says of an app it installed, on each of its services' pages, as
 * rows (#722): what it is and the way to its next version, how to reach it, what it
 * needs and the tools beside it, each with its one action; its notes one step away.
 * How to sign in is a card of its own, on the page of what a person opens (#426).
 */
export function InstalledPanel({ service }: { service: ServiceInfo }) {
  const { t } = useLingui();
  const environment = environmentOf(service);
  const install = installIn(useInstalls().data?.installs ?? [], environment);
  const id = install?.pin?.entry ?? "";
  const entry = useQuery(CatalogService.method.getCatalogEntry, { id }, { enabled: id !== "" }).data?.entry;
  const web = useServices().data?.find((s) => s.id === install?.webServiceId);
  const facts = useInstallFacts(entry, install, web);
  const upgrading = useUpgrade(service);
  const unavailableOf = useUpgradeUnavailable();
  const [notesOpen, setNotesOpen] = useState(false);
  const needRows = useNeedRows(entry, install ?? EMPTY_INSTALL);
  const companionRows = useCompanionRows(entry, install ?? EMPTY_INSTALL);
  if (!install) return null;
  // How to sign in is to what the entry opens, which a companion left out isn't (ADR 0076).
  const opens = entry?.web?.service ?? "";
  const webRuns = !entry?.companions.some((c) => c.service === opens) || install.companions[opens] === true;
  const name = install.name || install.pin?.entry || "";
  const version = install.pin?.version ?? "";
  const next = install.upgrade?.version ?? "";
  // A release that can't be planned where it runs, as one that drops its server's
  // architecture, says why, with nothing to take (#771).
  const unavailable = unavailableOf(install);
  // Its catalog withdrew the version it runs, which it keeps, as moving down could break its data (ADR 0080).
  const lower = install.withdrawn?.version ?? "";
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const mayUpgrade = upgrading.may;
  // One flow for every update: the install's, with its backup first, which a switch leaves out (ADR 0080).
  const upgrade = () => upgrading.open();
  const notes = facts.filter((f) => !f.label);
  const rows: CardRow[] = [
    {
      key: "installed",
      label: <Trans context="catalog card">Installed</Trans>,
      value: install.upgrade ? (
        <>
          <code>
            {name} {version}
          </code>{" "}
          <span className="card-row-quiet">
            {next === version ? <Trans>· a change to how it runs is out</Trans> : <Trans>· {next} is out</Trans>}
          </span>
          {unavailable ? <span className="card-row-quiet"> · {unavailable}</span> : null}
        </>
      ) : lower ? (
        <>
          <code>
            {name} {version}
          </code>{" "}
          <span className="card-row-quiet">
            <Trans>· withdrawn by its catalog, so it stays on {version} until a later release</Trans>
          </span>
        </>
      ) : (
        <code>
          {name} {version}
        </code>
      ),
      action: unavailable ? undefined : install.upgrade ? (
        <Button variant="primary" size="sm" onClick={upgrade} {...mayUpgrade.off}>
          {next === version ? <Trans>Review the change</Trans> : <Trans>Upgrade to {next}</Trans>}
        </Button>
      ) : lower ? (
        <RouterButtonLink to="/catalog/$entryId" params={{ entryId: id }} variant="ghost" size="sm">
          <Trans>Install {lower} beside it</Trans>
        </RouterButtonLink>
      ) : undefined,
    },
    ...facts.flatMap((f) => (f.label ? [{ key: f.key, label: f.label, value: f.text, action: f.action }] : [])),
    ...needRows,
    ...companionRows,
  ];
  if (notes.length > 0) {
    rows.push({
      key: "notes",
      label: <Trans>Its notes</Trans>,
      value: plural(notes.length, { one: "# note from its catalog", other: "# notes from its catalog" }),
      action: (
        <Button variant="ghost" size="sm" aria-expanded={notesOpen} onClick={() => setNotesOpen(!notesOpen)}>
          {notesOpen ? <Trans>Hide</Trans> : <Trans>Read</Trans>}
        </Button>
      ),
      under: notesOpen ? (
        <ul className="card-row-notes">
          {notes.map((n) => (
            <li key={n.key}>{n.text}</li>
          ))}
        </ul>
      ) : undefined,
    });
  }
  return (
    <>
      <Panel
        title={t`From the catalog`}
        actions={
          <RouterButtonLink to="/variables" search={{ environment }} variant="ghost" size="sm">
            <Trans>Its secrets</Trans>
          </RouterButtonLink>
        }
      >
        {/* An install whose owner Dockwell makes, or that's held until a person sets it up, says
            which (ADR 0105); a hold of its own routes alone, as a new empty database's, is ADR 0084's. */}
        {ownerStatus(install) && ownerStatus(install) !== "made" ? (
          <OwnerCallout service={service} install={install} entry={entry} />
        ) : (
          <HeldRoutes service={service} install={install} />
        )}
        <CardRows rows={rows} label={t`From the catalog`} />
      </Panel>
      {entry?.signIn && service.id === install.webServiceId && webRuns ? (
        <SignInCard entry={entry} install={install} />
      ) : null}
      {entry?.owner && service.id === install.webServiceId ? <OwnerCard entry={entry} install={install} /> : null}
    </>
  );
}

/** What the hooks read where the app is no install, so they run on every page alike. */
const EMPTY_INSTALL = create(CatalogInstallSchema, {});
