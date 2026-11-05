// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type Client, createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  Connection,
  type Domain,
  DomainService,
  type EditSchema,
  EventService,
  IntentKind,
  type Plan,
  PlanAction,
  type PlannedResource,
  ResourceService,
  ServiceService,
  StepState,
  VariableService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  type IconName,
  PHONE_QUERY,
  PlanDiff,
  Sheet,
  SheetSwap,
  SheetView,
  TextField,
  useMediaQuery,
  useSheetViewAway,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatDay } from "../../lib/format";
import { onApple } from "../../lib/platform";
import { useTimeWords } from "../../lib/time";
import { appOf, unreached } from "../apps/app";
import { lookOf, useDomainsReaching } from "../apps/domains";
import { useServices } from "../apps/useServices";
import { FailedDeployLogs } from "../logs/DeployLogLinks";
import { CopyButton } from "../servers/CopyButton";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { FailureNotice, MachineWords } from "./FailureNotice";
import { useFieldRows } from "./fields";
import { followChange } from "./follow";
import { type Notice, needsConfirming, useAsides, useRisks, useWarnings } from "./notices";
import { PlanSteps } from "./PlanSteps";
import {
  canUndo,
  changing,
  diffResources,
  failedStep,
  foldPlumbing,
  foundNothingNewer,
  goesAway,
  goesLive,
  intentOf,
  isEmpty,
  isFinal,
  isFleet,
  longestDowntime,
  longestUntilHealthy,
  madeProject,
  onlyDeletes,
  onlyIfNewer,
  replacesIfNewer,
  shortFingerprint,
  wentOffline,
  withoutIds,
} from "./plan";
import { RollbackLine, rollbackSubject } from "./RollbackLine";
import { useAlsoWords, useChangeWords } from "./words";
import "./ChangeSheet.css";

/** What every opening may say about what comes next. */
interface Then {
  /**
   * What to do once the sheet closes, with the change as it stood, such as
   * leave the page that opened it. Undefined when no plan came of it.
   */
  onClose?: (change: Change | undefined) => void;
  /** What the button at the end of a change that applied says, in place of "Done", such as "Open web". */
  doneLabel?: string;
  /** What the button at the end of a change that failed says, when closing goes back to where it can be fixed. */
  retryLabel?: string;
  /** Leaves altogether from a change that failed, when closing goes back to where it can be fixed: Done and × do this. */
  onDone?: () => void;
  /**
   * What to do about a failure from where this opened, by what it comes down to,
   * as the server's words can't know what the form in front of you has. Undefined
   * leaves the server's own.
   */
  fix?: (reason: string) => string | undefined;
}

/**
 * What the plan sheet opens on: edits to plan, a change on record, the undo
 * of one, or a plan that make makes its own way, such as a rollback, or a
 * form that has it already. A plan the sheet opened on with make is its to
 * discard, as one it proposed itself is. nothing says there is nothing to do
 * without a plan at all, such as a rollback to what already runs: it has no
 * Apply, as a plan that turns out empty hasn't either.
 */
export type Opening = Then &
  (
    | { kind: "propose"; summary: string; edits: MessageInitShape<typeof EditSchema>[] }
    | {
        kind: "change";
        id: string;
        /** It was seen to apply just now, so it can say what is live. */
        witnessed?: boolean;
      }
    | { kind: "undo"; id: string }
    | { kind: "plan"; make: () => Promise<{ change?: Change | undefined }> }
    | { kind: "nothing"; summary?: string; detail?: ReactNode }
  );

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
 * screen that changes something. A form that has a sheet of its own keeps it,
 * and shows its plan there with PlanView.
 */
export function ChangeSheetProvider({ children }: { children: ReactNode }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const [shown, setShown] = useState<{ opening: Opening; key: number }>();
  const open = useCallback((opening: Opening) => setShown((was) => ({ opening, key: (was?.key ?? 0) + 1 })), []);
  const api = useMemo(() => ({ open }), [open]);
  const close = useCallback(
    (change: Change | undefined, movedOn: boolean) => {
      setShown(undefined);
      // What it was opened to do is over once an undo takes its place.
      if (!movedOn) shown?.opening.onClose?.(change);
    },
    [shown],
  );
  return (
    <SheetContext value={api}>
      {children}
      {/* A new opening takes the sheet's place, which lets go of any plan it made, as closing it does. */}
      <Sheet
        open={shown !== undefined}
        className={cx("plan-sheet", phone && "is-phone")}
        over={
          shown ? (
            <SheetSwap viewKey={shown.key}>
              <PlanViews key={shown.key} opening={shown.opening} onClose={close} />
            </SheetSwap>
          ) : undefined
        }
      />
    </SheetContext>
  );
}

/**
 * The plan a form hands on to, in the form's own sheet: pass it to the form's
 * Sheet as over. The shell and the scrim stay where they are, and the view
 * crossfades. Closing it calls the opening's onClose, with the change as it
 * stood; a form that takes the view away then is there again, as it was.
 */
export function PlanView({ opening }: { opening: Opening }) {
  const close = useCallback((change: Change | undefined) => opening.onClose?.(change), [opening]);
  return <PlanViews opening={opening} onClose={close} />;
}

interface Shown {
  opening: Opening;
  key: number;
}

/** One opening of the plan sheet, and where Undo takes it from there, in the same sheet. */
function PlanViews({
  opening,
  onClose,
}: {
  opening: Opening;
  onClose: (change: Change | undefined, movedOn: boolean) => void;
}) {
  const [shown, setShown] = useState<Shown>({ opening, key: 0 });
  const [from, setFrom] = useState(opening);
  if (from !== opening) {
    setFrom(opening);
    setShown({ opening, key: shown.key + 1 });
  }
  const api = useMemo<ChangeSheetApi>(
    () => ({ open: (next) => setShown((was) => ({ opening: next, key: was.key + 1 })) }),
    [],
  );
  const close = (change: Change | undefined) => {
    const current = shown.opening;
    // Cancelling an undo goes back to what it would have undone, not out of the sheet.
    if (current.kind === "undo" && shown.key > 0 && (!change || change.state === ChangeState.PLANNED)) {
      const { doneLabel, retryLabel } = opening;
      setShown({
        opening: {
          kind: "change",
          id: current.id,
          witnessed: true,
          ...(doneLabel ? { doneLabel } : {}),
          ...(retryLabel ? { retryLabel } : {}),
        },
        key: shown.key + 1,
      });
      return;
    }
    onClose(change, shown.key > 0);
  };
  return (
    <SheetContext value={api}>
      <SheetSwap viewKey={shown.key}>
        <ChangeSheet showing={shown} opening={shown.opening} onClose={close} />
      </SheetSwap>
    </SheetContext>
  );
}

