// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type Client, createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ActorKind,
  AlertService,
  BackupService,
  CatalogService,
  type Change,
  type ChangeConfirmation,
  type ChangeElsewhere,
  ChangeService,
  ChangeState,
  Connection,
  DatabaseService,
  type Domain,
  type DomainRoute,
  DomainService,
  type EditSchema,
  EventService,
  FieldOp,
  ImageService,
  IntentKind,
  type Plan,
  PlanAction,
  type PlannedResource,
  ResourceService,
  StepState,
  StepUpAction,
  SystemServiceAction,
  TaskService,
  VariableService,
  WebsiteService,
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
  StatusBadge,
  Switch,
  TextField,
  useMediaQuery,
  useSheetViewAway,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useRouter } from "@tanstack/react-router";
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatDay } from "../../lib/format";
import { onApple } from "../../lib/platform";
import { useTimeWords } from "../../lib/time";
import { PasskeyError } from "../../lib/webauthn";
import { appOf, unreached } from "../apps/app";
import { DomainFacts, DomainNote, useSilent } from "../apps/DomainNote";
import {
  type EnvironmentReach,
  lookOf,
  routeWhere,
  TUNNEL_GUIDE,
  useDomainsReaching,
  visitorUrl,
} from "../apps/domains";
import { readHealthSoon, useHealth } from "../apps/health";
import { readServicesAgain, useServices } from "../apps/useServices";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedInUser } from "../auth/useAuthState";
import { FailedDeployLogs } from "../logs/DeployLogLinks";
import { CopyButton } from "../servers/CopyButton";
import { PrivateTag } from "../servers/ServerAddress";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { useWebsiteOfEnvironment } from "../websites/website";
import { ConfirmAccess } from "./ConfirmAccess";
import { BecauseNotice, FailureNotice, MachineWords } from "./FailureNotice";
import { useFieldRows } from "./fields";
import { followChange } from "./follow";
import { type Notice, needsConfirming, needsPasskey, useAsides, useDataLeft, useRisks, useWarnings } from "./notices";
import { panelName } from "./PanelPlan";
import { PlanSteps } from "./PlanSteps";
import {
  canUndo,
  changeTo,
  changing,
  diffResources,
  failedStep,
  foldPlumbing,
  foundNothingNewer,
  goesAway,
  goesLive,
  installs,
  intentOf,
  isEmpty,
  isFinal,
  isFleet,
  labelsOnly,
  longestDowntime,
  longestUntilHealthy,
  losesData,
  madeProject,
  onlyDeletes,
  onlyIfNewer,
  panelOf,
  panelStartsAgain,
  reboots,
  replacesIfNewer,
  restarts,
  routeDomain,
  routeWhereOf,
  shortFingerprint,
  shownName,
  startsStopped,
  unitOf,
  unitWithoutUndo,
  wentOffline,
  withoutIds,
} from "./plan";
import { RollbackLine, rollbackSubject } from "./RollbackLine";
import { UpgradeLine, UpgradePhases } from "./UpgradeLead";
import { upgradeOf, upgradePhases } from "./upgrade";
import { useAlsoWords, useChangeWords } from "./words";
import "./ChangeSheet.css";
import { useActorWords } from "../activity/actor";
import { placesOfChange, useMay } from "../auth/may";

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
  /**
   * A choice of what the plan takes, as a switch above it, such as deleting an app's project with the
   * app (ADR 0074). Flipping it plans the other opening in this one's place, and lets go of this plan.
   */
  choice?: PlanChoice;
}

/** A switch above a plan, between it and another plan, which `flip` opens. */
export interface PlanChoice {
  label: string;
  on: boolean;
  /** What it does, in a line, for the state it's in. */
  description?: string;
  /** Why it can't be flipped, for someone whose role doesn't cover the other plan (ADR 0059). It says so in place of the description. */
  off?: string;
  flip: () => Opening;
  /** Says the plan has no use for it, so the switch goes: a database's move takes its dump whatever a backup's switch says. */
  moot?: (plan: Plan | undefined) => boolean;
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

/** Reads what a change applied or undone may have changed again: the resources, the services and their domains, the tasks, the registry logins and what they read, the timeline's pages and the changes. */
function refreshAfter(queryClient: QueryClient): void {
  void readServicesAgain(queryClient);
  for (const schema of [
    ResourceService,
    DomainService,
    EventService,
    ChangeService,
    VariableService,
    CatalogService,
    BackupService,
    WebsiteService,
    AlertService,
    TaskService,
    ImageService,
  ]) {
    void queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema, cardinality: undefined }) });
  }
  readHealthSoon(queryClient);
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

/**
 * The change as the newer of two reads says it. A read can arrive after a newer one, as an
 * apply's own answer, read before its step began to wait, does behind the watch that said it
 * waits: a state behind is never taken, and at the same state, a step the other read says
 * waits keeps that, as what a running step waits for goes only as the step moves on, and the
 * server says it once.
 */
function later(was: Change | undefined, next: Change | undefined): Change | undefined {
  if (!next) return was;
  if (!was || was.id !== next.id) return next;
  if (ORDER[was.state] > ORDER[next.state]) return was;
  if (ORDER[was.state] < ORDER[next.state] || was.steps.length !== next.steps.length) return next;
  let kept = false;
  const steps = next.steps.map((step, i) => {
    const before = was.steps[i];
    if (!before?.confirmation || step.confirmation || before.state !== step.state) return step;
    kept = true;
    return { ...step, confirmation: before.confirmation };
  });
  return kept ? { ...next, steps } : next;
}

