// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  type EditSchema,
  type Plan,
  PlanWarning,
  type ServerPlan,
  type StepProgress,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  type IconName,
  PHONE_QUERY,
  PlanDiff,
  Sheet,
  StatusBadge,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { onApple } from "../../lib/platform";
import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow } from "../servers/useFleet";
import { followChange } from "./follow";
import { canUndo, diffResources, isFinal, longestDowntime, shortFingerprint } from "./plan";
import { type ChangeWords, useChangeWords } from "./words";
import "./ChangeSheet.css";

/** What the plan sheet opens on: edits to plan, a change on record, or the undo of one. */
export type Opening =
  | { kind: "propose"; summary: string; edits: MessageInitShape<typeof EditSchema>[] }
  | { kind: "change"; id: string }
  | { kind: "undo"; id: string };

interface ChangeSheetApi {
  /** Opens the plan sheet, in place of any open already. */
  open(opening: Opening): void;
}

const SheetContext = createContext<ChangeSheetApi>({ open: () => {} });

/** Opens the plan sheet, from anywhere in the shell. */
export function useChangeSheet(): ChangeSheetApi {
  return useContext(SheetContext);
}

/**
 * Holds the plan sheet, so anything in the shell can plan a change, review
 * one on record, or undo one: the command bar, Activity, and later every
 * screen that changes something.
 */
export function ChangeSheetProvider({ children }: { children: ReactNode }) {
  const [shown, setShown] = useState<{ opening: Opening; key: number }>();
  const open = useCallback((opening: Opening) => setShown((was) => ({ opening, key: (was?.key ?? 0) + 1 })), []);
  const api = useMemo(() => ({ open }), [open]);
  return (
    <SheetContext value={api}>
      {children}
      {shown ? <ChangeSheet key={shown.key} opening={shown.opening} onClose={() => setShown(undefined)} /> : null}
    </SheetContext>
  );
}

/** A later state wins, so an apply's answer never takes back what the watch already said. */
const ORDER: Readonly<Record<ChangeState, number>> = {
  [ChangeState.UNSPECIFIED]: 0,
  [ChangeState.PLANNED]: 1,
  [ChangeState.APPLYING]: 2,
  [ChangeState.APPLIED]: 3,
  [ChangeState.FAILED]: 3,
  [ChangeState.DISCARDED]: 3,
};

function later(was: Change | undefined, next: Change | undefined): Change | undefined {
  if (!next) return was;
  if (was && was.id === next.id && ORDER[was.state] > ORDER[next.state]) return was;
  return next;
}

type Busy = "apply" | "discard" | undefined;

/**
 * The plan sheet: what a change does, where, for how long, and whether it
 * can be undone, before anything runs (Plan-Review, M-Plan-Approve). Apply
 * follows the steps inline as they run. A plan that destroys data or may
 * cut off access asks for its fingerprint to be typed first, which the API
 * checks too (ADR 0017). Closing a plan this sheet made discards it.
 */
