// SPDX-License-Identifier: AGPL-3.0-only

import { type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { environmentOf } from "./app";
import { CIDeploySheet } from "./CIDeploySheet";

export function BuildMemoryNotice({
  available,
  serverId,
  service,
  onLeave,
}: {
  available?: bigint | undefined;
  serverId: string;
  service?: ServiceInfo | undefined;
  onLeave?: (() => void) | undefined;
}) {
  const { t, i18n } = useLingui();
  const [setup, setSetup] = useState(false);
  const mayDeploy = useMay()(ServiceService.method.deployService, {
    environment: service ? environmentOf(service) : "",
  });
  const free = available?.toLocaleString(i18n.locale);
  return (
    <>
      <Callout
        tone="critical"
        title={
          free === undefined
            ? t`This server had less than the 256 MiB a build needs.`
            : t`This server has ${free} MiB free and a build needs 256 MiB.`
        }
        actions={
          <>
            {service ? (
              <Button size="sm" icon="commit" onClick={() => setSetup(true)} {...mayDeploy.off}>
                <Trans>Deploy from CI</Trans>
              </Button>
            ) : null}
            {serverId ? (
              <RouterButtonLink
                size="sm"
                variant="ghost"
                to="/servers/$serverId"
                params={{ serverId }}
                hash="srv-hist-title"
                onClick={onLeave}
              >
                <Trans>See what uses memory</Trans>
              </RouterButtonLink>
            ) : null}
          </>
        }
      >
        <Trans>The build didn't start.</Trans>
      </Callout>
      {setup && service ? <CIDeploySheet service={service} onClose={() => setSetup(false)} /> : null}
    </>
  );
}