type Busy = "apply" | "discard" | undefined;

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
  const explain = useAuthError();
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
  // A plan the reader's role doesn't cover waits for an admin or the owner, and
  // someone else's in a protected environment, an admin approves with a
  // passkey, however it was planned: the API says which (ADR 0049).
  const user = useSignedInUser();
  // Undoing plans a change, which a viewer can't (ADR 0059). Applying and discarding are the plan's to say, below.
  const mayUndo = useMay()(ChangeService.method.undoChange, placesOfChange(change));
  const waits = phase === "review" && (change?.waits ?? "") !== "";
  const approves = phase === "review" && change?.approves === true;
  // Theirs to discard, as the sheet's own plan is: one they planned in the panel.
  const theirs =
    proposal.current !== undefined ||
    (change?.proposedBy?.kind === ActorKind.USER && change.proposedBy.id === user?.id);

  const apply = useCallback(async () => {
    if (!change || !canApply || busy) return;
    setBusy("apply");
    setFailure(undefined);
    // Closing the sheet while it applies leaves it to apply.
    const held = proposal.current;
    if (held) held.keep = true;
    try {
      const fingerprint = change.plan?.fingerprint ?? "";
      const res = await withStepUpIfAsked(transport, StepUpAction.APPLY_ROOT_CHANGE, change.id, (proof) =>
        client.applyChange({ id: change.id, fingerprint, proof }),
      );
      setChange((was) => later(was, res.change));
    } catch (err) {
      // Still planned: closing discards it, as before.
      if (held) held.keep = false;
      if (err instanceof PasskeyError && err.problem === "cancelled") {
        setFailure(
          new Error(
            t`The passkey prompt closed, so nothing was applied and the plan is still planned. Apply it again when you're ready.`,
          ),
        );
      } else {
        setFailure(err instanceof PasskeyError ? new Error(explain(err)) : err);
      }
    } finally {
      setBusy(undefined);
    }
  }, [change, canApply, busy, client, transport, explain, t]);

  // Closing lets go of the plan this sheet made, which discards it unless it applied.
  const close = useCallback(() => onClose(change), [onClose, change]);
  // From a change that failed, Done and × leave, where the way back goes to the form.
  const leave = phase === "failed" && opening.onDone ? opening.onDone : close;

  // ⌘↵ (Ctrl+Enter elsewhere) goes on through the flow, as each button says:
  // it applies the plan, and once that is over, takes the way on. A plan that
  // waits for an admin offers no Apply, so it takes what the footer offers. A
  // key held down from the step before doesn't count, so it never applies a
  // plan unseen.
  const over = phase === "arrived" || phase === "failed" || phase === "history" || phase === "discarded";
  useEffect(() => {
    if (away || (phase !== "review" && !over)) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Enter" || !(event.metaKey || event.ctrlKey) || event.isComposing || event.repeat) return;
      event.preventDefault();
      if (phase === "review" && !waits) void apply();
      else primary.current?.click();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [away, phase, over, waits, apply]);

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
      : change?.elsewhere?.editsElsewhere
        ? t`A change in a project you can't see`
        : (change?.summary ?? (opening.kind === "propose" ? opening.summary : undefined));
  const title = useTitle(phase, plan, change);
  const subject = shownName(changing(plan)[0]);
  const names = [
    ...changing(plan).map((r) => r.resource?.name ?? ""),
    ...changing(plan).map((r) => shownName(r)),
    ...(plan?.servers ?? []).map((s) => s.serverName),
  ];
  const serverName = plan?.removes?.serverName;
  const applyWords = serverName
    ? t`Remove ${serverName}`
    : deleting
      ? changing(plan).length === 1
        ? t`Delete ${subject}`
        : plural(changing(plan).length, { one: "Delete # resource", other: "Delete # resources" })
      : t`Apply plan`;

  const applyButton = (
    <Button
      variant={deleting || confirming ? "danger" : "primary"}
      size={size}
      icon={deleting || serverName ? "trash" : "check"}
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
      {restarts(plan) ||
      installs(plan) ||
      reboots(plan) ||
      unitWithoutUndo(plan) ||
      plan?.removes ||
      panelStartsAgain(plan) ? (
        <Trans>No undo</Trans>
      ) : onlyIfNewer(plan) ? (
        <Trans>Undo for {undoDays} days if this replaces anything</Trans>
      ) : (
        <Trans>Undo for {undoDays} days</Trans>
      )}
    </span>
  );

  // Leaving a plan that waits keeps it planned, for an admin to find on Activity.
  const leaveForAdmin = useCallback(() => {
    const held = proposal.current;
    if (held) held.keep = true;
    close();
  }, [close]);

  // Discarding a plan that waits: the sheet's own goes as it closes; one
  // reopened from Activity is discarded here, and the sheet says it was.
  const discard = useCallback(async () => {
    if (proposal.current || !change) {
      close();
      return;
    }
    setBusy("discard");
    setFailure(undefined);
    try {
      const res = await client.discardChange({ id: change.id });
      setChange((was) => later(was, res.change));
    } catch (err) {
      setFailure(err);
    } finally {
      setBusy(undefined);
    }
  }, [change, client, close]);

  let footer: ReactNode;
  if (waits) {
    footer = (
      <>
        <span className="dw-sheet-note">
          <Icon name="shield" size={14} />
          <Trans>An admin or the owner applies this</Trans>
        </span>
        {theirs ? (
          <Button variant="ghost" size={size} onClick={() => void discard()} loading={busy === "discard"}>
            <Trans>Discard</Trans>
          </Button>
        ) : null}
        <Button variant="primary" size={size} onClick={leaveForAdmin} ref={primary}>
          <Trans>Leave it for an admin</Trans>
        </Button>
      </>
    );
  } else if (phase === "planning" || phase === "unplanned" || phase === "nothing") {
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
        <div className="plan-gate-note">
          {undoNote}
          {needsPasskey(plan) || approves ? (
            <span className="dw-sheet-note">
              <Icon name="key" size={14} />
              <Trans>Applying this asks for your passkey</Trans>
            </span>
          ) : null}
        </div>
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
        ) : approves && change?.proposedBy?.kind === ActorKind.TOKEN ? (
          <span className="dw-sheet-note">
            <Icon name="key" size={14} />
            <Trans>
              Applying this approves a change planned with the API token {change.proposedBy.name}, with your passkey
            </Trans>
          </span>
        ) : approves ? (
          <span className="dw-sheet-note">
            <Icon name="key" size={14} />
            <Trans>Applying this approves {change?.proposedBy?.name}'s change, with your passkey</Trans>
          </span>
        ) : needsPasskey(plan) ? (
          // Root's to apply with nothing to type, as an install that restarts nothing (ADR 0064): the passkey
          // is said beside what can't be undone.
          <span className="dw-sheet-note plan-foot-notes">
            {undoNote}
            <span className="dw-sheet-note">
              <Icon name="key" size={14} />
              <Trans>Applying this asks for your passkey</Trans>
            </span>
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
          {confirmationOf(change) ? (
            <Trans>If you close this without keeping it, the change goes back on its own.</Trans>
          ) : (
            <Trans>It keeps applying if you close this.</Trans>
          )}
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
            {...mayUndo.off}
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
      {opening.choice &&
      !opening.choice.moot?.(plan) &&
      (phase === "planning" || phase === "review" || phase === "unplanned") ? (
        <PlanChoiceSwitch choice={opening.choice} onFlip={() => open(opening.choice?.flip() ?? opening)} />
      ) : null}
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
        onLeave={leave}
      />
    </SheetView>
  );
}

/** The switch a choice is, above the plan, which stays where it is while the other plan is made. */
function PlanChoiceSwitch({ choice, onFlip }: { choice: PlanChoice; onFlip: () => void }) {
  return (
    <div className="plan-choice">
      <Switch
        checked={choice.on}
        onChange={onFlip}
        label={choice.label}
        description={choice.off ?? choice.description}
        disabled={choice.off !== undefined}
      />
    </div>
  );
}

/** A website as the plan sheet needs it: the environment's website, if it is one. */
type WebsiteOf = ((environmentId: string) => { id: string; domain: string; name: string } | undefined) | undefined;

/**
 * The website every one of these services is its own of, by its domain, when they're all one
 * website's (ADR 0039), or undefined. A website's services are the website's, never apps.
 */
function websiteOfAll(services: readonly PlannedResource[], websiteOf: WebsiteOf): string | undefined {
  if (!websiteOf || services.length === 0) return undefined;
  const sites = services.map((s) => websiteOf(s.resource?.scope ?? ""));
  const first = sites[0];
  if (!first || sites.some((w) => w?.id !== first.id)) return undefined;
  return first.domain || first.name;
}

/** The services that are apps: those that aren't a website's own. */
function notWebsites(services: readonly PlannedResource[], websiteOf: WebsiteOf): PlannedResource[] {
  return websiteOf ? services.filter((s) => !websiteOf(s.resource?.scope ?? "")) : [...services];
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
      <span className="plan-title-words">{children}</span>
    </span>
  );
}