function ChangeSheet({ opening, onClose }: { opening: Opening; onClose: () => void }) {
  const { t, i18n } = useLingui();
  const words = useChangeWords();
  const transport = useTransport();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const { open } = useChangeSheet();
  const phone = useMediaQuery(PHONE_QUERY);
  const now = useControlPlaneNow(30_000);
  const [change, setChange] = useState<Change>();
  const [failure, setFailure] = useState<unknown>();
  const [busy, setBusy] = useState<Busy>();
  const [typed, setTyped] = useState("");
  // A plan this sheet made is this sheet's to discard when it closes unapplied.
  const own = opening.kind !== "change";

  useEffect(() => {
    const abort = new AbortController();
    const { signal } = abort;
    const start = async () => {
      try {
        const res =
          opening.kind === "propose"
            ? await client.proposeChange({ summary: opening.summary, edits: opening.edits }, { signal })
            : opening.kind === "undo"
              ? await client.undoChange({ id: opening.id }, { signal })
              : await client.getChange({ id: opening.id }, { signal });
        if (!signal.aborted) setChange(res.change);
      } catch (err) {
        if (!signal.aborted) setFailure(err);
      }
    };
    void start();
    return () => abort.abort();
  }, [client, opening]);

  // Follow the change while it can move on: someone may apply or discard a
  // plan, and an apply's steps run after it returns.
  const id = change?.id;
  const moving = change !== undefined && !isFinal(change);
  useEffect(() => {
    if (!id || !moving) return;
    const abort = new AbortController();
    void followChange(transport, id, abort.signal, (next) => setChange((was) => later(was, next)));
    return () => abort.abort();
  }, [transport, id, moving]);

  const plan = change?.plan;
  const planned = change?.state === ChangeState.PLANNED;
  const confirmWith = plan?.destructive ? shortFingerprint(plan) : "";
  const confirmed = confirmWith === "" || typed.trim().toLowerCase() === confirmWith.toLowerCase();

  const apply = useCallback(async () => {
    if (!change || !confirmed || busy) return;
    setBusy("apply");
    setFailure(undefined);
    try {
      const res = await client.applyChange({ id: change.id, fingerprint: change.plan?.fingerprint ?? "" });
      setChange((was) => later(was, res.change));
    } catch (err) {
      setFailure(err);
    } finally {
      setBusy(undefined);
    }
  }, [change, confirmed, busy, client]);

  const close = useCallback(() => {
    if (own && change?.state === ChangeState.PLANNED && busy !== "apply") {
      setBusy("discard");
      // The plan goes whether or not anyone waits; a failure leaves it planned, which Activity shows.
      void client.discardChange({ id: change.id }).catch(() => {});
    }
    onClose();
  }, [own, change, busy, client, onClose]);

  // ⌘↵ (Ctrl+Enter elsewhere) applies, as the button says.
  useEffect(() => {
    if (!planned) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Enter" || !(event.metaKey || event.ctrlKey) || event.isComposing) return;
      event.preventDefault();
      void apply();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [planned, apply]);

  const title = !change
    ? failure
      ? t`The plan didn't work`
      : t`Planning`
    : {
        [ChangeState.UNSPECIFIED]: t`Review plan`,
        [ChangeState.PLANNED]: t`Review plan`,
        [ChangeState.APPLYING]: t`Applying`,
        [ChangeState.APPLIED]: t`Applied`,
        [ChangeState.FAILED]: t`The change failed`,
        [ChangeState.DISCARDED]: t`Discarded`,
      }[change.state];
  const subtitle = change?.summary ?? (opening.kind === "propose" ? opening.summary : undefined);
  const undoDays = Math.round(Number(plan?.undoWindow?.seconds ?? 604_800n) / 86_400);
  const undoable = canUndo(change, now);

  let footer: ReactNode;
  if (!change) {
    footer = (
      <Button variant="ghost" onClick={onClose}>
        <Trans>Close</Trans>
      </Button>
    );
  } else if (planned) {
    footer = (
      <>
        <span className="dw-sheet-note">
          <Icon name="undo" size={14} />
          <Trans>Undo for {undoDays} days</Trans>
        </span>
        <Button variant="ghost" size={phone ? "lg" : "md"} onClick={close} disabled={busy === "apply"}>
          <Trans>Cancel</Trans>
        </Button>
        <Button
          variant="primary"
          size={phone ? "lg" : "md"}
          icon="check"
          kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
          loading={busy === "apply"}
          disabled={!confirmed}
          onClick={() => void apply()}
          className="plan-apply"
        >
          <Trans>Apply plan</Trans>
        </Button>
      </>
    );
  } else if (change.state === ChangeState.APPLYING) {
    footer = (
      <>
        <span className="dw-sheet-note">
          <Trans>It keeps applying if you close this.</Trans>
        </span>
        <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
          <Trans>Close</Trans>
        </Button>
      </>
    );
  } else {
    footer = (
      <>
        {undoable ? (
          <Button
            variant="ghost"
            size={phone ? "lg" : "md"}
            icon="undo"
            onClick={() => open({ kind: "undo", id: change.id })}
          >
            <Trans>Undo</Trans>
          </Button>
        ) : null}
        <Button size={phone ? "lg" : "md"} onClick={onClose}>
          <Trans>Done</Trans>
        </Button>
      </>
    );
  }

  return (
    <Sheet
      open
      title={title}
      subtitle={subtitle}
      onClose={close}
      footer={footer}
      className={cx("plan-sheet", phone && "is-phone")}
    >
      <div className="plan-body">
        {failure ? (
          <Callout tone="critical" title={change ? t`That didn't work` : undefined}>
            {sentence(failure)}
          </Callout>
        ) : null}
        {!change && !failure ? (
          <p className="plan-loading" aria-busy="true">
            <Trans>Planning: asking each server what this would take</Trans>
          </p>
        ) : null}
        {change ? (
          <>
            {change.state === ChangeState.FAILED ? (
              <Callout tone="critical" title={t`A step failed, so the change stopped there`}>
                {change.error}
                {change.rolledBackBy ? (
                  <>
                    {" "}
                    <Trans>Dockwell rolled back the steps already done.</Trans>
                  </>
                ) : null}
              </Callout>
            ) : null}
            {change.state === ChangeState.APPLIED ? (
              <Callout tone="healthy" title={change.steps.length > 0 ? t`Every step ran` : t`Recorded and applied`}>
                {change.undoneBy ? (
                  <Trans>It has been undone since.</Trans>
                ) : undoable && change.undoDeadline ? (
                  <Trans>
                    You can undo it until {formatDateTime(timestampDate(change.undoDeadline), i18n.locale)}.
                  </Trans>
                ) : null}
              </Callout>
            ) : null}
            <section className="plan-section" aria-label={t`Plan`}>
              {phone ? (
                <h3 className="dw-text-heading plan-heading">
                  <Trans>Plan</Trans>
                </h3>
              ) : null}
              {/* The impact sits on the diff on wide screens (Plan-Review), and leads the effects below on phones (M-Plan-Approve). */}
              <PlanDiff
                resources={diffResources(plan, t`Nothing to do`)}
                impact={phone ? undefined : <Impact plan={plan} />}
              />
            </section>
            <Steps plan={plan} progress={change.steps} words={words} />
            <Effects
              reach={phone}
              change={change}
              plan={plan}
              confirmWith={planned ? confirmWith : ""}
              typed={typed}
              onType={setTyped}
            />
          </>
        ) : null}
      </div>
    </Sheet>
  );
}

/** The plan's reach in one line: which servers, and how long anything is down. */
function Impact({ plan }: { plan: Plan | undefined }) {
  const time = useTimeWords();
  const servers = (plan?.servers ?? []).filter((s) => !s.removed);
  if (servers.length === 0) return <Trans>No server · nothing goes down</Trans>;
  const names = servers.map((s) => s.serverName).join(", ");
  const down = longestDowntime(servers);
  if (down === 0) return <Trans>{names} · no downtime expected</Trans>;
  const span = time.span(down);
  return (
    <Trans>
      {names} · about {span} down
    </Trans>
  );
}

/** Each server's steps, with how each is going once the change applies. */
function Steps({ plan, progress, words }: { plan: Plan | undefined; progress: StepProgress[]; words: ChangeWords }) {
  const { t } = useLingui();
  const servers = plan?.servers ?? [];
  if (servers.length === 0) return null;
  return (
    <section className="plan-section" aria-label={t`Steps by server`}>
      <h3 className="dw-text-label plan-heading">
        <Trans>Steps by server</Trans>
      </h3>
      {servers.map((server) => (
        <ServerSteps key={server.serverId} server={server} progress={progress} words={words} />
      ))}
    </section>
  );
}

function ServerSteps({
  server,
  progress,
  words,
}: {
  server: ServerPlan;
  progress: StepProgress[];
  words: ChangeWords;
}) {
  const { t } = useLingui();
  const time = useTimeWords();
  const down = longestDowntime([server]);
  const span = time.span(down);
  return (
    <div className="plan-server">
      <div className="plan-server-head">
        <Icon name="server" size={14} />
        <code className="plan-server-name">{server.serverName}</code>
        {server.offline ? <Tag>{t`Offline`}</Tag> : null}
        {server.removed ? <Tag>{t`Removed`}</Tag> : null}
        <span className="plan-server-meta">
          {server.removed ? (
            <Trans>No steps: it left the fleet</Trans>
          ) : down > 0 ? (
            <Trans>About {span} down</Trans>
          ) : (
            <Trans>No downtime</Trans>
          )}
        </span>
      </div>
      {server.steps.length > 0 ? (
        <ol className="plan-steps">
          {server.steps.map((step) => {
            const name = step.resource?.name ?? step.resourceId;
            const live = progress.find((p) => p.serverId === server.serverId && p.resourceId === step.resourceId);
            const look = live ? words.step(live.state) : undefined;
            const effect = words.effect(step.effect);
            const stepDown = step.downtime ? Number(step.downtime.seconds) * 1000 + step.downtime.nanos / 1e6 : 0;
            const stepSpan = time.span(stepDown);
            return (
              <li key={step.resourceId} className="plan-step">
                <span className="plan-step-what">
                  {look ? <StatusBadge status={look.status} label={look.word} /> : null}
                  <span>
                    {words.action(step.action)} <code>{name}</code>
                  </span>
                  <span className="plan-step-kind">{words.kind(step.resource?.kind ?? "")}</span>
                </span>
                <span className="plan-step-meta">
                  {effect ? <span>{effect}</span> : null}
                  {stepDown > 0 ? <span>{t`About ${stepSpan} down`}</span> : null}
                  {step.destroysData ? <span className="plan-step-warn">{t`Destroys data`}</span> : null}
                  {step.warnings.includes(PlanWarning.MAY_LOCK_OUT) ? (
                    <span className="plan-step-warn">{t`May cut off access to ${server.serverName}`}</span>
                  ) : null}
                  {live?.message && live.message !== "" ? <span>{live.message}</span> : null}
                </span>
              </li>
            );
          })}
        </ol>
      ) : null}
    </div>
  );
}

interface EffectsProps {
  /** Leads with the plan's reach, which the diff carries on wide screens. */
  reach: boolean;
  change: Change;
  plan: Plan | undefined;
  /** The fingerprint to type before applying, or empty when there is nothing to confirm. */
  confirmWith: string;
  typed: string;
  onType: (value: string) => void;
}

/** What applying means beyond the diff: reach, undo, offline servers, and what can't come back (M-Plan-Approve). */
function Effects({ reach, change, plan, confirmWith, typed, onType }: EffectsProps) {
  const { t } = useLingui();
  const offline = (plan?.servers ?? []).filter((s) => s.offline && !s.removed).map((s) => s.serverName);
  const rows: { icon: IconName; title: ReactNode; detail?: ReactNode; key: string }[] = [];
  const steps = (plan?.servers ?? []).reduce((n, s) => n + s.steps.length, 0);
  const how =
    steps === 0 ? (
      <Trans>Nothing runs on a server: Dockwell records it and it applies at once.</Trans>
    ) : (
      <Trans>Steps run one at a time on each server, servers side by side.</Trans>
    );
  rows.push(
    reach
      ? { key: "reach", icon: "activity", title: <Impact plan={plan} />, detail: how }
      : { key: "reach", icon: "activity", title: how },
  );
  if (change.state === ChangeState.PLANNED) {
    rows.push({
      key: "undo",
      icon: "undo",
      title: <Trans>Undo is a plan too</Trans>,
      detail: <Trans>Undoing makes a plan of its own, for you to review.</Trans>,
    });
  } else if (change.rollback) {
    rows.push({
      key: "undo",
      icon: "undo",
      title: <Trans>Dockwell made this change to roll back a failed one</Trans>,
    });
  } else if (change.undoneBy) {
    rows.push({ key: "undo", icon: "undo", title: <Trans>Undone</Trans> });
  }
  if (plan?.destructive) {
    rows.push({
      key: "destructive",
      icon: "shield",
      title: <Trans>This plan destroys data or may cut off access</Trans>,
      detail: <Trans>Undo may not bring everything back.</Trans>,
    });
  }
  return (
    <>
      {offline.length > 0 && change.state === ChangeState.PLANNED ? (
        <Callout tone="warning" title={t`Not every server is connected`}>
          <Trans>
            {offline.join(", ")} can't take its steps until its agent reconnects, so the plan can't apply until then.
          </Trans>
        </Callout>
      ) : null}
      <section className="plan-effects" aria-label={t`What applying means`}>
        {rows.map((row) => (
          <div key={row.key} className="plan-effect">
            <Icon name={row.icon} size={16} />
            <div className="plan-effect-text">
              <span className="plan-effect-title">{row.title}</span>
              {row.detail ? <span className="plan-effect-detail">{row.detail}</span> : null}
            </div>
          </div>
        ))}
        {confirmWith ? (
          <div className="plan-confirm">
            <TextField
              label={
                <Trans>
                  Type <code>{confirmWith}</code> to apply it
                </Trans>
              }
              hint={t`The start of this plan's fingerprint, so you apply exactly what you reviewed.`}
              mono
              autoComplete="off"
              spellCheck={false}
              value={typed}
              onChange={(event) => onType(event.target.value)}
            />
          </div>
        ) : null}
      </section>
    </>
  );
}
