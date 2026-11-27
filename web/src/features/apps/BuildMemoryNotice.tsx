// SPDX-License-Identifier: AGPL-3.0-only

import { type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { FailureNotice } from "../changes/FailureNotice";
import { environmentOf } from "./app";
import { CIDeploySheet } from "./CIDeploySheet";

export function isBuildMemoryFailure(reason: string | undefined): boolean {
  return reason === "build_memory" || reason === "build_killed";
}

export function BuildMemoryNotice({
  reason,
  available,
  limit,
  serverId,
  service,
  onLeave,
  fallback,
}: {
  reason: string;
  available?: bigint | undefined;
  limit?: bigint | undefined;
  serverId: string;
  service?: ServiceInfo | undefined;
  onLeave?: (() => void) | undefined;
  fallback?: { message: string; detail?: string | undefined } | undefined;
}) {
  const { t, i18n } = useLingui();
  const [setup, setSetup] = useState(false);
  const mayDeploy = useMay()(ServiceService.method.deployService, {
    environment: service ? environmentOf(service) : "",
  });
  const free = available?.toLocaleString(i18n.locale);
  const killed = reason === "build_killed";
  const cap = limit === undefined || limit === 0n ? undefined : limit.toLocaleString(i18n.locale);
  const title = killed
    ? t`The build was killed`
    : limit !== undefined
      ? t`The kernel stopped the build when memory ran out.`
      : free === undefined
        ? t`This server had less than the 256 MiB a build needs.`
        : t`This server had ${free} MiB free and a build needs 256 MiB.`;
  const actions = (
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
          to="/servers/$serverId/processes"
          params={{ serverId }}
          search={{ sort: "-memory" }}
          onClick={onLeave}
        >
          <Trans>See what uses memory</Trans>
        </RouterButtonLink>
      ) : null}
    </>
  );
  return (
    <>
      {fallback && limit === undefined && free === undefined && !killed ? (
        <FailureNotice {...fallback} actions={actions} />
      ) : (
        <Callout tone="critical" title={title} actions={actions}>
          {cap !== undefined ? (
            <Trans>The build could use up to {cap} MiB. Nothing was deployed.</Trans>
          ) : killed || limit !== undefined ? (
            <Trans>Nothing was deployed.</Trans>
          ) : (
            <Trans>The build didn't start.</Trans>
          )}
        </Callout>
      )}
      {setup && service ? <CIDeploySheet service={service} onClose={() => setSetup(false)} /> : null}
    </>
  );
}
