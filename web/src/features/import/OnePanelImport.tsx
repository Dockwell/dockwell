// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  ImportAction,
  ImportKind,
  type ImportOnePanelResponse,
  ImportService,
  type ImportStep,
  ImportWaitsFor,
  type Server,
} from "@dockwell/api";
import { Button, Callout, Icon, type IconName, Panel, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ServerFrame } from "../servers/ServerFrame";
import { useOnePanelHere } from "./onePanelHere";
import "./OnePanelImport.css";

/*
 * Moving a server off 1Panel (docs/adr/0056-importing-a-1panel-server.md). The page
 * reads what 1Panel keeps when a person asks, and plans nothing: Review the plan
 * has the plan sheet make it, so the sheet's is the plan from the start, and
 * leaving, reloading or closing discards it as the sheet discards any it made.
 * Nothing on the server changes until it's applied.
 */

/** Where a person said not now, on this browser alone, by server. */
function notNowKey(serverId: string): string {
  return `dockwell.onepanel.notNow.${serverId}`;
}

function saidNotNow(serverId: string): boolean {
  try {
    return localStorage.getItem(notNowKey(serverId)) !== null;
  } catch {
    return false;
  }
}

/**
 * On a server's Overview, where Docker runs containers 1Panel made: a way to move
 * them in, until they've moved, or someone says not now. The server's Move in tab
 * stays either way.
 */
export function OnePanelCallout({ server }: { server: Server }) {
  const { t } = useLingui();
  const here = useOnePanelHere(server.id);
  const [notNow, setNotNow] = useState(() => saidNotNow(server.id));
  if (!here || notNow) return null;
  const dismiss = () => {
    try {
      localStorage.setItem(notNowKey(server.id), "1");
    } catch {
      // A private window forgets, and it shows again next time.
    }
    setNotNow(true);
  };
  return (
    <Callout
      tone="info"
      title={t`Containers 1Panel made run on this server`}
      actions={
        <>
          <RouterButtonLink
            size="sm"
            to="/servers/$serverId/import"
            params={{ serverId: server.id }}
            icon="arrow-right"
          >
            <Trans>Move in from 1Panel</Trans>
          </RouterButtonLink>
          <Button size="sm" variant="ghost" onClick={dismiss}>
            <Trans>Not now</Trans>
          </Button>
        </>
      }
    >
      <Trans>
        Dockwell can take on what 1Panel runs as it is, with no restart, and say what it would make again and what it
        would leave.
      </Trans>
    </Callout>
  );
}

/** A server's page for moving in from 1Panel. */
export function OnePanelImportPage({ serverId }: { serverId: string }) {
  return <ServerFrame id={serverId}>{(server) => <OnePanelImport server={server} />}</ServerFrame>;
}

/** A group of steps, by when each moves. */
interface Group {
  key: string;
  title: string;
  steps: ImportStep[];
  /** Folded at first: what waits for a part of Dockwell, which can be long. */
  folded: boolean;
}

