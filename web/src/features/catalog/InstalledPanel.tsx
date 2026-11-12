// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { CatalogService, type ServiceInfo } from "@dockwell/api";
import { Button, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { RouterButtonLink } from "../../lib/links";
import { environmentOf } from "../apps/app";
import { useUpgrade } from "../apps/upgrade";
import { useServices } from "../apps/useServices";
import { Facts, SignInCard, useInstallFacts } from "./access";
import { CompanionLines } from "./Companions";
import { installIn, useInstalls } from "./useCatalog";
import "./Catalog.css";

/**
 * What the catalog says of an app it installed, on each of its services' pages: which
 * version, what it says of itself as state with the actions that resolve it, how to
 * sign in on the page of what a person opens, and when a newer version is out, the
 * way to its plan (#426).
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
  if (!install) return null;
  // How to sign in is to what the entry opens, which a companion left out isn't (ADR 0076).
  const opens = entry?.web?.service ?? "";
  const webRuns = !entry?.companions.some((c) => c.service === opens) || install.companions[opens] === true;
  const name = install.name || install.pin?.entry || "";
  const version = install.pin?.version ?? "";
  const next = install.upgrade?.version ?? "";
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const mayUpgrade = upgrading.may;
  // One flow for every update: the install's, with its backup first, which a switch leaves out (ADR 0080).
  const upgrade = () => upgrading.open();
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
        <p className="catalog-installed-line">
          <Trans>
            {name} {version}
          </Trans>
        </p>
        <Facts facts={facts} />
        {entry ? <CompanionLines entry={entry} install={install} /> : null}
        {/* Its update is the app's update card, once the control plane says it on the app (ADR 0079). */}
        {install.upgrade && !service.update ? (
          <div className="catalog-upgrade">
            <p className="catalog-upgrade-line">
              {next === version ? (
                <Trans>A change to how {version} runs is out.</Trans>
              ) : (
                <Trans>Version {next} is out.</Trans>
              )}
            </p>
            <Button variant="primary" size="sm" onClick={upgrade} {...mayUpgrade.off}>
              {next === version ? <Trans>Review the change</Trans> : <Trans>Upgrade to {next}</Trans>}
            </Button>
          </div>
        ) : null}
      </Panel>
      {entry?.signIn && service.id === install.webServiceId && webRuns ? (
        <SignInCard entry={entry} install={install} />
      ) : null}
    </>
  );
}
