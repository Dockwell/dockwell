// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { CatalogService, type ServiceInfo } from "@dockwell/api";
import { Button, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { RouterButtonLink } from "../../lib/links";
import { environmentOf } from "../apps/app";
import { useChangeSheet } from "../changes/ChangeSheet";
import { installIn, useInstalls } from "./useCatalog";
import "./Catalog.css";

/**
 * What the catalog says of an app it installed, on each of its services' pages: which
 * version, what to do first and the way to the secrets that takes, and when a newer
 * version is out, the way to its plan.
 */
export function InstalledPanel({ service }: { service: ServiceInfo }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const environment = environmentOf(service);
  const install = installIn(useInstalls().data?.installs ?? [], environment);
  if (!install) return null;
  const name = install.name || install.pin?.entry || "";
  const version = install.pin?.version ?? "";
  const next = install.upgrade?.version ?? "";
  const upgrade = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(CatalogService, transport).upgradeCatalogInstall({ id: install.projectId }),
    });
  return (
    <Panel
      title={t`From the catalog`}
      actions={
        <RouterButtonLink to="/variables" search={{ environment }} variant="secondary" size="sm">
          <Trans>Show its secrets</Trans>
        </RouterButtonLink>
      }
    >
      <p className="catalog-installed-line">
        <Trans>
          {name} {version}
        </Trans>
      </p>
      {install.firstSteps ? <p className="catalog-installed-steps">{install.firstSteps}</p> : null}
      {install.upgrade ? (
        <div className="catalog-upgrade">
          <p className="catalog-upgrade-line">
            {next === version ? (
              <Trans>A change to how {version} runs is out.</Trans>
            ) : (
              <Trans>Version {next} is out.</Trans>
            )}
          </p>
          <Button variant="primary" size="sm" onClick={upgrade}>
            {next === version ? <Trans>Review the change</Trans> : <Trans>Upgrade to {next}</Trans>}
          </Button>
        </div>
      ) : null}
    </Panel>
  );
}