/**
 * A plan a sheet makes: one each time the sheet opens, however many times
 * it mounts, as StrictMode mounts each twice in development. The sheets
 * showing it hold it. Once the last lets go without applying it, it is
 * discarded, even if it is still being made then: closing the sheet, or
 * opening another in its place, leaves no plan behind.
 */
interface Proposal {
  change: Promise<Change | undefined>;
  holders: number;
  /** Set while it applies, and once it has, so it stays. */
  keep: boolean;
  leaving?: ReturnType<typeof setTimeout>;
}

/** The plans sheets made, by the time each opened. */
const proposals = new WeakMap<object, Proposal>();

function hold(
  client: Client<typeof ChangeService>,
  showing: object,
  opening: Exclude<Opening, { kind: "change" | "nothing" }>,
): Proposal {
  let proposal = proposals.get(showing);
  if (!proposal) {
    // Never cancelled: a proposal cut off may still be made, and then it must be discarded.
    const made =
      opening.kind === "propose"
        ? client.proposeChange({ summary: opening.summary, edits: opening.edits })
        : opening.kind === "undo"
          ? client.undoChange({ id: opening.id })
          : opening.make();
    proposal = { change: made.then((res) => res.change), holders: 0, keep: false };
    proposals.set(showing, proposal);
  }
  clearTimeout(proposal.leaving);
  proposal.holders += 1;
  return proposal;
}

function letGo(client: Client<typeof ChangeService>, proposal: Proposal): void {
  proposal.holders -= 1;
  if (proposal.holders > 0) return;
  // StrictMode lets go and holds again at once, so wait a moment to be sure the sheet is gone.
  proposal.leaving = setTimeout(() => {
    if (proposal.holders > 0 || proposal.keep) return;
    // A failure leaves the plan planned, which Activity shows.
    proposal.change
      .then((change) => (change && !proposal.keep ? client.discardChange({ id: change.id }) : undefined))
      .catch(() => {});
  }, 0);
}

/** Reads what a change applied or undone may have changed again: the resources, the services and their domains, the timeline's pages and the changes. */
function refreshAfter(queryClient: QueryClient): void {
  for (const schema of [ResourceService, ServiceService, DomainService, EventService, ChangeService, VariableService]) {
    void queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema, cardinality: undefined }) });
  }
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

type Busy = "apply" | undefined;

/** How often a plan refused for another change's sake looks to see whether that has ended. */
const BUSY_LOOK_MS = 1_500;

/** Where the sheet stands, which says everything else about it. */
type Phase =
  /** Asking each server what it would take. */
  | "planning"
  /** No plan came of it. */
  | "unplanned"
  /** There is nothing to do, as a plan or without one. */
  | "nothing"
  | "review"
  | "applying"
  /** It applied, just now, as this sheet watched. */
  | "arrived"
  /** It applied a while ago, and this is its record. */
  | "history"
  | "failed"
  | "discarded";

function phaseOf(opening: Opening, change: Change | undefined, failure: unknown, witnessed: boolean): Phase {
  if (opening.kind === "nothing") return "nothing";
  if (!change) return failure ? "unplanned" : "planning";
  switch (change.state) {
    case ChangeState.APPLYING:
      return "applying";
    case ChangeState.APPLIED:
      return witnessed ? "arrived" : "history";
    case ChangeState.FAILED:
      return "failed";
    case ChangeState.DISCARDED:
      return "discarded";
  }
  return isEmpty(change.plan) ? "nothing" : "review";
}

/**
 * The plan sheet: what a change does, where, for how long, and whether it can
 * be undone, before anything runs (Plan-Review, M-Plan-Approve). What it can't
 * take back leads, and a plan like that asks for its fingerprint beside Apply,
 * which the API checks too (ADR 0017). Apply follows the steps inline as they
 * run, in the same place, and then says what happened: what is live and where,
 * or what went wrong and what to do about it. Closing a plan this sheet made
 * discards it, as opening another sheet in its place does.
 */
