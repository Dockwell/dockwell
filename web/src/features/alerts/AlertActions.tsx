// SPDX-License-Identifier: AGPL-3.0-only

import { type Alert, AlertRule_Condition as Condition, type Domain, type ServiceInfo } from "@dockwell/api";
import { ButtonLink, CopyButton } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { RouterButtonLink } from "../../lib/links";
import { environmentOf, nameOf } from "../apps/app";
import { useAllDomains } from "../apps/domains";
import { useServices } from "../apps/useServices";
import { serviceCommands } from "../servers/commands";
import { useFleet } from "../servers/useFleet";
import { useChecks } from "./checks";

/** The host of an address, or "" for one that isn't a URL. */
function hostOf(address: string): string {
  try {
    return new URL(address).host;
  } catch {
    return "";
  }
}

/** Where an app answers: the address of a domain one of its routes covers, whole. */
function siteOf(service: ServiceInfo | undefined, domains: readonly Domain[]): string {
  if (!service) return "";
  const name = nameOf(service);
  const env = environmentOf(service);
  const domain = domains.find(
    (d) => d.environmentId === env && d.routes.some((r) => r.service === name && (r.path === "" || r.path === "/")),
  );
  return domain?.url ?? "";
}

/** The app a domain's whole-domain route sends to, by the host of an address. */
export function serviceAt(
  address: string,
  domains: readonly Domain[],
  services: readonly ServiceInfo[],
): ServiceInfo | undefined {
  const host = hostOf(address);
  if (host === "") return undefined;
  const domain = domains.find((d) => hostOf(d.url) === host);
  const route = domain?.routes.find((r) => r.path === "" || r.path === "/");
  if (!domain || !route) return undefined;
  return services.find((s) => environmentOf(s) === domain.environmentId && nameOf(s) === route.service);
}

/**
 * What an alert asks someone to do, as buttons rather than a sentence that says what
 * to look at (#794): an app that's down opens its site and its logs, an address that
 * doesn't answer does the same for the app behind it and opens its check, a server
 * that's gone quiet copies the command that starts its agent again, and each of the
 * rest opens where it's put right.
 */
export function AlertActions({ alert, size = "sm" }: { alert: Alert; size?: "sm" | "md" }) {
  const { t } = useLingui();
  const services = useServices().data ?? [];
  const domains = useAllDomains().last?.domains ?? [];
  const checks = useChecks().data?.checks ?? [];
  const fleet = useFleet();
  const servers = fleet.status === "connecting" ? [] : fleet.servers;
  const small = { size } as const;
  const open = (href: string) =>
    href ? (
      <ButtonLink key="site" href={href} target="_blank" rel="noreferrer" icon="external" size={size}>
        <Trans>Open the site</Trans>
      </ButtonLink>
    ) : null;
  const logs = (service: ServiceInfo | undefined) =>
    service ? (
      <RouterButtonLink key="logs" to="/apps/$serviceId/logs" params={{ serviceId: service.id }} icon="file" {...small}>
        <Trans>Its logs</Trans>
      </RouterButtonLink>
    ) : null;
  const server = alert.serverId ? (
    <RouterButtonLink
      key="server"
      to="/servers/$serverId"
      params={{ serverId: alert.serverId }}
      icon="server"
      {...small}
    >
      <Trans>Open the server</Trans>
    </RouterButtonLink>
  ) : null;
  const to = (key: string, path: "/domains" | "/backups" | "/catalog/sources" | "/alerts/checks", words: string) => (
    <RouterButtonLink key={key} to={path} icon="arrow-right" {...small}>
      {words}
    </RouterButtonLink>
  );

  let actions: (ReactNode | null)[] = [];
  switch (alert.condition) {
    case Condition.SERVICE_DOWN: {
      const app = services.find((s) => s.id === alert.subjectId);
      actions = [open(siteOf(app, domains)), logs(app)];
      if (!app && alert.subjectId)
        actions.push(
          <RouterButtonLink key="app" to="/apps/$serviceId" params={{ serviceId: alert.subjectId }} {...small}>
            <Trans>Open the app</Trans>
          </RouterButtonLink>,
        );
      break;
    }
    case Condition.CHECK_DOWN: {
      const address =
        checks.find((c) => c.id === alert.subjectId)?.spec?.address ?? (alert.where ? `https://${alert.where}/` : "");
      const app = serviceAt(address, domains, services);
      actions = [open(address), logs(app), to("check", "/alerts/checks", t`Its uptime check`)];
      break;
    }
    case Condition.SERVER_UNREACHABLE: {
      const on = servers.find((s) => s.id === alert.serverId);
      const command = on ? serviceCommands(on).restartAgent : "sudo systemctl restart dockwell";
      actions = [
        <CopyButton
          key="copy"
          text={command}
          label={t`Copy the restart command`}
          copiedLabel={t`Restart command copied`}
          size={size}
        />,
        server,
      ];
      break;
    }
    case Condition.CHANGE_FAILED:
      if (alert.subjectId)
        actions = [
          <RouterButtonLink
            key="change"
            to="/changes/$changeId"
            params={{ changeId: alert.subjectId }}
            icon="activity"
            {...small}
          >
            <Trans>Open the change</Trans>
          </RouterButtonLink>,
        ];
      break;
    case Condition.UPDATE_AVAILABLE:
      if (alert.subjectId)
        actions = [
          <RouterButtonLink
            key="app"
            to="/apps/$serviceId"
            params={{ serviceId: alert.subjectId }}
            icon="arrow-up"
            {...small}
          >
            <Trans>Open the app</Trans>
          </RouterButtonLink>,
        ];
      break;
    case Condition.CERTIFICATE_EXPIRING:
    case Condition.CERTIFICATE_FAILED:
    case Condition.DOMAIN_UNPUBLISHED:
      actions = [to("domains", "/domains", t`Open Domains`)];
      break;
    case Condition.BACKUP_FAILED:
    case Condition.BACKUP_LATE:
    case Condition.RESTORE_DRILL_FAILED:
    case Condition.RESTORE_DRILL_LATE:
      actions = [to("backups", "/backups", t`Open Backups`)];
      break;
    case Condition.CATALOG_SOURCE_STUCK:
      actions = [to("sources", "/catalog/sources", t`Open Catalog sources`)];
      break;
    default:
      actions = [server];
  }
  const shown = actions.filter((a) => a !== null && a !== undefined);
  if (shown.length === 0 && alert.condition !== Condition.SERVER_UNREACHABLE && server) shown.push(server);
  return shown.length > 0 ? <span className="alerts-actions">{shown}</span> : null;
}
