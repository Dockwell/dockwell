// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  ContainerService,
  ImportAction,
  ImportKind,
  type ImportOnePanelResponse,
  ImportService,
  type ImportStep,
  type Server,
} from "@dockwell/api";
import { Button, Callout, Icon, type IconName, Panel, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ServerFrame } from "../servers/ServerFrame";
import "./OnePanelImport.css";

/*
 * Moving a server off 1Panel (docs/adr/0056-importing-a-1panel-server.md). The page
 * reads what 1Panel keeps when a person asks, as that read plans the change that
 * takes on what runs: a person reviews it in the plan sheet, which applies it or
 * discards it. Nothing on the server changes until it's applied.
 */

/** Whether a container is one 1Panel made: it names each after itself. */
function from1Panel(name: string): boolean {
  return name.startsWith("1Panel-");
}

/** On a server's Overview, where Docker runs what 1Panel made: a way to move it in. */
export function OnePanelCallout({ server }: { server: Server }) {
  const { t } = useLingui();
  // The same read as the containers panel's, which the query cache shares.
  const list = useQuery(ContainerService.method.listContainers, { server: server.id }, { retry: false });
  if (!list.data?.containers.some((c) => from1Panel(c.name))) return null;
  return (
    <Callout
      tone="info"
      title={t`1Panel runs on this server`}
      actions={
        <RouterButtonLink size="sm" to="/servers/$serverId/import" params={{ serverId: server.id }} icon="arrow-right">
          <Trans>Move in from 1Panel</Trans>
        </RouterButtonLink>
      }
    >
      <Trans>
        Dockwell can take on what it runs as it is, with no restart, and say what it would make again and what it would
        leave.
      </Trans>
    </Callout>
  );
}

/** A server's page for moving in from 1Panel. */
export function OnePanelImportPage({ serverId }: { serverId: string }) {
  return <ServerFrame id={serverId}>{(server) => <OnePanelImport server={server} />}</ServerFrame>;
}

function OnePanelImport({ server }: { server: Server }) {
  const { t } = useLingui();
  const transport = useTransport();
  const imports = useMemo(() => createClient(ImportService, transport), [transport]);
  const changes = useMemo(() => createClient(ChangeService, transport), [transport]);
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const [reading, setReading] = useState(false);
  const [failure, setFailure] = useState<unknown>();
  const [found, setFound] = useState<ImportOnePanelResponse>();
  // The plan the last read made, until it's handed to the sheet, which is then its own.
  const unreviewed = useRef<Change>(undefined);

  const letGo = () => {
    const c = unreviewed.current;
    unreviewed.current = undefined;
    if (c) void changes.discardChange({ id: c.id }).catch(() => undefined);
  };
  // Leaving with a plan nobody reviewed leaves nothing planned behind.
  useEffect(
    () => () => {
      const c = unreviewed.current;
      if (c) void changes.discardChange({ id: c.id }).catch(() => undefined);
    },
    [changes],
  );

  const read = async () => {
    setReading(true);
    setFailure(undefined);
    letGo();
    try {
      const res = await imports.importOnePanel({ serverId: server.id });
      unreviewed.current = res.change;
      setFound(res);
    } catch (err) {
      setFailure(err);
    } finally {
      setReading(false);
    }
  };

  const review = (change: Change) => {
    unreviewed.current = undefined;
    sheet.open({
      kind: "plan",
      make: async () => ({ change }),
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED) {
          void navigate({ to: "/apps" });
          return;
        }
        // Closed unapplied, it's discarded, so a review needs another read.
        setFound((f) => (f ? { ...f, change: undefined } : f));
      },
    });
  };

  const steps = found?.steps ?? [];
  const groups: { action: ImportAction; title: string; icon: IconName }[] = [
    { action: ImportAction.TAKE_ON, title: t`Take on, as it runs`, icon: "check" },
    { action: ImportAction.MAKE, title: t`Make again in Dockwell`, icon: "plus" },
    { action: ImportAction.LEAVE, title: t`Leave as it is`, icon: "minus" },
  ];
  const panel = found?.found;
  const change = found?.change;
  return (
    <div className="onepanel">
      <Panel title={t`Move in from 1Panel`}>
        <div className="onepanel-intro">
          <p>
            <Trans>
              Dockwell reads what 1Panel keeps on {server.name}, from its own records, read-only, and plans taking on
              what runs as it is, with no restart. It says what it would make again, and what it would leave and why.
              Nothing changes on the server until you apply the plan.
            </Trans>
          </p>
          <div className="onepanel-actions">
            <Button
              variant={found ? "secondary" : "primary"}
              icon="search"
              loading={reading}
              onClick={() => void read()}
            >
              {found ? <Trans>Read it again</Trans> : <Trans>Read what 1Panel keeps</Trans>}
            </Button>
            {change ? (
              <Button variant="primary" icon="arrow-right" onClick={() => review(change)}>
                <Trans>Review the plan</Trans>
              </Button>
            ) : null}
          </div>
        </div>
      </Panel>
      {failure ? (
        <Callout tone="critical" title={t`Couldn't read what 1Panel keeps`}>
          {sentence(failure)}
        </Callout>
      ) : null}
      {panel ? (
        <p className="onepanel-found">
          <Trans>
            1Panel {panel.version}, under <code>{panel.baseDir}</code>, read from <code>{panel.records}</code>.
          </Trans>
        </p>
      ) : null}
      {panel && !panel.knownVersion ? (
        <Callout tone="warning" title={t`A version of 1Panel this wasn't written from`}>
          <Trans>
            It was read the same way, by its records' column names, so check what it found before you apply the plan.
          </Trans>
        </Callout>
      ) : null}
      {found && !change && steps.some((s) => s.action === ImportAction.TAKE_ON) ? (
        <Callout tone="info" title={t`Nothing is planned yet`}>
          <Trans>What would be taken on says why it isn't planned now.</Trans>
        </Callout>
      ) : null}
      {found
        ? groups.map((g) => {
            const list = steps.filter((s) => s.action === g.action);
            if (list.length === 0) return null;
            return (
              <Panel key={g.action} flush title={`${g.title} (${list.length})`}>
                <ul className="onepanel-steps" aria-label={g.title}>
                  {list.map((s) => (
                    <StepRow key={`${s.kind}:${s.name}`} step={s} />
                  ))}
                </ul>
              </Panel>
            );
          })
        : null}
      {found && steps.length > 0 ? (
        <p className="onepanel-count">{plural(steps.length, { one: "# thing found", other: "# things found" })}</p>
      ) : null}
    </div>
  );
}