function ChangeSheet({
  showing,
  opening,
  onClose,
}: {
  /** This time the sheet opened, which its plan, if it makes one, is kept by. */
  showing: object;
  opening: Opening;
  onClose: (change: Change | undefined) => void;
}) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const { open } = useChangeSheet();
  const phone = useMediaQuery(PHONE_QUERY);
  const away = useSheetViewAway();
  const now = useControlPlaneNow(30_000);
  const [change, setChange] = useState<Change>();
  const [failure, setFailure] = useState<unknown>();
  const [busy, setBusy] = useState<Busy>();
  const [typed, setTyped] = useState("");
  const [witnessed, setWitnessed] = useState(opening.kind !== "change" || opening.witnessed === true);
  const queryClient = useQueryClient();
  const rows = useFieldRows();
  const { servers: fleetServers } = useFleetView();
  // The plan this sheet made, if it made one: its to discard unless it applies.
  const proposal = useRef<Proposal | undefined>(undefined);
  const heading = useRef<HTMLHeadingElement>(null);
  const primary = useRef<HTMLButtonElement>(null);
  const confirmField = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (opening.kind === "nothing") return;
    let live = true;
    const show = (next: Change | undefined) => live && setChange(next);
    const fail = (err: unknown) => live && setFailure(err);
    if (opening.kind === "change") {
      const abort = new AbortController();
      client.getChange({ id: opening.id }, { signal: abort.signal }).then((res) => show(res.change), fail);
      return () => {
        live = false;
        abort.abort();
      };
    }
    const held = hold(client, showing, opening);
    proposal.current = held;
    held.change.then(show, fail);
    return () => {
      live = false;
      proposal.current = undefined;
      letGo(client, held);
    };
  }, [client, showing, opening]);

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

  // Once a change applies, or fails partway, what the command bar and Activity
  // read about resources and changes may be out of date.
  const state = change?.state;
  useEffect(() => {
    if (state === ChangeState.APPLIED || state === ChangeState.FAILED) refreshAfter(queryClient);
  }, [state, queryClient]);

  // "Another change is applying this" stops being true without anyone saying so,
  // and this plan is ready again the moment it does.
  const refusedForNow = change?.state === ChangeState.PLANNED && failure !== undefined && reasonOf(failure) === "busy";
  useEffect(() => {
    if (!refusedForNow) return;
    const abort = new AbortController();
    const timer = setInterval(() => {
      client.listChanges({ pageSize: 50 }, { signal: abort.signal }).then(
        (res) => {
          if (!res.changes.some((c) => c.state === ChangeState.APPLYING)) setFailure(undefined);
        },
        () => {},
      );
    }, BUSY_LOOK_MS);
    return () => {
      clearInterval(timer);
      abort.abort();
    };
  }, [refusedForNow, client]);

  // A rollback runs on after the change has failed, and what it puts back moves those on again.
  const readAgain = useCallback(() => refreshAfter(queryClient), [queryClient]);

  // A change seen moving is one this sheet watched arrive.
  useEffect(() => {
    if (state === ChangeState.APPLYING || state === ChangeState.PLANNED) setWitnessed(true);
  }, [state]);

  const plan = change?.plan;
  const phase = phaseOf(opening, change, failure, witnessed);
  const confirming = phase === "review" && needsConfirming(plan);
  const confirmWith = confirming ? shortFingerprint(plan) : "";
  const confirmed = confirmWith === "" || typed.trim().toLowerCase() === confirmWith.toLowerCase();
  // A server the plan found away may be back by now; the fleet says, and its word is the latest.
  const offline = (plan?.servers ?? [])
    .filter((s) => s.offline && !s.removed)
    .filter((s) => {
      const known = fleetServers?.find((f) => f.id === s.serverId);
      return !known || known.connection !== Connection.CONNECTED;
    })
    .map((s) => s.serverName);
  const canApply = confirmed && offline.length === 0;

  const apply = useCallback(async () => {
    if (!change || !canApply || busy) return;
    setBusy("apply");
    setFailure(undefined);
    // Closing the sheet while it applies leaves it to apply.
    const held = proposal.current;
    if (held) held.keep = true;
    try {
      const res = await client.applyChange({ id: change.id, fingerprint: change.plan?.fingerprint ?? "" });
      setChange((was) => later(was, res.change));
    } catch (err) {
      // Still planned: closing discards it, as before.
      if (held) held.keep = false;
      setFailure(err);
    } finally {
      setBusy(undefined);
    }
  }, [change, canApply, busy, client]);

  // Closing lets go of the plan this sheet made, which discards it unless it applied.
  const close = useCallback(() => onClose(change), [onClose, change]);
  // From a change that failed, Done and × leave, where the way back goes to the form.
  const leave = phase === "failed" && opening.onDone ? opening.onDone : close;

  // ⌘↵ (Ctrl+Enter elsewhere) goes on through the flow, as each button says:
  // it applies the plan, and once that is over, takes the way on. A key held
  // down from the step before doesn't count, so it never applies a plan unseen.
  const over = phase === "arrived" || phase === "failed" || phase === "history" || phase === "discarded";
  useEffect(() => {
    if (away || (phase !== "review" && !over)) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Enter" || !(event.metaKey || event.ctrlKey) || event.isComposing || event.repeat) return;
      event.preventDefault();
      if (phase === "review") void apply();
      else primary.current?.click();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [away, phase, over, apply]);

  // Focus goes where the next step is, so a keyboard needs no Tab to go on, and
  // never falls out of the sheet as a button it was on goes away. It stays on
  // anything else someone moved to meanwhile, such as a row they opened.
  const wasPhase = useRef<Phase>(undefined);
  useEffect(() => {
    if (away || wasPhase.current === phase) return;
    wasPhase.current = phase;
    const focused = document.activeElement;
    const adrift =
      !focused ||
      focused === document.body ||
      focused === heading.current ||
      focused.getAttribute("role") === "dialog" ||
      focused.classList.contains("plan-apply");
    if (phase === "review" && needsConfirming(plan)) confirmField.current?.focus({ preventScroll: true });
    else if (phase === "applying" && adrift) heading.current?.focus({ preventScroll: true });
    else if ((phase === "arrived" || phase === "failed") && adrift) primary.current?.focus({ preventScroll: true });
  }, [away, phase, plan]);

  const size = phone ? "lg" : "md";
  const kbd = phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵";
  const deleting = onlyDeletes(plan);
  const undoDays = Math.round(Number(plan?.undoWindow?.seconds ?? 604_800n) / 86_400);
  const undoable = canUndo(change, now);
  const subtitle =
    opening.kind === "nothing"
      ? opening.summary
      : (change?.summary ?? (opening.kind === "propose" ? opening.summary : undefined));
  const title = useTitle(phase, plan, change);
  const subject = changing(plan)[0]?.resource?.name ?? "";
  const names = [
    ...changing(plan).map((r) => r.resource?.name ?? ""),
    ...(plan?.servers ?? []).map((s) => s.serverName),
  ];
  const applyWords = deleting
    ? changing(plan).length === 1
      ? t`Delete ${subject}`
      : plural(changing(plan).length, { one: "Delete # resource", other: "Delete # resources" })
    : t`Apply plan`;

  const applyButton = (
    <Button
      variant={deleting || confirming ? "danger" : "primary"}
      size={size}
      icon={deleting ? "trash" : "check"}
      kbd={kbd}
      loading={busy === "apply"}
      disabled={!canApply}
      onClick={() => void apply()}
      className="plan-apply"
      aria-describedby={confirming ? "plan-confirm-why" : undefined}
    >
      {applyWords}
    </Button>
  );
  const undoNote = (
    <span className="dw-sheet-note">
      <Icon name="undo" size={14} />
      {onlyIfNewer(plan) ? (
        <Trans>Undo for {undoDays} days if this replaces anything</Trans>
      ) : (
        <Trans>Undo for {undoDays} days</Trans>
      )}
    </span>
  );

  let footer: ReactNode;
  if (phase === "planning" || phase === "unplanned" || phase === "nothing") {
    footer = (
      <Button variant="secondary" size={size} onClick={close} ref={primary}>
        <Trans>Close</Trans>
      </Button>
    );
  } else if (phase === "review") {
    footer = confirming ? (
      <div className="plan-gate">
        <TextField
          ref={confirmField}
          className="plan-gate-field"
          label={
            <span id="plan-confirm-why">
              <Trans>
                Type <code>{confirmWith}</code> to apply it
              </Trans>
            </span>
          }
          mono
          autoComplete="off"
          spellCheck={false}
          value={typed}
          invalid={typed.trim().length >= confirmWith.length && !confirmed}
          onChange={(event) => setTyped(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !event.metaKey && !event.ctrlKey && !event.nativeEvent.isComposing) {
              event.preventDefault();
              void apply();
            }
          }}
        />
        <div className="plan-gate-note">{undoNote}</div>
        <Button variant="ghost" size={size} onClick={close} className="plan-gate-cancel">
          <Trans>Cancel</Trans>
        </Button>
        {applyButton}
      </div>
    ) : (
      <>
        {offline.length > 0 ? (
          <span className="dw-sheet-note">
            <Trans>{offline.join(", ")} has to reconnect first</Trans>
          </span>
        ) : (
          undoNote
        )}
        <Button variant="ghost" size={size} onClick={close}>
          <Trans>Cancel</Trans>
        </Button>
        {applyButton}
      </>
    );
  } else if (phase === "applying") {
    footer = (
      <>
        <span className="dw-sheet-note">
          <Trans>It keeps applying if you close this.</Trans>
        </span>
        <Button variant="ghost" size={size} onClick={close}>
          <Trans>Close</Trans>
        </Button>
      </>
    );
  } else {
    const deadline = change?.undoDeadline ? formatDay(timestampDate(change.undoDeadline), i18n.locale, now) : "";
    // Going back to the form can't help a server that is away, and Done says it is out of your hands.
    const offlineNow = wentOffline(change);
    const retry = phase === "failed" && !offlineNow ? opening.retryLabel : undefined;
    const done = phase === "arrived" ? opening.doneLabel : undefined;
    footer = (
      <>
        {undoable && change ? (
          <Button
            variant="ghost"
            size={size}
            icon="undo"
            onClick={() => open({ kind: "undo", id: change.id })}
            className="plan-undo"
          >
            <Trans>Undo until {deadline}</Trans>
          </Button>
        ) : null}
        {retry ? (
          <Button variant="ghost" size={size} onClick={leave}>
            <Trans>Done</Trans>
          </Button>
        ) : null}
        <Button
          ref={primary}
          variant={retry || phase === "arrived" || offlineNow ? "primary" : "secondary"}
          size={size}
          kbd={kbd}
          onClick={retry ? close : leave}
        >
          {retry ?? done ?? <Trans>Done</Trans>}
        </Button>
      </>
    );
  }

  return (
    <SheetView
      title={
        <TitleWithState phase={phase} live={goesLive(plan).length > 0}>
          <MachineWords>{title}</MachineWords>
        </TitleWithState>
      }
      subtitle={subtitle ? <MachineWords names={names}>{subtitle}</MachineWords> : undefined}
      onClose={leave}
      footer={footer}
      headingRef={heading}
      className={cx("plan-view", phone && "is-phone")}
    >
      {/* Says where it stands to a screen reader, as a title that changes doesn't. A failure says so itself. */}
      <span className="dw-sr" role="status">
        {phase === "applying" || phase === "arrived" ? title : ""}
      </span>
      <PlanBody
        key={phase === "planning" ? phase : "plan"}
        phase={phase}
        phone={phone}
        opening={opening}
        change={change}
        plan={plan}
        failure={failure}
        offline={offline}
        rows={rows}
        onRollbackSettled={readAgain}
        witnessed={witnessed}
      />
    </SheetView>
  );
}

