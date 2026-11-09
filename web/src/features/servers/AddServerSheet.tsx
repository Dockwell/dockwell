// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { UpdateService } from "@dockwell/api";
import { Button, Sheet } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { shortVersion } from "../shell/version";
import { CopyCommand } from "../system/CopyCommand";
import { installCommand, isRelease } from "../updates/updates";
import "./ServersPage.css";

/**
 * What a new server runs to join, with the ticket in its place: the install script of
 * the control plane's own release, so its agent is that version, from where the
 * control plane reads releases, as the Updates tab says to update an agent. Undefined
 * where either isn't known, as for a build that isn't a release.
 */
export function joinCommand(releases: string, controlPlane: string): string | undefined {
  // A build on the way to a release, as the rail reads it, has no install script of its own to fetch.
  if (shortVersion(controlPlane).endsWith("dev")) return undefined;
  const install = installCommand(releases, controlPlane);
  return install ? `${install} --join <ticket>` : undefined;
}

/**
 * How to add a server today: a ticket from the control server, then one command on
 * the new one. Add-Server's ticket made here, and its wait for the server to
 * connect, need an API that makes tickets (#325).
 */
export function AddServerSheet({
  control,
  onClose,
}: {
  /** The control server, whose release the new one's agent takes. */
  control: { id: string; name: string } | undefined;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const updates = useQuery(
    UpdateService.method.getServerUpdates,
    { id: control?.id ?? "" },
    { enabled: control !== undefined, retry: false },
  );
  const release = updates.data?.dockwell;
  const version = release?.controlPlane ?? "";
  const command = joinCommand(release?.releases ?? "", release?.controlPlane ?? "");
  const development = version !== "" && (shortVersion(version).endsWith("dev") || !isRelease(version));
  const name = control?.name;
  return (
    <Sheet
      open
      title={t`Add a server`}
      subtitle={t`Connect a new server with a ticket from the control server.`}
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
          <h3>{name ? <Trans>Make a ticket on {name}</Trans> : <Trans>Make a ticket on the control server</Trans>}</h3>
          <p>
            <Trans>
              Over SSH, as the user that runs Dockwell, with <code>sudo</code> in front when it runs as the system
              service. The ticket works once, for an hour.
            </Trans>
          </p>
          <CopyCommand command="dockwell token create" />
        </li>
        <li>
          <h3>
            {command ? <Trans>Run one command on the new server</Trans> : <Trans>Join and start the agent</Trans>}
          </h3>
          {command ? (
            <>
              <p>
                <Trans>
                  On any Linux server: another provider, a home lab or a VM. Put the ticket in place of{" "}
                  <code>{"<ticket>"}</code>. It installs the control plane's version of the agent, joins, and starts.
                </Trans>
              </p>
              <CopyCommand command={command} />
            </>
          ) : control && updates.isPending ? (
            <p>
              <Trans>Reading the control server's installation details.</Trans>
            </p>
          ) : (
            <>
              <p>
                {development ? (
                  <Trans>
                    This control server runs a development build with no release installer. Build the agent from the
                    same source revision for the new server's architecture and install it as <code>dockwell</code>.
                  </Trans>
                ) : (
                  <Trans>
                    The control server's installation details are unavailable. Install an agent that matches its version
                    as <code>dockwell</code> before joining.
                  </Trans>
                )}
              </p>
              <p>
                <Trans>
                  On the new server, put the ticket in place of <code>{"<ticket>"}</code>. Run both commands as the same
                  user, with <code>sudo</code> for both when using the system service. Keep <code>dockwell run</code>{" "}
                  running, or configure it as a service.
                </Trans>
              </p>
              <CopyCommand command="dockwell join <ticket>" />
              <CopyCommand command="dockwell run" />
            </>
          )}
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
