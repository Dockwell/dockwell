// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { CatalogService, type ServiceInfo } from "@dockwell/api";
import { Button, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { RouterButtonLink } from "../../lib/links";
import { environmentOf } from "../apps/app";
import { useServices } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { Facts, SignInCard, useInstallFacts } from "./access";
import { installIn, useInstalls } from "./useCatalog";
import "./Catalog.css";
import { useMay } from "../auth/may";

/**
 * What the catalog says of an app it installed, on each of its services' pages: which
 * version, what it says of itself as state with the actions that resolve it, how to
 * sign in on the page of what a person opens, and when a newer version is out, the
 * way to its plan (#426).
 */
export function InstalledPanel({ service }: { service: ServiceInfo }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const environment = environmentOf(service);
  const may = useMay();
  const install = installIn(useInstalls().data?.installs ?? [], environment);
  const id = install?.pin?.entry ?? "";
  const entry = useQuery(CatalogService.method.getCatalogEntry, { id }, { enabled: id !== "" }).data?.entry;
  const web = useServices().data?.find((s) => s.id === install?.webServiceId);
  const facts = useInstallFacts(entry, install, web);
  if (!install) return null;
  const name = install.name || install.pin?.entry || "";
  const version = install.pin?.version ?? "";
  const next = install.upgrade?.version ?? "";
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const mayUpgrade = may(CatalogService.method.upgradeCatalogInstall, { project: install.projectId });
  const upgrade = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(CatalogService, transport).upgradeCatalogInstall({ id: install.projectId }),
    });
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
        {install.upgrade ? (
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
      {entry?.signIn && service.id === install.webServiceId ? <SignInCard entry={entry} install={install} /> : null}
    </>
  );
}