/**
 * Where it stands, as a shape and a colour beside the word the title says,
 * while it runs and once something is live. A plan to review has nothing to
 * say yet, a failure has its Callout, and what is gone isn't healthy.
 */
function TitleWithState({ phase, live, children }: { phase: Phase; live: boolean; children: ReactNode }) {
  if (phase !== "applying" && !(phase === "arrived" && live)) return <>{children}</>;
  const applying = phase === "applying";
  return (
    <span className="plan-title">
      <span className={`plan-title-mark dw-tone-${applying ? "cobalt" : "healthy"}`}>
        <Glyph shape={applying ? "spinner" : "circle"} size={applying ? 14 : 10} />
      </span>
      {children}
    </span>
  );
}

/** The sheet's title, in words that say what happened, once something has, and what it was for when the plan says. */
function useTitle(phase: Phase, plan: Plan | undefined, change: Change | undefined): string {
  const { t } = useLingui();
  const words = useChangeWords();
  const intent = intentOf(plan);
  switch (phase) {
    case "planning":
      return t`Planning`;
    case "unplanned":
      return t`The plan didn't work`;
    case "nothing":
      return t`Nothing to change`;
    case "review":
      return t`Review plan`;
    case "applying":
      return intent ? words.intent(intent, "doing") : t`Applying`;
    case "failed": {
      if (wentOffline(change)) {
        const failed = failedStep(change);
        const server = plan?.servers.find((s) => s.serverId === failed?.serverId)?.serverName ?? "";
        if (intent?.kind === IntentKind.DEPLOY) return t`${server} went offline during this deploy`;
        if (intent?.kind === IntentKind.ROLL_BACK) return t`${server} went offline during this rollback`;
        return t`${server} went offline during this change`;
      }
      if (change?.rollback) return t`Rolling back failed`;
      if (intent) return words.intent(intent, "failed");
      const made = madeProject(plan);
      if (made) return t`Couldn't create ${made}`;
      const all = changing(plan);
      const one = all.length === 1 ? all[0] : undefined;
      if (one) {
        const name = one.resource?.name ?? "";
        const verb = words.verb(one.action, "failed");
        return t`${verb} ${name}`;
      }
      return t`The change failed`;
    }
    case "discarded":
      return t`Discarded`;
    case "history":
      if (change?.undoneBy) return t`Undone`;
      if (foundNothingNewer(change)) {
        const same = changing(plan)[0]?.resource?.name ?? "";
        return t`${same} stays as it is`;
      }
      return intent ? words.intent(intent, "done") : t`Applied`;
  }
  // A pull that found nothing newer changed nothing, so nothing is newly live.
  if (foundNothingNewer(change)) {
    const same = changing(plan)[0]?.resource?.name ?? "";
    return t`${same} stays as it is`;
  }
  // A deploy is live, as a new app is. A port or a rollback says what it did, as its button promised.
  if (intent && intent.kind !== IntentKind.DEPLOY) return words.intent(intent, "done");
  const live = goesLive(plan);
  const name = live[0]?.resource?.name ?? "";
  if (live.length === 1) return t`${name} is live`;
  const made = madeProject(plan);
  if (live.length > 1 && made) return t`${made} is live`;
  if (live.length > 1) return plural(live.length, { one: "# app is live", other: "# apps are live" });
  const all = changing(plan);
  const only = all[0];
  const subject = only?.resource?.name ?? "";
  if (all.length > 1) {
    return onlyDeletes(plan)
      ? plural(all.length, { one: "# resource is deleted", other: "# resources are deleted" })
      : t`Applied`;
  }
  switch (only?.action) {
    case PlanAction.CREATE:
      return t`${subject} is created`;
    case PlanAction.UPDATE:
      return t`${subject} is changed`;
    case PlanAction.REPLACE:
      return t`${subject} is replaced`;
    case PlanAction.DELETE:
      return t`${subject} is deleted`;
  }
  return t`Applied`;
}

