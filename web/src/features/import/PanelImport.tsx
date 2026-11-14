// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  ImportAction,
  ImportKind,
  ImportService,
  type ImportStep,
  type Panel as PanelFound,
  PanelKind,
  type Server,
} from "@dockwell/api";
import { Button, Callout, Panel } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ServerFrame } from "../servers/ServerFrame";
import { OnePanelImport, StepGroup, useGroups } from "./OnePanelImport";
import { useOnePanelHere } from "./onePanelHere";
import { usePanelRunning } from "./StopPanel";
import "./OnePanelImport.css";

/*
 * Moving in from Coolify or Dokploy (docs/adr/0089-importing-from-coolify-and-dokploy.md).
 * The page reads what the panel runs, from Docker and its folders, when a person
 * asks, and plans nothing: Review the plan has the sheet make it. Its domains come
 * over as a step of their own, which stops its proxy, and the panel stops as one
 * more. Nothing on the server changes until a plan is applied.
 */

/** A server's Move in page: each panel that runs there, 1Panel's as before. */
export function MoveInPage({ serverId }: { serverId: string }) {
  return <ServerFrame id={serverId}>{(server) => <MoveIn server={server} />}</ServerFrame>;
}

function MoveIn({ server }: { server: Server }) {
  const onePanel = useOnePanelHere(server);
  const kind = usePanelRunning(server);
  return (
    <>
      {kind !== undefined ? <PanelImport server={server} kind={kind} /> : null}
      {onePanel || kind === undefined ? <OnePanelImport server={server} /> : null}
    </>
  );
}

function PanelImport({ server, kind }: { server: Server; kind: PanelKind }) {
  const { t } = useLingui();
  const transport = useTransport();
  const imports = useMemo(() => createClient(ImportService, transport), [transport]);
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const name = kind === PanelKind.COOLIFY ? "Coolify" : "Dokploy";
  const [reading, setReading] = useState(false);
  const [failure, setFailure] = useState<unknown>();
  const [found, setFound] = useState<{ found?: PanelFound; steps: ImportStep[] }>();

  const importIt = (skipPlan: boolean) =>
    kind === PanelKind.COOLIFY
      ? imports.importCoolify({ serverId: server.id, skipPlan })
      : imports.importDokploy({ serverId: server.id, skipPlan });

  // A read plans nothing: what's found, and what an import would do with it.
  const read = async () => {
    setReading(true);
    setFailure(undefined);
    try {
      setFound(await importIt(true));
    } catch (err) {
      setFound(undefined);
      setFailure(err);
    } finally {
      setReading(false);
    }
  };

  // Each plan is the sheet's to make, and to discard. What it changed is read again
  // once it closes, as the steps it brought over are done.
  const again = () => void read();
  const review = () => {
    sheet.open({
      kind: "plan",
      make: async () => {
        const res = await importIt(false);
        setFound(res);
        return { change: res.change };
      },
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED) void navigate({ to: "/apps" });
      },
    });
  };
  const handOver = () => {
    sheet.open({
      kind: "plan",
      make: async () => ({ change: (await imports.planPanelDomains({ serverId: server.id, panel: kind })).change }),
      onClose: again,
    });
  };
  const stop = () => {
    sheet.open({
      kind: "plan",
      make: async () => ({ change: (await imports.planStopPanel({ serverId: server.id, panel: kind })).change }),
      onClose: again,
    });
  };

  const steps = found?.steps ?? [];
  const groups = useGroups(steps, name);
  const toTake = steps.some((s) => s.action === ImportAction.TAKE_ON);
  const panel = found?.found;
  const notHere = reasonOf(failure) === "panel_not_found";
  const reviewButton = (
    <Button variant="primary" icon="arrow-right" onClick={review}>
      <Trans>Review the plan</Trans>
    </Button>
  );
  return (
    <div className="onepanel">
      <Panel title={t`Move in from ${name}`}>
        <div className="onepanel-intro">
          <p>
            <Trans>
              Dockwell reads what {name} runs from Docker and its folders, read-only. Nothing changes until you apply a
              plan.
            </Trans>
          </p>
          <div className="onepanel-actions">
            <Button
              variant={found ? "secondary" : "primary"}
              icon="search"
              loading={reading}
              onClick={() => void read()}
            >
              {found ? <Trans>Read it again</Trans> : <Trans>Read what {name} runs</Trans>}
            </Button>
            {toTake ? reviewButton : null}
          </div>
        </div>
      </Panel>
      {failure && notHere ? (
        <Callout tone="info" title={t`${name} isn't on this server`}>
          {sentence(failure)}
        </Callout>
      ) : failure ? (
        <Callout tone="critical" title={t`Couldn't read what ${name} runs`}>
          {sentence(failure)}
        </Callout>
      ) : null}
      {panel && !panel.knownVersion ? (
        <Callout tone="warning" title={t`A version of ${name} this wasn't written from`}>
          <Trans>It was read by its labels and folders all the same. Check what it found before you apply.</Trans>
        </Callout>
      ) : null}
      {groups.map((g) => (
        <StepGroup
          key={g.key}
          group={g}
          panel={name}
          actions={
            g.key === "ports" ? (
              <Button size="sm" icon="globe" onClick={handOver}>
                <Trans>Hand over ports 80 and 443</Trans>
              </Button>
            ) : undefined
          }
          extra={(step) =>
            step.kind === ImportKind.PANEL ? (
              <Button size="sm" icon="stop" onClick={stop}>
                <Trans>Stop {name}</Trans>
              </Button>
            ) : null
          }
        />
      ))}
      {found && steps.length > 0 ? (
        <div className="onepanel-end">
          <p className="onepanel-count">{plural(steps.length, { one: "# thing found", other: "# things found" })}</p>
          {toTake ? reviewButton : null}
        </div>
      ) : null}
    </div>
  );
}
