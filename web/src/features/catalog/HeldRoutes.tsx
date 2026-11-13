// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type CatalogInstall, CatalogService, type ServiceInfo } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { appOf } from "../apps/app";
import { useAppDomains } from "../apps/domains";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";

/**
 * An app held while it starts on an empty database and its first visitor makes its
 * owner: its domains answer that it isn't open, and the ports it publishes listen on
 * its server alone. What that means, as a state, and the action that opens it, a plan
 * the server makes from what runs, so nothing else of the app changes (ADR 0084).
 */
export function HeldRoutes({ service, install }: { service: ServiceInfo; install: CatalogInstall }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay()(CatalogService.method.openCatalogInstall, { project: install.projectId });
  const domains = [
    ...new Set(useAppDomains(service).domains.flatMap((d) => (d.routes.some((r) => r.held) ? [d.name] : []))),
  ];
  const ports = (appOf(service)?.ports ?? []).filter((p) => p.held).map((p) => String(p.hostPort));
  if (domains.length === 0 && ports.length === 0) return null;
  const open = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(CatalogService, transport).openCatalogInstall({ id: install.projectId }),
    });
  const where = domains.join(", ");
  const at = ports.join(", ");
  return (
    <Callout tone="warning" title={t`Not open yet`}>
      {domains.length > 0 && ports.length > 0 ? (
        <Trans>
          {where} answers that it isn't open, and port {at} listens on its server alone, so its owner sets it up first,
          through the panel.
        </Trans>
      ) : domains.length > 0 ? (
        <Trans>{where} answers that it isn't open, so its owner sets it up first, through the panel.</Trans>
      ) : (
        <Trans>Port {at} listens on its server alone, so its owner sets it up first, through the panel.</Trans>
      )}{" "}
      <Button variant="secondary" size="sm" onClick={open} {...may.off}>
        <Trans>Open it to everyone</Trans>
      </Button>
    </Callout>
  );
}