/** The sheet's title, in words that say what happened, once something has, and what it was for when the plan says. */
/** When a change's last step ended, as an agent's report from before then says nothing of what it made. */
function landedAt(change: Change | undefined): Date | undefined {
  let newest: Date | undefined;
  for (const step of change?.steps ?? []) {
    const at = step.finishTime ? timestampDate(step.finishTime) : undefined;
    if (at && (!newest || at > newest)) newest = at;
  }
  return newest;
}

function useTitle(phase: Phase, plan: Plan | undefined, change: Change | undefined): string {
  const { t, i18n } = useLingui();
  const words = useChangeWords();
  const intent = intentOf(plan);
  const health = useHealth();
  // Only what runs can be a website's; a plan that runs nothing, such as a reboot, reads no websites.
  const websiteOf = useWebsiteOfEnvironment(goesLive(plan).length > 0);
  // A change of labels alone says it of the servers, which take no step (ADR 0091).
  const labelled = labelsOnly(plan);
  if (labelled.length > 0) {
    const on = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(labelled);
    if (phase === "applying") return t`Labelling ${on}`;
    if (phase === "failed") return t`Labelling ${on} failed`;
    if (phase === "history" || phase === "arrived") return t`Labels are changed on ${on}`;
  }
  // A reboot says what it does, and that the server came back.
  if (reboots(plan)) {
    const server = plan?.servers.find((s) => s.steps.length > 0)?.serverName ?? "";
    if (phase === "applying") return t`Rebooting ${server}`;
    if (phase === "failed") return t`Rebooting ${server} failed`;
    if (phase === "history" || phase === "arrived") return t`${server} is back after its reboot`;
  }
  // An action on a system service says what it does to the unit (ADR 0082).
  const unit = unitOf(plan);
  if (unit) {
    const server = plan?.servers.find((s) => s.steps.length > 0)?.serverName ?? "";
    const name = unit.name;
    const doing: Record<SystemServiceAction, [string, string]> = {
      [SystemServiceAction.UNSPECIFIED]: [t`Acting on ${name} on ${server}`, t`Acted on ${name} on ${server}`],
      [SystemServiceAction.START]: [t`Starting ${name} on ${server}`, t`Started ${name} on ${server}`],
      [SystemServiceAction.STOP]: [t`Stopping ${name} on ${server}`, t`Stopped ${name} on ${server}`],
      [SystemServiceAction.RESTART]: [t`Restarting ${name} on ${server}`, t`Restarted ${name} on ${server}`],
      [SystemServiceAction.RELOAD]: [t`Reloading ${name} on ${server}`, t`Reloaded ${name} on ${server}`],
      [SystemServiceAction.ENABLE]: [t`Enabling ${name} at boot on ${server}`, t`${name} starts at boot on ${server}`],
      [SystemServiceAction.DISABLE]: [
        t`Disabling ${name} at boot on ${server}`,
        t`${name} no longer starts at boot on ${server}`,
      ],
      [SystemServiceAction.MASK]: [t`Masking ${name} on ${server}`, t`Masked ${name} on ${server}`],
      [SystemServiceAction.UNMASK]: [t`Unmasking ${name} on ${server}`, t`Unmasked ${name} on ${server}`],
    };
    const [ing, ed] = doing[unit.action];
    if (phase === "applying") return ing;
    if (phase === "failed") return t`${ing} failed`;
    if (phase === "history" || phase === "arrived") return ed;
  }
  // Stopping the panel an import came from says so, and starting it again (ADR 0089).
  const panel = panelOf(plan);
  // An import that makes apps again says what an import does; its scaling is a step.
  if (panel && !panel.apps) {
    const server = plan?.servers.find((s) => s.steps.length > 0)?.serverName ?? "";
    const name = panelName(panel.kind);
    const [ing, ed] = panel.start
      ? [t`Starting ${name} again on ${server}`, t`${name} runs again on ${server}`]
      : [t`Stopping ${name} on ${server}`, t`${name} is stopped on ${server}`];
    if (phase === "applying") return ing;
    if (phase === "failed") return t`${ing} failed`;
    if (phase === "history" || phase === "arrived") return ed;
  }
  // An install says what it does, not that its server's host is changed.
  if (installs(plan)) {
    const server = plan?.servers.find((s) => s.steps.length > 0)?.serverName ?? "";
    if (phase === "applying") return t`Installing updates on ${server}`;
    if (phase === "failed" && !wentOffline(change)) return t`Installing updates on ${server} failed`;
    if (phase === "history" || phase === "arrived") return t`Updates are installed on ${server}`;
  }
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
        const name = shownName(one);
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
  // A deploy says how what it deployed runs, as its server says, and a new app the same. A
  // port or a rollback says what it did, as its button promised.
  if (intent && intent.kind !== IntentKind.DEPLOY) return words.intent(intent, "done");
  // Taking something on deployed nothing: it runs as it did.
  if (plan?.servers.some((server) => server.steps.some((step) => step.adopts))) {
    const subject = shownName(changing(plan)[0]);
    return t`${subject} is taken on`;
  }
  const live = goesLive(plan);
  // A website's own services deploy as the website, by its domain, never as apps (ADR 0039).
  const site = websiteOfAll(live, websiteOf);
  if (live.length > 1 && site) return t`${site} is deployed`;
  const name = site ?? live[0]?.resource?.name ?? "";
  if (live.length === 1 && live[0]) {
    const runs = health.of({ id: live[0].id }, landedAt(change));
    if (runs.read === "read") {
      switch (runs.kind) {
        case "healthy":
          return t`${name} is healthy`;
        case "running":
          return t`${name} is running`;
        case "starting":
          return t`${name} is starting`;
        case "unhealthy":
          return t`${name} fails its health check`;
        case "not-running":
          return t`${name} isn't running`;
        case "paused":
          return t`${name} is paused`;
        case "finished":
          return t`${name} ran to its end`;
        case "unknown":
      }
    }
    return t`${name} is deployed`;
  }
  const made = madeProject(plan);
  if (live.length > 1 && made) return t`${made} is deployed`;
  if (live.length > 1) {
    const apps = notWebsites(live, websiteOf);
    return plural(apps.length || live.length, { one: "# app is deployed", other: "# apps are deployed" });
  }
  const all = changing(plan);
  const only = all[0];
  const subject = shownName(only);
  if (all.length > 1) {
    return onlyDeletes(plan)
      ? plural(all.length, { one: "# resource is deleted", other: "# resources are deleted" })
      : t`Applied`;
  }
  if (plan?.servers.some((server) => server.steps.some((step) => step.releases))) {
    return t`Dockwell let go of ${subject}`;
  }
  // A server's firewall policy has one name, "default", which says nothing: its
  // title says what it does.
  if (only?.resource?.kind === "firewall.policy") {
    const where = plan?.servers.find((s) => s.steps.length > 0)?.serverName ?? only.resource.scope;
    return only.action === PlanAction.DELETE
      ? t`${where} lets in what no rule drops`
      : t`${where} denies everything else`;
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
  onLeave,
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
  /** Closes the sheet, when something in it goes elsewhere. */
  onLeave: () => void;
}) {
  const { t } = useLingui();
  const words = useChangeWords();
  const alsoWords = useAlsoWords();
  const fleetView = useFleetView();
  const risks = useRisks(plan, phase !== "review");
  // Worked out from the review on, so an arrival has what it says as it lands, and room for it.
  const reach = useReach(plan, change, phase === "arrived");
  const serving = reach.domains.flatMap(({ read }) =>
    read && lookOf(read.domain).state === "serving" ? [read.domain.name] : [],
  );
  const warnings = useWarnings(plan, phase !== "review", serving);
  const dataLeft = useDataLeft(plan, change?.steps ?? []);
  const asides = useAsides(plan);
  const fleet = isFleet(plan);
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
        <Callout tone="critical">
          {/* What the form that opened this says of it, as a stale plan, which only the form can put right. */}
          {opening.fix?.(reasonOf(failure)) ?? <MachineWords>{sentence(failure)}</MachineWords>}
        </Callout>
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
      letsGo: words.letsGo(landed ? "done" : "plan"),
      // A server's host goes by its server's name (ADR 0091).
      serverName: (id) => fleetView.servers?.find((s) => s.id === id)?.name,
    }),
  );
  const steps = change?.steps ?? [];
  // Steps sit under the diff, in the room it leaves, so nothing moves as they appear. With one server they
  // say nothing more once it has landed, and a person reading a plan has the plan.
  // An install's packages are its plan (ADR 0064): its step lists them as it's reviewed. So is what a
  // reboot takes offline.
  // So is what stopping a panel stops (ADR 0089).
  const installs =
    plan?.servers.some((s) => s.steps.some((st) => st.install || st.reboot || st.unit || st.panel)) ?? false;
  const unitOnly =
    (plan?.resources.length ?? 0) > 0 &&
    (plan?.resources.every((r) => r.unit !== undefined || r.panel !== undefined) ?? false);
  // An update on one server says how it goes as its phases, which say what its steps would (ADR 0080).
  const upgrade = upgradeOf(plan);
  const phased = upgrade !== undefined && !fleet;
  const showSteps =
    (phase === "applying" && !phased) ||
    ((fleet || installs) && (ran || phase === "review")) ||
    (phase === "failed" && (steps.length > 1 || fleet) && !phased);
  const failed = failedStep(change);
  // Discarded, it did nothing. Failed, it may have done some of this, for a while.
  const setOut = phase === "failed" ? t`What it set out to do` : t`What it would have done`;
  const stepsDone = steps.filter((s) => s.state === StepState.DONE).length;
  const nothingNewer = foundNothingNewer(change);
  const confirmation = phase === "applying" ? confirmationOf(change) : undefined;

  return (
    <div className="plan-body">
      {failure ? (
        <Callout tone="critical" title={t`That didn't apply`}>
          {/* What the form that opened this says of it, as a stale plan, which only the form can put right. */}
          {opening.fix?.(reasonOf(failure)) ?? <MachineWords>{refusal(failure, plan)}</MachineWords>}
        </Callout>
      ) : null}
      {/* A change to who reaches a server waits for you to say you still can: that comes first. */}
      {confirmation && change ? (
        <ConfirmAccess
          changeId={change.id}
          wait={confirmation}
          stays={
            plan?.servers.find((s) => s.serverId === confirmation.serverId)?.steps.find((step) => step.stays)?.stays
          }
          appliedBy={change.appliedBy?.id}
        />
      ) : null}
      {/* A change Dockwell made on its own says why first, as nobody asked for it. */}
      {change?.because ? <BecauseNotice because={change.because} /> : null}
      {/* What it reaches in projects the reader can't see is a count, never a name (ADR 0084). */}
      {change?.elsewhere && change.elsewhere.items > 0 ? <ElsewhereLine elsewhere={change.elsewhere} /> : null}
      {/* An update says what it goes from and to, and how it goes, before anything else of its plan. */}
      {upgrade && phase !== "discarded" ? <UpgradeLine intent={upgrade} /> : null}
      {phased && (phase === "applying" || phase === "failed") ? (
        <UpgradePhases phases={upgradePhases(plan, change)} />
      ) : null}
      {/* A plan that waits for an admin says why, in the server's words, which name what it reaches. */}
      {phase === "review" && change?.waits ? (
        <Callout tone="info" title={t`Waits for an admin or the owner`}>
          <MachineWords>{change.waits}</MachineWords>
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
          {dataLeft ? (
            <Callout tone="warning" title={dataLeft.title}>
              {dataLeft.detail}
              <HowItWorks notices={[{ key: "data-left", ...dataLeft }]} />
            </Callout>
          ) : null}
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
        <ReachRoom reach={reach} phone={phone}>
          <div className="plan-lead">
            <Icon name="activity" size={16} />
            <span>
              <Impact plan={plan} />
            </span>
            {phase === "applying" && steps.length > 1 ? (
              <span className="plan-lead-count">{t`${stepsDone} of ${steps.length} done`}</span>
            ) : null}
          </div>
        </ReachRoom>
      ) : null}
      {phase === "arrived" && change && nothingNewer ? (
        <ReachRoom reach={reach} phone={phone}>
          <div className="plan-lead">
            <Icon name="check" size={16} />
            <span>
              <MachineWords>{nothingNewer}</MachineWords>
            </span>
          </div>
        </ReachRoom>
      ) : null}
      {phase === "arrived" && change && !nothingNewer ? (
        <Arrival reach={reach} change={change} phone={phone} onLeave={onLeave} />
      ) : null}
      {phase === "history" && change ? <OnRecord change={change} /> : null}
      {phase !== "discarded" ? <Places plan={plan} /> : null}
      {/* An action on a system service is its step (ADR 0082): the host it edits says nothing more. */}
      {unitOnly ? null : (
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
      )}
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
  const [first, ...rest] = risks.filter((risk) => !risk.amber);
  if (first) {
    return (
      <>
        <Callout tone="critical" title={<MachineWords>{first.title}</MachineWords>} className="plan-risk">
          {first.detail}
          {inSight([first, ...rest])
            .slice(1)
            .map((risk) => (
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
          <HowItWorks notices={[first, ...rest]} />
        </Callout>
        {risks
          .filter((risk) => risk.amber)
          .map((risk) => (
            <Callout
              key={risk.key}
              tone="warning"
              title={<MachineWords>{risk.title}</MachineWords>}
              className="plan-risk"
            >
              {risk.detail}
              <HowItWorks notices={[risk]} />
            </Callout>
          ))}
      </>
    );
  }
  // With nothing worse to say first, what a plan warns of in amber leads.
  const notices: Notice[] = [...risks.filter((risk) => risk.amber), ...warnings];
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
  const [lead, ...others] = inSight(notices);
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
      <HowItWorks notices={notices} />
    </Callout>
  );
}

/**
 * Why a notice says what it says, and how it comes about, closed: the notice
 * itself is its fact and what to do about it. Several in one callout each say
 * whose it is.
 */
function HowItWorks({ notices }: { notices: readonly Notice[] }) {
  // Whose each reason is shows once it's open, so a closed one repeats no title in the page.
  const [open, setOpen] = useState(false);
  // Notices of one title, such as each way an app gets root, give their reasons together.
  const groups: { title: ReactNode; reasons: { key: string; more: ReactNode }[] }[] = [];
  for (const { key, title, more } of notices) {
    if (more === undefined || more === "") continue;
    const same = groups.find((g) => typeof title === "string" && g.title === title);
    if (same) same.reasons.push({ key, more });
    else groups.push({ title, reasons: [{ key, more }] });
  }
  if (groups.length === 0) return null;
  const several = inSight(notices).length > 1;
  return (
    <details className="plan-details plan-how" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary>
        <Trans>How this works</Trans>
      </summary>
      {groups.flatMap((group) =>
        group.reasons.map(({ key, more }, i) => (
          <p key={key} className="plan-risk-more">
            {several && open && i === 0 ? (
              <>
                <b>
                  <MachineWords>{group.title}</MachineWords>.
                </b>{" "}
              </>
            ) : null}
            <MachineWords>{more}</MachineWords>
          </p>
        )),
      )}
    </details>
  );
}

/**
 * The notices a callout shows, each once: one with the title and the words of
 * one before it says nothing new in sight, as what differs, such as how an app
 * gets root, is under How this works.
 */
function inSight(notices: readonly Notice[]): Notice[] {
  const seen = new Set<string>();
  return notices.filter((notice) => {
    const { title, detail } = notice;
    if (typeof title !== "string" || (detail !== undefined && typeof detail !== "string")) return true;
    const key = `${title}\n${detail ?? ""}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
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
  const { i18n, t } = useLingui();
  const now = useControlPlaneNow(30_000);
  // What stops reads as the Projects list reads it: a website by its domain, a database as one, the rest as apps.
  const stopsAny = goesAway(plan).length > 0;
  const websiteOf = useWebsiteOfEnvironment(stopsAny);
  const databases = useQuery(DatabaseService.method.listDatabases, {}, { enabled: stopsAny }).data?.databases;
  const servers = (plan?.servers ?? []).filter((s) => !s.removed);
  // A change of labels alone is on its servers, with no step there (ADR 0091).
  const labelled = labelsOnly(plan);
  if (servers.length === 0 && labelled.length > 0) {
    const on = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(labelled);
    return t`${on} · no step, nothing goes down`;
  }
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
  // A website's own services stop as the website, by its domain, and only apps count as apps (ADR 0039).
  const site = websiteOfAll(away, websiteOf);
  const notSites = notWebsites(away, websiteOf);
  const databaseIds = new Set((databases ?? []).map((d) => d.id));
  const databasesAway = notSites.filter((r) => databaseIds.has(r.id)).length;
  const count = {
    apps: notSites.length > 0 ? notSites.length - databasesAway : away.length,
    databases: databasesAway,
  };
  const names = away.map((r) => r.resource?.name ?? r.id);
  const name = site ?? names[0] ?? "";
  const down = longestDowntime(servers);
  const span = time.span(down);
  const healthy = longestUntilHealthy(servers);
  const bound = time.bound(healthy);
  const ifNewer = servers
    .flatMap((s) => s.steps)
    .filter((s) => s.downtime)
    .every(replacesIfNewer);
  const going = servers.flatMap((s) => s.steps).flatMap((s) => (s.goes ? [s.goes] : []));
  const lost = going[0]?.where ?? "";
  const rolling = servers.flatMap((s) => s.steps).some((s) => s.rollout?.rolling && !startsStopped(s));
  // An install that restarts Docker stops every container there, unless Docker keeps them running (ADR 0064).
  const dockerStops = servers
    .flatMap((s) => s.steps)
    .some((s) => (s.install?.dockerRestarts === true && !s.install.keepsRunning) || s.unit?.dockerRestarts === true);
  // A reboot takes down everything on its server until it's back (ADR 0064).
  const rebooting = servers.flatMap((s) => s.steps).some((s) => s.reboot !== undefined);
  // An action on a system service says what it does to what that unit serves (ADR 0082).
  const unit = servers.flatMap((s) => s.steps).find((s) => s.unit !== undefined)?.unit;
  const running = rebooting ? (
    <Trans>everything there is offline until it's back</Trans>
  ) : unit?.replacesRules ? (
    <Trans>published ports may stop answering until Docker restarts</Trans>
  ) : unit && !unit.dockerRestarts ? (
    unit.action === SystemServiceAction.RESTART ? (
      <Trans>what it serves pauses while it restarts</Trans>
    ) : unit.action === SystemServiceAction.STOP ? (
      <Trans>what it serves stops until it's started</Trans>
    ) : unit.action === SystemServiceAction.START ? (
      <Trans>nothing stops</Trans>
    ) : unit.action === SystemServiceAction.RELOAD ? (
      <Trans>no downtime expected</Trans>
    ) : (
      <Trans>nothing stops now; it counts at the next boot</Trans>
    )
  ) : going.length > 1 ? (
    <Trans>visitors lose {going.length} addresses</Trans>
  ) : going.length === 1 ? (
    <Trans>visitors lose {lost}</Trans>
  ) : dockerStops ? (
    <Trans>apps and websites stop while Docker restarts</Trans>
  ) : down === 0 ? (
    rolling ? (
      <Trans>rolling, with nothing stopped</Trans>
    ) : (
      <Trans>no downtime expected</Trans>
    )
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
  // A deleted volume's data stays on its server for the undo's 7 days; anything else that destroys data doesn't.
  const gone = servers.flatMap((s) => s.steps).some(losesData);
  // A removal has no undo, whatever it stops (ADR 0090).
  const undoable = windowMs > 0 && !gone && !plan?.removes;
  const stays = [
    ...new Set(
      away.flatMap((r) =>
        r.changes.filter((c) => /(^|\.)mounts\[[^\]]*\]\.volume$/.test(c.path) && c.from !== "").map((c) => c.from),
      ),
    ),
  ].join(", ");
  /** How many apps and databases stop, and until when it can be undone, if it can. */
  const stopWords = (n: { apps: number; databases: number }, deadline: string): string => {
    if (n.databases === 0) {
      return deadline
        ? plural(n.apps, {
            one: `# app stops, and you can undo this until ${deadline}.`,
            other: `# apps stop, and you can undo this until ${deadline}.`,
          })
        : plural(n.apps, { one: "# app stops.", other: "# apps stop." });
    }
    if (n.apps === 0) {
      return deadline
        ? plural(n.databases, {
            one: `# database stops, and you can undo this until ${deadline}.`,
            other: `# databases stop, and you can undo this until ${deadline}.`,
          })
        : plural(n.databases, { one: "# database stops.", other: "# databases stop." });
    }
    const apps = plural(n.apps, { one: "# app", other: "# apps" });
    const dbs = plural(n.databases, { one: "# database", other: "# databases" });
    return deadline
      ? t`${apps} and ${dbs} stop, and you can undo this until ${deadline}.`
      : t`${apps} and ${dbs} stop.`;
  };
  const stops =
    away.length === 1 || (away.length > 1 && site) ? (
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
      stopWords(count, undoable ? deadline : "")
    ) : null;
  const others = changing(plan).length > away.length;
  // Which registry logins its pulls sign in with, by name, as the control plane picked them (ADR 0086).
  const logins = [
    ...new Set(servers.flatMap((s) => s.steps).flatMap((s) => (s.registryLogin ? [s.registryLogin] : []))),
  ];
  const loginList = logins.join(", ");
  // A build's Dockerfile can read whatever the logins it's given can (ADR 0086).
  const buildLogins = [...new Set(servers.flatMap((s) => s.steps).flatMap((s) => s.buildLogins))].join(", ");
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
      {logins.length > 0 ? (
        <>
          {" "}
          ·{" "}
          <Trans>
            pulls with <code>{loginList}</code>
          </Trans>
        </>
      ) : null}
      {buildLogins ? (
        <>
          {" "}
          ·{" "}
          <Trans>
            its Dockerfile can read any image <code>{buildLogins}</code> can read
          </Trans>
        </>
      ) : null}
    </>
  );
}