function OnePanelImport({ server }: { server: Server }) {
  const { t } = useLingui();
  const transport = useTransport();
  const imports = useMemo(() => createClient(ImportService, transport), [transport]);
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const [reading, setReading] = useState(false);
  const [failure, setFailure] = useState<unknown>();
  const [found, setFound] = useState<ImportOnePanelResponse>();

  // A read plans nothing: what's found, and what an import would do with it.
  const read = async () => {
    setReading(true);
    setFailure(undefined);
    try {
      setFound(await imports.importOnePanel({ serverId: server.id, skipPlan: true }));
    } catch (err) {
      // What an earlier read found may have changed, so it goes.
      setFound(undefined);
      setFailure(err);
    } finally {
      setReading(false);
    }
  };

  // The sheet makes the plan, so it's the sheet's to discard.
  const review = () => {
    sheet.open({
      kind: "plan",
      make: async () => {
        const res = await imports.importOnePanel({ serverId: server.id });
        setFound(res);
        return { change: res.change };
      },
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED) void navigate({ to: "/apps" });
      },
    });
  };

  const steps = found?.steps ?? [];
  const groups = useGroups(steps);
  const toTake = steps.some((s) => s.action === ImportAction.TAKE_ON);
  const panel = found?.found;
  const notHere = reasonOf(failure) === "onepanel_not_found";
  const reviewButton = (
    <Button variant="primary" icon="arrow-right" onClick={review}>
      <Trans>Review the plan</Trans>
    </Button>
  );
  return (
    <div className="onepanel">
      <Panel title={t`Move in from 1Panel`}>
        <div className="onepanel-intro">
          <p>
            <Trans>
              Dockwell reads what 1Panel keeps on {server.name}, from its own records, read-only, and says what it would
              take on as it runs, with no restart, what it would make again, and what it would leave, and why. Review
              the plan to see each change. Nothing changes on the server until you apply it.
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
            {toTake ? reviewButton : null}
          </div>
        </div>
      </Panel>
      {failure && notHere ? (
        <Callout tone="info" title={t`1Panel isn't on this server`}>
          {sentence(failure)}
        </Callout>
      ) : failure ? (
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
            This was written from 1Panel v2.0 to v2.4.2, and v1.10, and this server runs {panel.version}. It was read
            the same way, by its records' column names, so check what it found before you apply the plan.
          </Trans>
        </Callout>
      ) : null}
      {found && !toTake ? (
        <Callout tone="info" title={t`Nothing to plan now`}>
          <Trans>Each thing below says whether it's in Dockwell already, what it waits for, or why it stays.</Trans>
        </Callout>
      ) : null}
      {groups.map((g) => (
        <StepGroup key={g.key} group={g} />
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

/** The steps by when each moves: in this plan, later, in Dockwell already, or not at all. */
function useGroups(steps: ImportStep[]): Group[] {
  const { t } = useLingui();
  const now = (s: ImportStep) =>
    (s.action === ImportAction.TAKE_ON || s.action === ImportAction.MAKE) && s.waitsFor === ImportWaitsFor.UNSPECIFIED;
  const later = (wf: ImportWaitsFor) => (s: ImportStep) => s.action !== ImportAction.LEAVE && s.waitsFor === wf;
  const all: Group[] = [
    { key: "plan", title: t`In this plan`, steps: steps.filter(now), folded: false },
    {
      key: "websites",
      title: t`Later, once Dockwell has websites`,
      steps: steps.filter(later(ImportWaitsFor.WEBSITES)),
      folded: true,
    },
    {
      key: "databases",
      title: t`Later, once Dockwell has databases`,
      steps: steps.filter(later(ImportWaitsFor.DATABASES)),
      folded: true,
    },
    {
      key: "tasks",
      title: t`Later, once Dockwell has scheduled tasks`,
      steps: steps.filter(later(ImportWaitsFor.SCHEDULED_TASKS)),
      folded: true,
    },
    {
      key: "firewall",
      title: t`Later, once Dockwell has firewall rules`,
      steps: steps.filter(later(ImportWaitsFor.FIREWALL)),
      folded: true,
    },
    {
      key: "done",
      title: t`In Dockwell already`,
      steps: steps.filter((s) => s.action === ImportAction.DONE),
      folded: false,
    },
    {
      key: "stays",
      title: t`Stays with 1Panel`,
      steps: steps.filter((s) => s.action === ImportAction.LEAVE),
      folded: false,
    },
  ];
  return all.filter((g) => g.steps.length > 0);
}

/**
 * Each step with a key: its kind, name and summary, as a name can repeat within a
 * kind, such as a database of the same name on two servers, and a count beside
 * those where they repeat too.
 */
function keyed(steps: ImportStep[]): { key: string; step: ImportStep }[] {
  const seen = new Map<string, number>();
  return steps.map((step) => {
    const base = `${step.kind}:${step.name}:${step.summary}`;
    const n = (seen.get(base) ?? 0) + 1;
    seen.set(base, n);
    return { key: n > 1 ? `${base}:${n}` : base, step };
  });
}

/** A group's steps, folded to a count where it waits, so a long list stays short. */
function StepGroup({ group }: { group: Group }) {
  const { t } = useLingui();
  const [open, setOpen] = useState(!group.folded);
  const count = group.steps.length;
  return (
    <Panel
      flush
      title={`${group.title} (${count})`}
      actions={
        group.folded ? (
          <Button
            size="sm"
            variant="ghost"
            icon={open ? "chevron-up" : "chevron-down"}
            aria-expanded={open}
            onClick={() => setOpen(!open)}
          >
            {open ? t`Hide them` : plural(count, { one: "Show it", other: "Show all #" })}
          </Button>
        ) : undefined
      }
    >
      {open ? (
        <ul className="onepanel-steps" aria-label={group.title}>
          {keyed(group.steps).map(({ key, step }) => (
            <StepRow key={key} step={step} />
          ))}
        </ul>
      ) : null}
    </Panel>
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
  [ImportKind.COMPOSE_PROJECT]: "layers",
  [ImportKind.CONTAINERS]: "layers",
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
      case ImportKind.COMPOSE_PROJECT:
        return t`Compose project`;
      case ImportKind.CONTAINERS:
        return t`Containers`;
      default:
        return t`1Panel`;
    }
  };
}

/**
 * One thing found: what it is, what it becomes, and why. Those are the import's own
 * words, in English, which no wrapping here pretends to translate.
 */
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
        </div>
        <p className="onepanel-step-line" lang="en">
          {step.summary}
        </p>
        {step.becomes ? (
          <p className="onepanel-step-line">
            <span className="onepanel-step-label">
              <Trans>Becomes</Trans>
            </span>
            <span lang="en">{step.becomes}</span>
          </p>
        ) : null}
        {step.reason ? (
          <p className="onepanel-step-line onepanel-step-why" lang="en">
            {step.reason}
          </p>
        ) : null}
      </div>
    </li>
  );
}