function PlanBody({
  phase,
  phone,
  opening,
  change,
  plan,
  failure,
  offline,
  rows,
  onRollbackSettled,
  witnessed,
}: {
  phase: Phase;
  phone: boolean;
  opening: Opening;
  change: Change | undefined;
  plan: Plan | undefined;
  failure: unknown;
  offline: readonly string[];
  rows: ReturnType<typeof useFieldRows>;
  onRollbackSettled: () => void;
  /** This sheet watched it happen, so what it says of what runs now is so. */
  witnessed: boolean;
}) {
  const { t } = useLingui();
  const words = useChangeWords();
  const alsoWords = useAlsoWords();
  const risks = useRisks(plan);
  const warnings = useWarnings(plan, phase !== "review");
  const asides = useAsides(plan);
  const fleet = isFleet(plan);

  // Read from the review on, so an arrival has them as it lands.
  useDomainsOfPlan(plan);
  if (phase === "planning") {
    return (
      <div className="plan-body">
        <p className="plan-loading" aria-busy="true">
          <Glyph shape="spinner" size={14} />
          <Trans>Planning: asking each server what this would take</Trans>
        </p>
      </div>
    );
  }
  if (phase === "unplanned") {
    return (
      <div className="plan-body">
        <Callout tone="critical">{sentence(failure)}</Callout>
      </div>
    );
  }
  if (phase === "nothing") {
    return (
      <div className="plan-body">
        <div className="plan-nothing">
          <Icon name="check" size={20} />
          <p>
            {(opening.kind === "nothing" ? opening.detail : undefined) ?? (
              <Trans>Everything this plan asks for is already as it says, so there is nothing to apply.</Trans>
            )}
          </p>
        </div>
      </div>
    );
  }

  const ran = phase === "applying" || phase === "arrived" || phase === "history";
  const landed = phase === "arrived" || phase === "history";
  const cameToNothing = phase === "failed" || phase === "discarded";
  const fold = foldPlumbing(
    plan,
    diffResources(plan, rows, {
      // Many apps have been read through once: what is left to see once they are live is that they are.
      ...(cameToNothing || (landed && goesLive(plan).length > 1) ? { expanded: false } : {}),
      tag: (intent) => words.intentTag(intent, landed),
    }),
  );
  const steps = change?.steps ?? [];
  // Steps sit under the diff, in the room it leaves, so nothing moves as they appear. With one server they
  // say nothing more once it has landed, and a person reading a plan has the plan.
  const showSteps =
    phase === "applying" ||
    (fleet && (ran || phase === "review")) ||
    (phase === "failed" && (steps.length > 1 || fleet));
  const failed = failedStep(change);
  // Discarded, it did nothing. Failed, it may have done some of this, for a while.
  const setOut = phase === "failed" ? t`What it set out to do` : t`What it would have done`;
  const stepsDone = steps.filter((s) => s.state === StepState.DONE).length;
  const nothingNewer = foundNothingNewer(change);

  return (
    <div className="plan-body">
      {failure ? (
        <Callout tone="critical" title={t`That didn't apply`}>
          <MachineWords>{refusal(failure, plan)}</MachineWords>
        </Callout>
      ) : null}
      {/* What it reaches stays said while it runs and once it has, so nothing moves as it lands. */}
      {phase === "review" || phase === "applying" || phase === "arrived" ? (
        <TopNotice risks={risks} warnings={warnings} offline={phase === "review" ? offline : []} phase={phase} />
      ) : null}
      {phase === "failed" && change ? (
        <>
          {/* First, what runs now, which is what anyone wants to know. Then why. */}
          <RollbackLine
            change={change}
            subject={rollbackSubject(plan)}
            onSettled={onRollbackSettled}
            record={!witnessed}
          />
          {wentOffline(change) ? null : (
            <>
              <FailureNotice
                message={change.error || t`A step failed, so the change stopped there.`}
                detail={failed?.detail}
                fix={opening.fix?.(failed?.reason ?? "")}
              />
              <FailedDeployLogs change={change} />
            </>
          )}
        </>
      ) : null}
      {/* One line that holds its place through the flow: the plan's reach, then where it is reached. */}
      {(phase === "review" && !phone) || phase === "applying" ? (
        <div className="plan-lead">
          <Icon name="activity" size={16} />
          <span>
            <Impact plan={plan} />
          </span>
          {phase === "applying" && steps.length > 1 ? (
            <span className="plan-lead-count">{t`${stepsDone} of ${steps.length} done`}</span>
          ) : null}
        </div>
      ) : null}
      {phase === "arrived" && change && nothingNewer ? (
        <div className="plan-lead">
          <Icon name="check" size={16} />
          <span>
            <MachineWords>{nothingNewer}</MachineWords>
          </span>
        </div>
      ) : null}
      {phase === "arrived" && change && !nothingNewer ? <Arrival plan={plan} change={change} /> : null}
      {phase === "history" && change ? <OnRecord change={change} /> : null}
      {phase !== "discarded" ? <Places plan={plan} /> : null}
      <section className="plan-section" aria-label={cameToNothing ? setOut : t`Plan`}>
        {cameToNothing ? (
          <h3 className="dw-text-label plan-heading">{setOut}</h3>
        ) : phone && phase === "review" ? (
          <h3 className="dw-text-heading plan-heading">
            <Trans>Plan</Trans>
          </h3>
        ) : null}
        <PlanDiff
          resources={fold.shown}
          done={landed}
          noSummary={cameToNothing || nothingNewer !== ""}
          also={
            fold.folded.length > 0
              ? {
                  label: (
                    <MachineWords names={[fold.project, fold.environment]}>{alsoWords(fold, landed)}</MachineWords>
                  ),
                  action: "create",
                  resources: fold.folded,
                }
              : undefined
          }
        />
      </section>
      {showSteps ? <PlanSteps plan={plan} progress={steps} /> : null}
      {phase === "review" ? (
        <Effects
          phone={phone}
          plan={plan}
          risks={risks}
          warnings={warnings}
          offline={offline}
          asides={asides}
          fleet={fleet}
        />
      ) : null}
      {change?.rollback && ran ? (
        <p className="plan-note">
          <Icon name="undo" size={14} />
          <Trans>Dockwell made this change to roll back a failed one.</Trans>
        </p>
      ) : null}
    </div>
  );
}