/** A port an arrival says it's reached at. */
interface PortReach {
  at: string;
  /** The address or the name it's at, without its port. */
  host: string;
  local: boolean;
  target: number;
  udp: boolean;
}

/** What to do next for a route behind what ends TLS in front of it, as nothing else there is Dockwell's to do. */
type NextStep = "tunnel" | "cloudflare" | "proxy";

/** A route an arrival leads with: what was read of it, or until then what the plan that adds it says. */
interface DomainInReach {
  key: string;
  /** Its domain and path. */
  where: string;
  read?: { domain: Domain; route: DomainRoute };
  /** The port it sends to, from the plan, until it's read. */
  port?: number;
  next?: NextStep;
  /** The servers next is on, once there's more than one it could be. */
  on?: string;
}

/**
 * What a change's arrival leads with. It's worked out from the review on, with what
 * the plan says of what it'll change, so there's room for it before it's there.
 */
interface Reach {
  kind: "took" | "nothing" | "rows";
  domains: readonly DomainInReach[];
  ports: readonly PortReach[];
  /** How many rows its ports will take once the change is through, which ports can't say before then. */
  portRows: number;
  live: readonly PlannedResource[];
  server: string;
  /** What's to read hasn't been: it can't say where this is reached, or that nothing does it. */
  reading: boolean;
  domainsFailed: boolean;
  retryingDomains: boolean;
  retryDomains: () => void;
  /** When the services were last read, and whether reading them failed. */
  servicesReadAt: number;
  servicesFailed: boolean;
}

