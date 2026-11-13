// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, type ServiceInfo } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { environmentOf } from "../apps/app";
import { useAppDomains } from "../apps/domains";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";

/**
 * An app's routes held while it starts on an empty database and its first visitor
 * makes its owner: what they answer, as a state, and the action that opens them, a
 * plan that sets each route as it is but held (ADR 0084).
 */
export function HeldRoutes({ service }: { service: ServiceInfo }) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const environment = environmentOf(service);
  const may = useMay()(ChangeService.method.proposeChange, { environment });
  const held = useAppDomains(service).domains.flatMap((d) =>
    d.routes.filter((r) => r.held).map((route) => ({ domain: d.name, route })),
  );
  if (held.length === 0) return null;
  const domains = [...new Set(held.map((h) => h.domain))].join(", ");
  const open = () =>
    sheet.open({
      kind: "propose",
      summary: t`Open ${domains} to everyone`,
      edits: held.map(({ domain, route }) => ({
        resource: { kind: "route", scope: environment, name: route.name },
        spec: {
          kind: {
            case: "route" as const,
            value: {
              domain,
              path: route.path,
              service: route.service,
              tls: route.tls,
              trustedProxies: route.trustedProxies,
              plainForAnyone: route.plainForAnyone,
              ...(route.hostPort ? { hostPort: route.hostPort } : { port: route.port }),
            },
          },
        },
      })),
    });
  return (
    <Callout tone="warning" title={t`Not open yet`}>
      <Trans>{domains} answers that it isn't open, so its owner sets it up first, through the panel.</Trans>{" "}
      <Button variant="secondary" size="sm" onClick={open} {...may.off}>
        <Trans>Open it to everyone</Trans>
      </Button>
    </Callout>
  );
}