/**
 * Why a call was refused, in a sentence, with no change's ID in it. A sentence
 * starts with a capital, but a name in the plan keeps its case: "hz-2 isn't
 * connected", never "Hz-2".
 */
function refusal(failure: unknown, plan: Plan | undefined): string {
  const said = withoutIds(sentence(failure));
  const lowered = said.charAt(0).toLowerCase() + said.slice(1);
  const names = [
    ...(plan?.servers ?? []).map((s) => s.serverName),
    ...(plan?.resources ?? []).map((r) => r.resource?.name ?? ""),
  ];
  return names.some((name) => name !== "" && (lowered.startsWith(`${name} `) || lowered.startsWith(`${name}'`)))
    ? lowered
    : said;
}

/**
 * What matters most, at the top of the plan: one Callout, as the brand book
 * asks. What can't be taken back leads, with the worst of it first. Short of
 * that, what is worth a warning: what the plan says about itself, and a server
 * that has to reconnect first.
 */
function TopNotice({
  risks,
  warnings,
  offline,
  phase,
}: {
  risks: readonly Notice[];
  warnings: readonly Notice[];
  offline: readonly string[];
  phase: Phase;
}) {
  const { t } = useLingui();
  const [first, ...rest] = risks;
  if (first) {
    return (
      <Callout tone="critical" title={<MachineWords>{first.title}</MachineWords>} className="plan-risk">
        {first.detail}
        {rest.map((risk) => (
          <p key={risk.key} className="plan-risk-more">
            <b>
              <MachineWords>{risk.title}</MachineWords>.
            </b>{" "}
            {risk.detail}
          </p>
        ))}
        {risks.some((risk) => risk.trust) ? (
          <p className="plan-risk-more">
            {phase === "review" ? (
              <Trans>Apply this only if you trust what it runs.</Trans>
            ) : (
              <Trans>Keep this only if you trust what it runs.</Trans>
            )}
          </p>
        ) : null}
      </Callout>
    );
  }
  const notices: Notice[] = [...warnings];
  if (offline.length > 0) {
    notices.push({
      key: "offline",
      title: t`Not every server is connected`,
      detail: (
        <Trans>
          {offline.join(", ")} can't take its steps until its agent reconnects, so the plan can't apply until then.
        </Trans>
      ),
    });
  }
  const [lead, ...others] = notices;
  if (!lead) return null;
  return (
    <Callout tone="warning" title={<MachineWords>{lead.title}</MachineWords>} className="plan-risk">
      <MachineWords>{lead.detail}</MachineWords>
      {others.map((notice) => (
        <p key={notice.key} className="plan-risk-more">
          <b>
            <MachineWords>{notice.title}</MachineWords>.
          </b>{" "}
          <MachineWords>{notice.detail}</MachineWords>
        </p>
      ))}
    </Callout>
  );
}

/** Which project and environment a plan lands in, once there is a choice of either, so it can't read as another's. */
function Places({ plan }: { plan: Plan | undefined }) {
  // A project this plan brings to life is said where it is made.
  const made = madeProject(plan);
  const places = (plan?.places ?? []).filter((p) => p.environment !== "" || p.project !== made);
  if (places.length === 0) return null;
  return (
    <ul className="plan-places">
      {places.map(({ project, environment }) => (
        <li key={`${project}/${environment}`} className="plan-lead">
          <Icon name="folder" size={16} />
          <span>
            {project && environment ? (
              <Trans>
                In project <code>{project}</code>, environment <code>{environment}</code>
              </Trans>
            ) : project ? (
              <Trans>
                In project <code>{project}</code>
              </Trans>
            ) : (
              <Trans>
                In environment <code>{environment}</code>
              </Trans>
            )}
          </span>
        </li>
      ))}
    </ul>
  );
}