/** How many ports a service has once a plan's through, from how many it has now. */
function portsAfter(now: number, planned: PlannedResource): number {
  const count = (op: FieldOp) =>
    new Set(planned.changes.filter((c) => c.op === op).flatMap((c) => /^app\.ports\[(\d+)\]/.exec(c.path)?.[1] ?? []))
      .size;
  return now + count(FieldOp.ADDED) - count(FieldOp.REMOVED);
}

/**
 * The environments a plan's services and routes are in, each with its own, as a name
 * means something only in its environment.
 */
function reachesOf(live: readonly PlannedResource[], routes: readonly PlannedResource[]): EnvironmentReach[] {
  const environments = [...new Set([...live, ...routes].map((r) => r.resource?.scope ?? ""))].filter((e) => e !== "");
  const named = (resources: readonly PlannedResource[], environment: string) =>
    resources.filter((r) => r.resource?.scope === environment).map((r) => r.resource?.name ?? "");
  return environments.map((environment) => ({
    environment,
    services: named(live, environment),
    routes: named(routes, environment),
  }));
}

/** What's left to do for a route with no certificate, by what the plan says it answers. Nothing, for one that answers anyone. */
function nextStepOf(planned: PlannedResource): NextStep | undefined {
  if (changeTo(planned, "tls") !== "off" || changeTo(planned, "plain_for_anyone") === "true") return undefined;
  const proxies = planned.changes.filter((c) => c.path.startsWith("trusted_proxies")).map((c) => c.to);
  if (proxies.includes("cloudflare")) return "cloudflare";
  // The server writes a lone address as a range, as it does whatever is named.
  return proxies.every((proxy) => /^(127\.0\.0\.1|::1)(\/(32|128))?$/.test(proxy ?? "")) ? "tunnel" : "proxy";
}

