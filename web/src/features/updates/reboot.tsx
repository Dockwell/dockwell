// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, type Server, UpdateService } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useServerUpdates } from "./updates";

/**
 * Reboot, one step from where a server says it needs one (ADR 0064): the plan of
 * a reboot in the sheet, which lists what goes offline and, on the control server,
 * that the panel stops with it, then the passkey.
 */
export function RebootButton({ server, variant = "primary" }: { server: Server; variant?: "primary" | "secondary" }) {
  const transport = useTransport();
  const sheet = useChangeSheet();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const may = useMay()(ChangeService.method.planReboot, { install: true });
  const [planning, setPlanning] = useState(false);
  const plan = () => {
    setPlanning(true);
    sheet.open({
      kind: "plan",
      make: async () => {
        try {
          return await client.planReboot({ serverIds: [server.id] });
        } finally {
          setPlanning(false);
        }
      },
    });
  };
  return (
    <Button size="sm" variant={variant} icon="refresh" loading={planning} onClick={plan} {...may.off}>
      <Trans>Reboot</Trans>
    </Button>
  );
}

/**
 * On a server's page, that it needs a reboot for what was installed to take effect,
 * with Reboot beside it, as its Updates tab has. Read only for someone who may read
 * its updates (ADR 0059), and said only once a read says so.
 */
export function RebootCallout({ server }: { server: Server }) {
  const { t } = useLingui();
  const ask = useMay()(UpdateService.method.getServerUpdates).allowed;
  const updates = useServerUpdates(server.id, { ask }).data?.updates;
  if (!updates?.rebootRequired || updates.reason) return null;
  const name = server.name;
  return (
    <Callout tone="warning" title={t`${name} needs a reboot`} actions={<RebootButton server={server} />}>
      {t`For what was installed to take effect`}
    </Callout>
  );
}