/** The plan's reach in one line: which servers, and what it does to what they run. */
function Impact({ plan }: { plan: Plan | undefined }) {
  const time = useTimeWords();
  const { i18n } = useLingui();
  const now = useControlPlaneNow(30_000);
  const servers = (plan?.servers ?? []).filter((s) => !s.removed);
  if (servers.length === 0) return <Trans>No server · nothing goes down</Trans>;
  const where =
    servers.length <= 2 ? (
      servers.map((s, i) => (
        <span key={s.serverId}>
          {i > 0 ? ", " : null}
          <code>{s.serverName}</code>
        </span>
      ))
    ) : (
      <span title={servers.map((s) => s.serverName).join(", ")}>
        {plural(servers.length, { one: "# server", other: "# servers" })}
      </span>
    );
  const away = goesAway(plan);
  const names = away.map((r) => r.resource?.name ?? r.id);
  const name = names[0] ?? "";
  const down = longestDowntime(servers);
  const span = time.span(down);
  const healthy = longestUntilHealthy(servers);
  const bound = time.bound(healthy);
  const ifNewer = servers
    .flatMap((s) => s.steps)
    .filter((s) => s.downtime)
    .every(replacesIfNewer);
  const running =
    down === 0 ? (
      <Trans>no downtime expected</Trans>
    ) : ifNewer ? (
      <Trans>about {span} down, if there is a newer image</Trans>
    ) : healthy > down ? (
      <Trans>
        about {span} down, and up to {bound} until healthy
      </Trans>
    ) : (
      <Trans>about {span} down</Trans>
    );
  // What stops, what stays, and until when it can come back, once, and in agreement with the footer.
  const windowMs = Number(plan?.undoWindow?.seconds ?? 0n) * 1000;
  const deadline = formatDay(new Date(now + windowMs), i18n.locale, now);
  const gone = servers.flatMap((s) => s.steps).some((s) => s.destroysData);
  const undoable = windowMs > 0 && !gone;
  const stays = [
    ...new Set(
      away.flatMap((r) =>
        r.changes.filter((c) => /(^|\.)mounts\[[^\]]*\]\.volume$/.test(c.path) && c.from !== "").map((c) => c.from),
      ),
    ),
  ].join(", ");
  const stops =
    away.length === 1 ? (
      !undoable ? (
        <Trans>
          <code>{name}</code> stops.
        </Trans>
      ) : stays ? (
        <Trans>
          <code>{name}</code> stops. <code>{stays}</code> stays, and you can undo this until {deadline}.
        </Trans>
      ) : (
        <Trans>
          <code>{name}</code> stops, and you can undo this until {deadline}.
        </Trans>
      )
    ) : away.length > 1 ? (
      undoable ? (
        plural(away.length, {
          one: `# app stops, and you can undo this until ${deadline}.`,
          other: `# apps stop, and you can undo this until ${deadline}.`,
        })
      ) : (
        plural(away.length, { one: "# app stops.", other: "# apps stop." })
      )
    ) : null;
  const others = changing(plan).length > away.length;
  return (
    <>
      {where} ·{" "}
      {stops ? (
        others && down > 0 ? (
          <>
            {running} · {stops}
          </>
        ) : (
          stops
        )
      ) : (
        running
      )}
    </>
  );
}

/** The arrival's first line: where it is reached, or that nothing can reach it yet, and what to do about that. */
function Arrival({ plan, change }: { plan: Plan | undefined; change: Change }) {
  const { t } = useLingui();
  const services = useServices().data;
  const { servers } = useFleetView();
  const live = goesLive(plan).filter((r) => r.resource?.kind === "service");
  const reaching = useDomainsOfPlan(plan);
  // A new app with no port among its fields publishes none, which the plan says at once, before the list is read again.
  const unpublished = unreached(change);
  const known = live.map((r) => services?.find((s) => s.id === r.id));
  const nowhere =
    live.length > 0 &&
    live.every((r) => r.action === PlanAction.CREATE && unpublished.includes(r.resource?.name ?? ""));
  const domains = reaching.domains;
  if (reaching.isLoading || (live.length === 0 && domains.length === 0)) return <TookLine change={change} />;
  if (domains.length === 0 && !nowhere && known.some((s) => !s)) return <TookLine change={change} />;
  const rows = known.flatMap((service) => {
    if (!service) return [];
    const ports = appOf(service)?.ports ?? [];
    const addresses = service.deployments.map(
      (d) => servers?.find((s) => s.id === d.serverId)?.address ?? d.serverName,
    );
    return addresses.flatMap((address) =>
      ports.map((port) => {
        const local = port.hostIp === "127.0.0.1" || port.hostIp === "::1";
        const host = port.hostIp && port.hostIp !== "0.0.0.0" && port.hostIp !== "::" ? port.hostIp : address;
        const at = `${host.includes(":") ? `[${host}]` : host}:${port.hostPort}`;
        return { at, local, target: port.containerPort, udp: port.protocol === 2 };
      }),
    );
  });
  if (domains.length === 0 && (nowhere || rows.length === 0)) {
    const name = live[0]?.resource?.name ?? "";
    const server = plan?.servers[0]?.serverName ?? "";
    return (
      <div className="plan-lead is-next">
        <Icon name="globe" size={16} />
        <span>
          {live.length === 1 ? (
            <Trans>
              Nothing outside <code>{server}</code> reaches <code>{name}</code> yet. Add a port or a domain.
            </Trans>
          ) : (
            <Trans>Nothing outside the server reaches these yet. Add a port or a domain to each.</Trans>
          )}
        </span>
      </div>
    );
  }
  return (
    <ul className="plan-reach">
      {domains.map((domain) => (
        <DomainReach key={domain.name} domain={domain} />
      ))}
      {rows.map(({ at, local, target, udp }) => (
        <li key={at} className="plan-reach-row">
          <Icon name="globe" size={16} />
          {local || udp ? (
            <code className="plan-reach-at">{at}</code>
          ) : (
            <a className="plan-reach-at" href={`http://${at}`} target="_blank" rel="noreferrer">
              <code>{at}</code>
              <Icon name="external" size={14} />
            </a>
          )}
          {local ? <span className="plan-reach-how">{t`only on the server`}</span> : null}
          {udp ? <span className="plan-reach-how">{t`port ${target}, UDP`}</span> : null}
          <CopyButton text={at} label={t`Copy ${at}`} />
        </li>
      ))}
    </ul>
  );
}

/**
 * The domains a plan's arrival leads with: a domain is how people reach what it sends
 * to, ahead of any port, and a change can be nothing but a route, which says where it
 * went. The sheet reads them from the review on, so an arrival has them as it lands,
 * and nothing moves when it does.
 */
function useDomainsOfPlan(plan: Plan | undefined) {
  const live = goesLive(plan).filter((r) => r.resource?.kind === "service");
  const routes = (plan?.resources ?? []).filter((r) => r.resource?.kind === "route" && r.action !== PlanAction.DELETE);
  const environment = [...live, ...routes][0]?.resource?.scope ?? "";
  const named = (resources: readonly PlannedResource[]) =>
    resources.filter((r) => r.resource?.scope === environment).map((r) => r.resource?.name ?? "");
  return useDomainsReaching(environment, named(live), named(routes));
}

/**
 * A domain in an arrival: open once it serves, and until then what the server says
 * of its certificate. Behind a tunnel or a proxy it's the name alone: what this server
 * serves is http://, and how people reach it is for what's in front to say, which may
 * well be HTTPS, so it's never written or copied as the address.
 */