/** What's left to do, and where: on names the server, once there's more than one it could be. */
function useNextWords(next: NextStep | undefined, on: string | undefined): string {
  const { t } = useLingui();
  switch (next) {
    case "tunnel":
      return on
        ? t`Next, point the tunnel on ${on} at http://127.0.0.1:80. https:// there fails, as this route takes no certificate.`
        : t`Next, point your tunnel's service at http://127.0.0.1:80. https:// there fails, as this server takes no certificate.`;
    case "cloudflare":
      return t`Next, set this zone's SSL/TLS mode to Flexible in Cloudflare. This path crosses the open internet unencrypted, so a tunnel is the better way.`;
    case "proxy":
      return on
        ? t`Next, point what's in front at port 80 on ${on}, over http://. https:// there fails, as this route takes no certificate.`
        : t`Next, point what's in front at port 80 on this server, over http://. https:// there fails, as this server takes no certificate.`;
    default:
      return "";
  }
}

/** What to do next, and where the guide says more. */
function NextStepNote({ next, on }: { next: NextStep; on: string | undefined }) {
  const words = useNextWords(next, on);
  return (
    <>
      {words}{" "}
      <a className="domain-note-link" href={TUNNEL_GUIDE} target="_blank" rel="noreferrer">
        <Trans>See the guide</Trans>
      </a>
    </>
  );
}

function useReach(plan: Plan | undefined, change: Change | undefined, landed: boolean): Reach {
  const listed = useServices();
  const { servers } = useFleetView();
  const live = goesLive(plan).filter((r) => r.resource?.kind === "service");
  const routes = (plan?.resources ?? []).filter((r) => r.resource?.kind === "route" && r.action !== PlanAction.DELETE);
  const read = useDomainsReaching(reachesOf(live, routes));

  // What the plan adds isn't in a read from before it, and it says what the domain is.
  const unread = routes.filter((r) => !read.routes.has(r.resource?.name ?? ""));
  const domainOf = routeDomain;
  // Which servers, once there's more than one a tunnel or a proxy could be for.
  const serversOf = (planned: PlannedResource) =>
    (servers?.length ?? 0) > 1
      ? (plan?.servers ?? [])
          .filter((server) => server.steps.some((step) => step.resourceId === planned.id))
          .map((server) => server.serverName)
          .join(", ")
      : "";
  const nextOf = (planned: PlannedResource | undefined) => {
    const next = planned ? nextStepOf(planned) : undefined;
    const on = planned && next ? serversOf(planned) : "";
    return { ...(next ? { next } : {}), ...(on ? { on } : {}) };
  };
  // What the change itself makes leads, then what's wrong, as only the first few show.
  const ownFirst = (row: { domain: Domain; route: DomainRoute }) => {
    if (routes.some((r) => r.resource?.name === row.route.name)) return 0;
    const state = lookOf(row.domain).state;
    return state === "failed" ? 1 : state === "stale" ? 2 : state === "pending" ? 3 : 4;
  };
  const domains: DomainInReach[] = [...read.rows]
    .sort((a, b) => ownFirst(a) - ownFirst(b))
    .map(({ domain, route }) => {
      const planned = routes.find((r) => r.resource?.name === route.name);
      return { key: route.id, where: routeWhere(domain, route), read: { domain, route }, ...nextOf(planned) };
    });
  for (const route of unread) {
    const name = domainOf(route);
    if (!name) continue;
    const where = routeWhereOf(route) ?? name;
    if (domains.some((domain) => domain.where === where)) continue;
    const port = Number(changeTo(route, "host_port") ?? changeTo(route, "port") ?? 0);
    domains.push({ key: route.id, where, ...(port ? { port } : {}), ...nextOf(route) });
  }

  // A new app with no port among its fields publishes none, which the plan says at once, before the list is read again.
  const unpublished = unreached(change);
  const known = live.map((r) => listed.data?.find((s) => s.id === r.id));
  const nowhere =
    live.length > 0 &&
    live.every((r) => r.action === PlanAction.CREATE && unpublished.includes(r.resource?.name ?? ""));
  const ports = known.flatMap((service) => {
    if (!service) return [];
    const addresses = service.deployments.map(
      (d) => servers?.find((s) => s.id === d.serverId)?.address ?? d.serverName,
    );
    return addresses.flatMap((address) =>
      (appOf(service)?.ports ?? []).map((port) => {
        const local = port.hostIp === "127.0.0.1" || port.hostIp === "::1";
        const host = port.hostIp && port.hostIp !== "0.0.0.0" && port.hostIp !== "::" ? port.hostIp : address;
        const at = `${host.includes(":") ? `[${host}]` : host}:${port.hostPort}`;
        return { at, host, local, target: port.containerPort, udp: port.protocol === 2 };
      }),
    );
  });
  const portRows = live.reduce((all, r, i) => {
    const service = known[i];
    const now = service ? (appOf(service)?.ports.length ?? 0) : 0;
    // Once the list has the change, as it may while it applies, what it says is what's after.
    const after = service && service.revision > r.baseRevision ? now : portsAfter(now, r);
    return all + Math.max(0, after) * Math.max(1, service?.deployments.length ?? plan?.servers.length ?? 1);
  }, 0);

  const kind = (): Reach["kind"] => {
    if (domains.length > 0 || (landed && read.failed)) return "rows";
    if (live.length === 0) return "took";
    if (landed ? ports.length > 0 && !nowhere : portRows > 0) return "rows";
    return nowhere || known.every((service) => service) ? "nothing" : "took";
  };
  return {
    kind: kind(),
    domains,
    ports,
    portRows,
    live,
    server: plan?.servers[0]?.serverName ?? "",
    reading: read.isLoading || (!read.failed && unread.some((route) => !domainOf(route))),
    domainsFailed: read.failed,
    retryingDomains: read.retrying,
    retryDomains: read.retry,
    servicesReadAt: listed.dataUpdatedAt,
    servicesFailed: listed.isError,
  };
}