const kindIcons: Record<ImportKind, IconName> = {
  [ImportKind.UNSPECIFIED]: "question",
  [ImportKind.APP]: "layers",
  [ImportKind.RUNTIME]: "terminal",
  [ImportKind.WEBSITE]: "browser",
  [ImportKind.CERTIFICATE]: "lock",
  [ImportKind.DATABASE_SERVER]: "database",
  [ImportKind.DATABASE]: "database",
  [ImportKind.CRON_JOB]: "clock",
  [ImportKind.PANEL]: "settings",
};

function useKindWord(): (k: ImportKind) => string {
  const { t } = useLingui();
  return (k) => {
    switch (k) {
      case ImportKind.APP:
        return t`App`;
      case ImportKind.RUNTIME:
        return t`Runtime`;
      case ImportKind.WEBSITE:
        return t`Website`;
      case ImportKind.CERTIFICATE:
        return t`Certificate`;
      case ImportKind.DATABASE_SERVER:
        return t`Database server`;
      case ImportKind.DATABASE:
        return t`Database`;
      case ImportKind.CRON_JOB:
        return t`Cron job`;
      default:
        return t`1Panel`;
    }
  };
}

/** One thing found: what it is, what it becomes, and why. The words are the import's own. */
function StepRow({ step }: { step: ImportStep }) {
  const kindWord = useKindWord();
  return (
    <li className="onepanel-step">
      <span className="onepanel-step-icon">
        <Icon name={kindIcons[step.kind] ?? "question"} size={15} />
      </span>
      <div className="onepanel-step-what">
        <div className="onepanel-step-head">
          <span className="onepanel-step-name" title={step.name}>
            {step.name}
          </span>
          <Tag words>{kindWord(step.kind)}</Tag>
          {step.waitsFor ? (
            <Tag words>
              <Trans>Waits for {step.waitsFor}</Trans>
            </Tag>
          ) : null}
        </div>
        <p className="onepanel-step-line">{step.summary}</p>
        {step.becomes ? (
          <p className="onepanel-step-line">
            <Trans>Becomes {step.becomes}.</Trans>
          </p>
        ) : null}
        {step.reason ? <p className="onepanel-step-line onepanel-step-why">{step.reason}</p> : null}
      </div>
    </li>
  );
}