function DomainReach({ domain }: { domain: Domain }) {
  const { t } = useLingui();
  const look = lookOf(domain);
  const name = domain.name;
  const serving = look.state === "serving";
  return (
    <li className={cx("plan-reach-row", "is-domain", look.state === "failed" && "is-failed")}>
      <Icon name="globe" size={16} />
      {serving ? (
        <a className="plan-reach-at" href={domain.url} target="_blank" rel="noreferrer">
          <code>{name}</code>
          <Icon name="external" size={14} />
        </a>
      ) : (
        <code className="plan-reach-at">{name}</code>
      )}
      {look.noCertificate ? (
        <span className="plan-reach-how">{t`TLS ends before this server`}</span>
      ) : serving ? null : (
        <span className="plan-reach-how">{look.message || t`Getting its certificate`}</span>
      )}
      {serving ? <CopyButton text={look.noCertificate ? name : domain.url} label={t`Copy ${name}`} /> : null}
    </li>
  );
}

/** What is left to say for what has no address to lead with: what stopped, or how long it took. */
function TookLine({ change }: { change: Change }) {
  const time = useTimeWords();
  const servers = (change.plan?.servers ?? []).map((s) => s.serverName);
  const names = servers.join(", ");
  const stopped = goesAway(change.plan);
  const name = stopped[0]?.resource?.name ?? "";
  const spent = change.steps.reduce((all, step) => {
    if (!step.startTime || !step.finishTime) return all;
    return (
      all +
      (Number(step.finishTime.seconds - step.startTime.seconds) * 1000 +
        (step.finishTime.nanos - step.startTime.nanos) / 1e6)
    );
  }, 0);
  const took = time.bound(spent);
  return (
    <div className="plan-lead">
      <Icon name={stopped.length > 0 ? "activity" : "check"} size={16} />
      <span>
        {change.steps.length === 0 ? (
          <Trans>Recorded and applied. Nothing had to run on a server.</Trans>
        ) : stopped.length === 1 && servers.length === 1 ? (
          <Trans>
            <code>{name}</code> has stopped on <code>{names}</code>
          </Trans>
        ) : servers.length === 1 ? (
          <Trans>
            Took {took} on <code>{names}</code>
          </Trans>
        ) : (
          <Trans>Took {took}</Trans>
        )}
      </span>
    </div>
  );
}

/** What a change's record says, for one that applied a while ago. */
function OnRecord({ change }: { change: Change }) {
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const by = change.appliedBy?.name ?? "";
  const when = change.applyTime ? time.ago(timestampDate(change.applyTime), now) : "";
  return (
    <div className="plan-lead">
      <Icon name={change.undoneBy ? "undo" : "check"} size={16} />
      <span>
        {change.undoneBy ? (
          <Trans>
            Applied {when} by {by}, and undone since.
          </Trans>
        ) : (
          <Trans>
            Applied {when} by {by}.
          </Trans>
        )}
      </span>
    </div>
  );
}

/**
 * What applying means beyond the diff, as M-Plan-Approve has it: a row each,
 * and only what there is to say. On a phone the plan's reach leads them, as
 * the diff carries it on wide screens. How a fleet's steps run matters only
 * once there is a fleet (PRD principle 4). A warning goes here when something
 * worse has the Callout. What is only worth a mention stays out of any panel.
 */
function Effects({
  phone,
  plan,
  risks,
  warnings,
  offline,
  asides,
  fleet,
}: {
  phone: boolean;
  plan: Plan | undefined;
  risks: readonly Notice[];
  warnings: readonly Notice[];
  offline: readonly string[];
  asides: readonly Notice[];
  fleet: boolean;
}) {
  const { t } = useLingui();
  const rows: { icon?: IconName; warn?: boolean; title: ReactNode; detail?: ReactNode; key: string }[] = [];
  if (phone) rows.push({ key: "reach", icon: "activity", title: <Impact plan={plan} /> });
  // What is there today: a step that fails puts back what ran, as ADR 0017 has it. Waves and their checks are #28.
  const serving = changing(plan).filter((r) => r.resource?.kind === "service" && r.action !== PlanAction.DELETE);
  const only = serving.length === 1 ? serving[0] : undefined;
  if (only && only.baseRevision > 0n) {
    const back = `r${only.baseRevision}`;
    rows.push({
      key: "safety",
      icon: "undo",
      title: (
        <Trans>
          Rolls back to <code>{back}</code> if it fails
        </Trans>
      ),
    });
  } else if (serving.some((r) => r.baseRevision > 0n)) {
    rows.push({
      key: "safety",
      icon: "undo",
      title: <Trans>Each app rolls back to what it runs now if this fails</Trans>,
    });
  }
  if (fleet) {
    rows.push({
      key: "how",
      icon: "layers",
      title: <Trans>Steps run one at a time on each server, servers side by side.</Trans>,
    });
  }
  // What the Callout has no room for, as something worse has it.
  if (risks.length > 0) {
    for (const warning of warnings) rows.push({ ...warning, warn: true });
  }
  if (risks.length > 0 && offline.length > 0) {
    rows.push({
      key: "offline",
      warn: true,
      title: t`Not every server is connected`,
      detail: (
        <Trans>
          {offline.join(", ")} can't take its steps until its agent reconnects, so the plan can't apply until then.
        </Trans>
      ),
    });
  }
  return (
    <>
      {asides.map((aside) => (
        <p key={aside.key} className={cx("plan-note", aside.notice && "plan-step-notice")}>
          {aside.title}
        </p>
      ))}
      {rows.length > 0 ? (
        <section className="plan-effects" aria-label={t`What applying means`}>
          {rows.map((row) => (
            <div key={row.key} className="plan-effect">
              {row.warn ? (
                <span className="plan-effect-warn">
                  <Glyph shape="triangle" size={12} />
                </span>
              ) : row.icon ? (
                <Icon name={row.icon} size={16} />
              ) : null}
              <div className="plan-effect-text">
                <span className="plan-effect-title">{row.title}</span>
                {row.detail ? <span className="plan-effect-detail">{row.detail}</span> : null}
              </div>
            </div>
          ))}
        </section>
      ) : null}
    </>
  );
}