/** How many rows an arrival shows of where it's reached, as the plan comes after them and has to be seen. */
const REACH_SHOWN = 3;

/** Nothing reaches what a plan brings to life yet, in words the arrival and its room share. */
function useNothingWords(reach: Reach): string {
  const { t } = useLingui();
  const name = reach.live[0]?.resource?.name ?? "";
  const server = reach.server;
  return reach.live.length === 1
    ? t`Nothing outside ${server} reaches ${name} yet.`
    : t`Nothing outside the server reaches these yet.`;
}

/**
 * Holds room for what the arrival will lead with, from the review on, so the plan
 * stays where it is as that arrives. The words are in attributes, styled in, as this
 * is only there to take up the space they will: nothing reads them, or finds them.
 */
function ReachRoom({ reach, phone, children }: { reach: Reach; phone: boolean; children: ReactNode }) {
  const nothing = useNothingWords(reach);
  const several = reach.live.length > 1;
  const heldPorts = Math.max(0, REACH_SHOWN - reach.domains.length);
  return (
    <div className="plan-room">
      {children}
      <div className="plan-room-held" aria-hidden="true">
        {reach.kind === "rows" ? (
          <>
            {reach.domains.slice(0, REACH_SHOWN).map((domain) => (
              <HeldRow
                key={domain.key}
                where={domain.where}
                next={domain.next}
                on={domain.on}
                note={domain.read ? lookOf(domain.read.domain).message : ""}
              />
            ))}
            {Array.from({ length: Math.min(heldPorts, reach.portRows) }, (_, i) => `port-${i}`).map((key) => (
              <div key={key} className="plan-reach-row" />
            ))}
            {reach.domains.length + reach.portRows > REACH_SHOWN ? <div className="plan-reach-row" /> : null}
          </>
        ) : reach.kind === "nothing" ? (
          <div className="plan-lead is-next">
            <span className="plan-room-icon" />
            <div className="plan-unreached">
              <span data-words={nothing} />
              {(several ? reach.live : reach.live.slice(0, 1)).map((r) => (
                <div key={r.id} className="plan-add-reach">
                  <span className={cx("dw-btn", phone ? "dw-btn-lg" : "dw-btn-sm")} />
                </div>
              ))}
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}

/** A route's row in the room held for it: its lines, and none of its words to read or find. */
function HeldRow({
  where,
  next,
  on,
  note,
}: {
  where: string;
  next: NextStep | undefined;
  on: string | undefined;
  note: string;
}) {
  const nextWords = useNextWords(next, on);
  const words = [note, nextWords].filter((said) => said !== "").join(" ");
  return (
    <div className="plan-reach-row is-domain">
      <span className="plan-room-icon" />
      <span className="plan-reach-at" data-words={where} />
      <span className="plan-copy-room" />
      <span className="plan-reach-facts" data-words="-" />
      {words ? <span className="plan-reach-how" data-words={words} /> : null}
    </div>
  );
}

/** Takes you to the app's page, with the form to add a domain or a port open there. */
function AddReach({
  planned,
  labelled,
  phone,
  onLeave,
}: {
  planned: PlannedResource;
  /** Several apps are on offer, so each button says which it's for. */
  labelled: boolean;
  phone: boolean;
  onLeave: () => void;
}) {
  const { t } = useLingui();
  const router = useRouter({ warn: false });
  const name = planned.resource?.name ?? "";
  const go = (add: "domain" | "port") => {
    onLeave();
    void router.navigate({ to: "/apps/$serviceId", params: { serviceId: planned.id }, search: { add } });
  };
  const size = phone ? "lg" : "sm";
  return (
    <div className="plan-add-reach">
      {labelled ? <code className="plan-add-reach-name">{name}</code> : null}
      <Button
        icon="plus"
        size={size}
        {...(labelled ? { "aria-label": t`Add a domain to ${name}` } : {})}
        onClick={() => go("domain")}
      >
        <Trans>Add a domain</Trans>
      </Button>
      <Button
        icon="plus"
        size={size}
        {...(labelled ? { "aria-label": t`Add a port to ${name}` } : {})}
        onClick={() => go("port")}
      >
        <Trans>Add a port</Trans>
      </Button>
    </div>
  );
}

/** The arrival's first line: where it is reached, or that nothing can reach it yet, and what to do about that. */
function Arrival({
  reach,
  change,
  phone,
  onLeave,
}: {
  reach: Reach;
  change: Change;
  phone: boolean;
  onLeave: () => void;
}) {
  const { t } = useLingui();
  const nothing = useNothingWords(reach);
  const [landedAt] = useState(() => Date.now());
  const [wholeList, setWholeList] = useState(false);
  // What the list said before a change to its ports isn't what it says now, which is on its way.
  const portsMoved = reach.live.some((r) => r.changes.some((c) => c.path.startsWith("app.ports")));
  const portsBehind = portsMoved && reach.servicesReadAt < landedAt && !reach.servicesFailed;
  if (reach.kind === "took" || reach.reading || portsBehind) {
    return (
      <ReachRoom reach={reach} phone={phone}>
        <TookLine change={change} />
      </ReachRoom>
    );
  }
  if (reach.kind === "nothing") {
    return (
      <div className="plan-lead is-next">
        <Icon name="globe" size={16} />
        <div className="plan-unreached">
          <span>
            <MachineWords names={[reach.server, ...reach.live.map((r) => r.resource?.name ?? "")]}>
              {nothing}
            </MachineWords>
          </span>
          {reach.live.map((planned) => (
            <AddReach
              key={planned.id}
              planned={planned}
              labelled={reach.live.length > 1}
              phone={phone}
              onLeave={onLeave}
            />
          ))}
        </div>
      </div>
    );
  }
  const domains = wholeList ? reach.domains : reach.domains.slice(0, REACH_SHOWN);
  const ports = wholeList ? reach.ports : reach.ports.slice(0, Math.max(0, REACH_SHOWN - domains.length));
  const more = reach.domains.length + reach.ports.length - domains.length - ports.length;
  return (
    <>
      {reach.domainsFailed ? (
        <Callout
          tone="warning"
          title={t`Couldn't read the addresses this is reached at`}
          actions={
            <Button onClick={reach.retryDomains} loading={reach.retryingDomains}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          <Trans>The change itself applied. Trying again only reads them, and changes nothing.</Trans>
        </Callout>
      ) : null}
      {reach.domains.length + reach.ports.length > 0 ? (
        <ul className="plan-reach">
          {domains.map(({ key, where, read, port, next, on }) =>
            read ? (
              <DomainReach key={key} domain={read.domain} route={read.route} next={next} on={on} onLeave={onLeave} />
            ) : (
              <li key={key} className="plan-reach-row is-domain">
                <Icon name="globe" size={16} />
                <code className="plan-reach-at">{where}</code>
                <span className="plan-copy-room" />
                <span className="plan-reach-facts">
                  <span className="domain-facts">
                    <StatusBadge status="deploying" label={t`Waiting for the server`} labelIsState />
                    {port ? (
                      <span className="domain-port">
                        <Trans>
                          Port <code>{port}</code>
                        </Trans>
                      </span>
                    ) : null}
                  </span>
                </span>
                {next ? (
                  <span className="plan-reach-how">
                    <NextStepNote next={next} on={on} />
                  </span>
                ) : null}
              </li>
            ),
          )}
          {ports.map(({ at, host, local, target, udp }) => (
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
              {local ? <span className="plan-reach-how">{t`only on the server`}</span> : <PrivateTag address={host} />}
              {udp ? <span className="plan-reach-how">{t`port ${target}, UDP`}</span> : null}
              <CopyButton text={at} label={t`Copy ${at}`} />
            </li>
          ))}
          {more > 0 ? (
            <li className="plan-reach-row">
              <button type="button" className="plan-reach-more" onClick={() => setWholeList(true)}>
                <Trans>and {more} more</Trans>
              </button>
            </li>
          ) : null}
        </ul>
      ) : null}
    </>
  );
}

/**
 * A route in an arrival: open once it serves, and until then what the server says of
 * it. Its address is where someone visiting goes, which the row, its link and its copy
 * all agree on.
 */
function DomainReach({
  domain,
  route,
  next,
  on,
  onLeave,
}: {
  domain: Domain;
  route: DomainRoute;
  next: NextStep | undefined;
  on: string | undefined;
  onLeave: () => void;
}) {
  const { t } = useLingui();
  const look = lookOf(domain);
  const silent = useSilent(domain, look);
  const where = routeWhere(domain, route);
  const serving = look.state === "serving";
  const url = visitorUrl(domain, route);
  return (
    <li
      className={cx(
        "plan-reach-row",
        "is-domain",
        look.state === "failed" && "is-failed",
        look.warning && "is-warning",
      )}
    >
      <Icon name="globe" size={16} />
      {serving ? (
        <a className="plan-reach-at" href={url} target="_blank" rel="noreferrer">
          <code>{where}</code>
          <Icon name="external" size={14} />
        </a>
      ) : (
        <code className="plan-reach-at">{where}</code>
      )}
      {serving ? <CopyButton text={url} label={t`Copy ${where}`} /> : <span className="plan-copy-room" />}
      <span className="plan-reach-facts">
        <DomainFacts domain={domain} route={route} look={look} silent={silent} />
      </span>
      <span className="plan-reach-how">
        <DomainNote domain={domain} look={look} silent={silent} onLeave={onLeave} />
        {next && look.state !== "failed" ? <NextStepNote next={next} on={on} /> : null}
      </span>
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
  const actor = useActorWords();
  const now = useControlPlaneNow(30_000);
  // A token the CLI made reads by where it ran, as on Activity, not by its own name.
  const by = change.appliedBy ? actor(change.appliedBy).name : "";
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
  const rows: {
    icon?: IconName;
    warn?: boolean;
    title: ReactNode;
    detail?: ReactNode;
    more?: ReactNode;
    key: string;
  }[] = [];
  if (phone) rows.push({ key: "reach", icon: "activity", title: <Impact plan={plan} /> });
  // What is there today: a step that fails puts back what ran, as ADR 0017 has it. Waves are ADR 0094's.
  // A restart changes no spec, so it has nothing to roll back to.
  const restarting = restarts(plan);
  if (restarting) {
    rows.push({
      key: "no-undo",
      icon: "undo",
      title: <Trans>A restart has no undo, as it changes nothing to go back to.</Trans>,
    });
  }
  const serving = restarting
    ? []
    : changing(plan).filter((r) => r.resource?.kind === "service" && r.action !== PlanAction.DELETE);
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
  if (fleet && plan?.rollout) {
    // A rollout's servers go one after another, wave by wave (ADR 0094).
    rows.push({
      key: "how",
      icon: "layers",
      title: <Trans>Servers go one at a time, wave by wave, each wave once the one before has passed.</Trans>,
    });
  } else if (fleet) {
    rows.push({
      key: "how",
      icon: "layers",
      title: <Trans>Steps run one at a time on each server, servers side by side.</Trans>,
    });
  }
  // What the Callout has no room for, as something worse has it.
  const worse = risks.some((risk) => !risk.amber);
  if (worse) {
    for (const warning of warnings) rows.push({ ...warning, warn: true });
  }
  if (worse && offline.length > 0) {
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
                <HowItWorks notices={[row]} />
              </div>
            </div>
          ))}
        </section>
      ) : null}
    </>
  );
}

/**
 * What a change waits for, while a change to who reaches a server waits for you
 * to confirm you still can: on its steps, as the watch moves them on, and on the
 * change itself only when it has no steps to say. The change's own is as of
 * when it was read, so a step that has moved on since outranks it.
 */
function confirmationOf(change: Change | undefined): ChangeConfirmation | undefined {
  if (change && change.steps.length > 0) return change.steps.find((s) => s.confirmation)?.confirmation;
  return change?.confirmation;
}

/** What a change reaches in projects the reader can't see, as a count of what it does there. */
function ElsewhereLine({ elsewhere }: { elsewhere: ChangeElsewhere }) {
  const { restarts, items } = elsewhere;
  return (
    <p className="plan-elsewhere">
      <Icon name="lock" size={14} />
      <span>
        {restarts > 0
          ? plural(restarts, {
              one: "# app in a project you can't see restarts with it",
              other: "# apps in projects you can't see restart with it",
            })
          : plural(items, {
              one: "# more in a project you can't see changes with it",
              other: "# more in projects you can't see change with it",
            })}
      </span>
    </p>
  );
}
