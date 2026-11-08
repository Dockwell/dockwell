// SPDX-License-Identifier: AGPL-3.0-only

import { Button, Sheet } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { CopyCommand } from "../system/CopyCommand";
import "./ServersPage.css";

/** What a new server runs to join, with the ticket in its place. */
export const JOIN_COMMAND = "curl -fsSL https://get.dockwell.dev | sh -s -- --join <ticket>";

/**
 * How to add a server today: a ticket from the control server, then one command on
 * the new one. Add-Server's ticket made here, and its wait for the server to
 * connect, need an API that makes tickets (#325).
 */
export function AddServerSheet({ control, onClose }: { control: string | undefined; onClose: () => void }) {
  const { t } = useLingui();
  return (
    <Sheet
      open
      title={t`Add a server`}
      subtitle={t`It joins in seconds, and nothing on your servers changes.`}
      onClose={onClose}
      className="servers-add"
      footer={
        <Button variant="primary" onClick={onClose}>
          <Trans>Done</Trans>
        </Button>
      }
    >
      <ol className="servers-add-steps">
        <li>
          <h3>
            {control ? <Trans>Make a ticket on {control}</Trans> : <Trans>Make a ticket on the control server</Trans>}
          </h3>
          <p>
            <Trans>
              Over SSH, as the user that runs Dockwell. It works once, for an hour, and needs sudo when Dockwell runs as
              the system service.
            </Trans>
          </p>
          <CopyCommand command="sudo dockwell token create" />
        </li>
        <li>
          <h3>
            <Trans>Run one command on the new server</Trans>
          </h3>
          <p>
            <Trans>
              As root, on any Linux server: another provider, a home lab or a VM. Put the ticket in place of{" "}
              <code>{"<ticket>"}</code>. It installs the agent, joins, and starts.
            </Trans>
          </p>
          <CopyCommand command={JOIN_COMMAND} />
        </li>
        <li>
          <h3>
            <Trans>Watch it appear</Trans>
          </h3>
          <p>
            <Trans>
              It shows on Servers as soon as its agent connects. Your apps stay where they are: an app moves only when
              you apply a plan for it.
            </Trans>
          </p>
        </li>
      </ol>
    </Sheet>
  );
}
