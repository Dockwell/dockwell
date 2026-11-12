// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type AvailableUpdate,
  type Change,
  ChangeService,
  ChangeState,
  type ServiceInfo,
  StepUpAction,
  UpdateKind,
  UpdateSource,
} from "@dockwell/api";
import { Button, cx, Glyph, Icon, Sheet, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";

import { reasonOf } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { withStepUpIfAsked } from "../auth/stepUp";
import { MachineWords } from "../changes/FailureNotice";
import { followChange } from "../changes/follow";
import { needsConfirming } from "../changes/notices";
import { isEmpty, isFinal, longestDowntime, withoutIds } from "../changes/plan";
import { upgradePhases } from "../changes/upgrade";
import { environmentOf, nameOf } from "./app";
import { counts, offered, planUpdate, stepOf, useKindWord, useMayUpdate } from "./updates";
import { readServicesAgain } from "./useServices";
import "./Updates.css";

/**
 * Updating several apps at once (ADR 0080): picked by row on the Apps list or the
 * Overview, each its own change, planned and applied together, so each rolls back
 * alone and one app's failure never undoes another's update.
 */

/**
 * Which apps are picked to update together, of those that can be: an update that
 * counts, a person who may plan it, and no change of the app's under way.
 */
export function useUpdatePicks(services: readonly ServiceInfo[] | undefined, now: number) {
  const mayUpdate = useMayUpdate();
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const anchor = useRef<string>(undefined);
  const pickable = (services ?? []).filter(
    (s) => counts(s.update, now) && s.changeState !== ChangeState.APPLYING && mayUpdate(s).allowed,
  );
  const can = new Set(pickable.map((s) => s.id));
  const selected = pickable.filter((s) => picked.has(s.id));
  const change = (ids: readonly string[], on: boolean) =>
    setPicked((was) => {
      const next = new Set(was);
      for (const id of ids) {
        if (on) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  return {
    /** Whether an app can be picked. */
    can: (id: string) => can.has(id),
    /** Whether it is. */
    has: (id: string) => can.has(id) && picked.has(id),
    selected,
    /** What can be picked whose update is a patch. */
    patches: pickable.filter((s) => offered(s.update)?.kind === UpdateKind.PATCH),
    /** Picks or lets go of one, or with shift everything from the last one picked to it, in order. */
    toggle(order: readonly string[], id: string, on: boolean, range: boolean) {
      const from = anchor.current ? order.indexOf(anchor.current) : -1;
      const to = order.indexOf(id);
      change(range && from >= 0 && to >= 0 ? order.slice(Math.min(from, to), Math.max(from, to) + 1) : [id], on);
      anchor.current = id;
    },
    setAll: change,
    clear() {
      setPicked(new Set());
      anchor.current = undefined;
    },
  };
}

export type UpdatePicks = ReturnType<typeof useUpdatePicks>;

/** A checkbox for a row, part-checked where some of what it stands for is picked. */
export function PickBox({
  checked,
  partly = false,
  label,
  onPick,
}: {
  checked: boolean;
  partly?: boolean;
  label: string;
  onPick: (on: boolean, range: boolean) => void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = partly && !checked;
  }, [partly, checked]);
  return (
    <input
      ref={ref}
      type="checkbox"
      className="update-pick"
      checked={checked}
      aria-label={label}
      onChange={(e) => onPick(e.currentTarget.checked, (e.nativeEvent as MouseEvent).shiftKey === true)}
    />
  );
}

/** Picks every patch update that can be picked, where there's one. */
export function SelectPatches({ picks }: { picks: UpdatePicks }) {
  const n = picks.patches.length;
  if (n === 0) return null;
  return (
    <Button
      variant="ghost"
      size="sm"
      icon="check"
      onClick={() =>
        picks.setAll(
          picks.patches.map((s) => s.id),
          true,
        )
      }
    >
      {plural(n, { one: "Select # patch update", other: "Select # patch updates" })}
    </Button>
  );
}

/** "Update n selected", once anything is picked, which opens the plan of each. */
export function UpdatePicksBar({ picks, className }: { picks: UpdatePicks; className?: string }) {
  const { t } = useLingui();
  const [open, setOpen] = useState<readonly ServiceInfo[]>();
  const n = picks.selected.length;
  return (
    <>
      {n > 0 ? (
        <section className={cx("update-picks-bar", className)} aria-label={t`Selected updates`}>
          <span className="update-picks-count">{plural(n, { one: "# selected", other: "# selected" })}</span>
          <span className="update-picks-grow" />
          <Button size="sm" variant="ghost" onClick={() => picks.clear()}>
            <Trans>Clear</Trans>
          </Button>
          <Button size="sm" variant="primary" icon="arrow-up" onClick={() => setOpen(picks.selected)}>
            {plural(n, { one: "Update # selected", other: "Update # selected" })}
          </Button>
        </section>
      ) : null}
      {open ? (
        <UpdateSeveralSheet
          services={open}
          onClose={(applied) => {
            setOpen(undefined);
            if (applied) picks.clear();
          }}
        />
      ) : null}
    </>
  );
}

/** Where one app's update is, in the sheet. */
type RowState =
  | { kind: "planning" }
  | { kind: "refused"; why: string }
  | { kind: "nothing"; change: Change }
  | { kind: "planned"; change: Change }
  | { kind: "own"; change: Change; why: string }
  | { kind: "applying"; change: Change };

interface Row {
  service: ServiceInfo;
  offer: AvailableUpdate;
  state: RowState;
}

/**
 * What the sheet updates: each app once, and a catalog install once, whichever of
 * its apps were picked, as its update upgrades the install.
 */
function rowsOf(services: readonly ServiceInfo[]): Row[] {
  const installs = new Set<string>();
  const out: Row[] = [];
  for (const service of services) {
    const offer = offered(service.update);
    if (!offer) continue;
    if (service.update?.source === UpdateSource.CATALOG) {
      if (installs.has(environmentOf(service))) continue;
      installs.add(environmentOf(service));
    }
    out.push({ service, offer, state: { kind: "planning" } });
  }
  return out;
}

/**
 * The plan of each picked app's update, one row each, and one Apply for them all
 * (ADR 0080). Each is its own change, safe by default: its backup first, the pull
 * before anything stops, the health-gated rollout that rolls back on its own. A
 * plan that needs a person's own review, as one that destroys data does, stays
 * planned for that, on Activity. Once applied, each row follows its own change.
 * Closing before Apply discards what it planned; after, the updates go on.
 */
export function UpdateSeveralSheet({
  services,
  onClose,
}: {
  services: readonly ServiceInfo[];
  onClose: (applied: boolean) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const [rows, setRows] = useState<Row[]>(() => rowsOf(services));
  const [applied, setApplied] = useState(false);
  const [busy, setBusy] = useState(false);
  const abort = useRef(new AbortController());
  const set = (id: string, state: RowState) =>
    setRows((was) => was.map((r) => (r.service.id === id ? { ...r, state } : r)));

  // Each plan is asked for at once, and says what it is as it comes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the plans are asked for once, as the sheet opens
  useEffect(() => {
    const signal = abort.current.signal;
    for (const row of rowsOf(services)) {
      void planUpdate(transport, row.service, row.offer).then(
        (res) => {
          if (signal.aborted) return;
          const change = res.change;
          if (!change) set(row.service.id, { kind: "refused", why: t`It planned nothing` });
          else if (isEmpty(change.plan)) set(row.service.id, { kind: "nothing", change });
          else {
            const why = ownReview(change);
            set(row.service.id, why ? { kind: "own", change, why } : { kind: "planned", change });
          }
        },
        (err: unknown) => {
          if (!signal.aborted) set(row.service.id, { kind: "refused", why: withoutIds(messageOf(err)) });
        },
      );
    }
    return () => abort.current.abort();
  }, []);

  /** What a plan needs from a person beyond Apply, said as why it waits for its own review. */
  function ownReview(change: Change): string {
    if (change.waits) return change.waits;
    if (needsConfirming(change.plan)) return t`It destroys data, which you confirm in its own plan`;
    if ((change.plan?.servers ?? []).some((s) => s.offline && !s.removed)) return t`Its server is offline`;
    return "";
  }

  const planning = rows.some((r) => r.state.kind === "planning");
  const ready = rows.filter((r) => r.state.kind === "planned");
  const following = rows.filter((r) => r.state.kind === "applying");
  // Over once every apply has answered and every change it started has ended.
  const over = applied && !busy && following.every((r) => r.state.kind === "applying" && isFinal(r.state.change));

  const follow = (id: string, change: Change) => {
    void followChange(transport, change.id, abort.current.signal, (now) => {
      set(id, { kind: "applying", change: now });
      if (isFinal(now)) void readServicesAgain(queryClient);
    });
  };

  const applyAll = async () => {
    if (busy || ready.length === 0) return;
    setBusy(true);
    setApplied(true);
    // Each row goes on as applying at once, and says it's applying until its change says more.
    setRows((was) =>
      was.map((r) => (r.state.kind === "planned" ? { ...r, state: { kind: "applying", change: r.state.change } } : r)),
    );
    for (const row of ready) {
      if (row.state.kind === "planned") await applyRow(row, row.state.change, true);
    }
    setBusy(false);
  };

  // A plan another of these changed the servers under, as one that pulled the image
  // this one pulls too, is planned again. The same update, needing nothing more,
  // applies; one that turns out otherwise is left for its own review, as what was
  // reviewed isn't what would apply.
  const applyRow = async (row: Row, change: Change, again: boolean): Promise<void> => {
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.APPLY_ROOT_CHANGE, change.id, (proof) =>
        client.applyChange({ id: change.id, fingerprint: change.plan?.fingerprint ?? "", proof }),
      );
      const now = res.change ?? change;
      set(row.service.id, { kind: "applying", change: now });
      follow(row.service.id, now);
    } catch (err) {
      if (!again || reasonOf(err) !== "plan_changed") {
        set(row.service.id, { kind: "refused", why: withoutIds(messageOf(err)) });
        return;
      }
      void client.discardChange({ id: change.id }).catch(() => {});
      try {
        const fresh = (await planUpdate(transport, row.service, row.offer)).change;
        if (!fresh || isEmpty(fresh.plan)) {
          set(
            row.service.id,
            fresh ? { kind: "nothing", change: fresh } : { kind: "refused", why: t`It planned nothing` },
          );
        } else if (!ownReview(fresh) && sameUpdate(change, fresh)) {
          await applyRow(row, fresh, false);
        } else {
          set(row.service.id, { kind: "own", change: fresh, why: t`Its plan changed as the others applied` });
        }
      } catch (replanned) {
        set(row.service.id, { kind: "refused", why: withoutIds(messageOf(replanned)) });
      }
    }
  };

  // Closing before Apply lets go of every plan the sheet made; after, only of what
  // it didn't apply and needs nobody's review, and the rest goes on.
  const close = () => {
    for (const row of rows) {
      const s = row.state;
      if (s.kind === "nothing" || (!applied && (s.kind === "planned" || s.kind === "own"))) {
        void client.discardChange({ id: s.change.id }).catch(() => {});
      }
    }
    abort.current.abort();
    onClose(applied);
  };

  const n = rows.length;
  const title = !applied
    ? plural(n, { one: "Update # app", other: "Update # apps" })
    : over
      ? t`Updates finished`
      : plural(following.length, { one: "Updating # app", other: "Updating # apps" });
  const footer = !applied ? (
    <>
      <p className="dw-sheet-note">
        <Trans>Each app updates as its own change, and one that fails rolls back alone.</Trans>
      </p>
      <Button variant="ghost" onClick={close}>
        <Trans>Cancel</Trans>
      </Button>
      <Button
        variant="primary"
        icon="arrow-up"
        disabled={planning || ready.length === 0}
        onClick={() => void applyAll()}
      >
        {plural(ready.length, { one: "Apply # update", other: "Apply # updates" })}
      </Button>
    </>
  ) : (
    <Button variant={over ? "primary" : "ghost"} onClick={close}>
      {over ? <Trans>Done</Trans> : <Trans>Close</Trans>}
    </Button>
  );
  return (
    <Sheet open title={title} onClose={close} footer={footer} className="update-several">
      <ul className="update-several-rows" aria-label={t`Updates`}>
        {rows.map((row) => (
          <SeveralRow key={row.service.id} row={row} />
        ))}
      </ul>
      {applied && !over ? (
        <p className="update-when">
          <Trans>Closing leaves them to go on, each on its app's page and on Activity.</Trans>
        </p>
      ) : null}
    </Sheet>
  );
}

/**
 * Whether two plans are the same update: the same intent, and the same resources,
 * each with the same action. What differs, such as a pull another update made, is
 * nothing a person reviewed.
 */
function sameUpdate(a: Change, b: Change): boolean {
  const ia = a.plan?.intent;
  const ib = b.plan?.intent;
  if (ia?.kind !== ib?.kind || ia?.resourceId !== ib?.resourceId || ia?.image !== ib?.image) return false;
  const shape = (c: Change) =>
    (c.plan?.resources ?? [])
      .map((r) => `${r.id} ${r.action}`)
      .sort()
      .join("\n");
  return shape(a) === shape(b);
}

/** A refusal's words, as the API gave them. */
function messageOf(err: unknown): string {
  return ConnectError.from(err).rawMessage;
}

/** One app's update in the sheet: what it goes to, and where it is. */
function SeveralRow({ row }: { row: Row }) {
  const { t } = useLingui();
  const kindWord = useKindWord();
  const time = useTimeWords();
  const { service, offer, state } = row;
  const name = nameOf(service);
  return (
    <li className={cx("update-several-row", `is-${rowLook(state)}`)}>
      <span className="update-several-what">
        <span className="update-several-name">{name}</span>
        <code className="update-step">{stepOf(offer)}</code>
        <Tag className={cx("update-kind", offer.kind === UpdateKind.MAJOR && "is-major")}>{kindWord(offer.kind)}</Tag>
      </span>
      <span className="update-several-state">
        <RowWords state={state} spanOf={time.span} />
      </span>
      {state.kind === "own" ? <span className="dw-sr">{t`${name} stays planned, on Activity`}</span> : null}
    </li>
  );
}

/** A row's look, for its mark. */
function rowLook(state: RowState): string {
  if (state.kind !== "applying") return state.kind;
  switch (state.change.state) {
    case ChangeState.APPLIED:
      return "done";
    case ChangeState.FAILED:
      return "failed";
    default:
      return "applying";
  }
}

function RowWords({ state, spanOf }: { state: RowState; spanOf: (ms: number) => string }) {
  const { t } = useLingui();
  switch (state.kind) {
    case "planning":
      return (
        <>
          <Glyph shape="spinner" size={12} />
          <Trans>Planning</Trans>
        </>
      );
    case "refused":
      return (
        <span className="update-several-refused">
          <Trans>Can't update:</Trans> <MachineWords>{state.why}</MachineWords>
        </span>
      );
    case "nothing":
      return <Trans>Nothing to change</Trans>;
    case "own":
      return (
        <span className="update-several-own">
          <Trans>Left for its own review:</Trans> <MachineWords>{state.why}</MachineWords>
        </span>
      );
    case "planned": {
      const down = longestDowntime(state.change.plan?.servers ?? []);
      const backs = (state.change.plan?.servers ?? []).some((s) => s.steps.some((step) => step.backupFirst));
      const span = spanOf(down);
      return (
        <>
          {backs ? <span>{t`Backs up first`}</span> : null}
          {down > 0 ? <span>{t`About ${span} down`}</span> : <span>{t`Nothing stops`}</span>}
        </>
      );
    }
    case "applying":
      return <Following change={state.change} />;
  }
}

/** Where an applied update is: its phase while it goes, then how it ended. */
function Following({ change }: { change: Change }) {
  if (change.state === ChangeState.APPLIED) {
    return (
      <span className="update-ok">
        <Icon name="check" size={14} />
        <Trans context="an app's update landed">Updated</Trans>
      </span>
    );
  }
  if (change.state === ChangeState.FAILED) {
    return (
      <span className="update-several-failed">
        {change.rolledBackBy ? <Trans>Failed, and rolled back:</Trans> : <Trans>Failed:</Trans>}{" "}
        <MachineWords>{withoutIds(change.error)}</MachineWords>
      </span>
    );
  }
  const current = upgradePhases(change.plan, change).find((p) => p.state === "current");
  return (
    <>
      <Glyph shape="spinner" size={12} />
      {current?.activity ? <MachineWords>{current.activity}</MachineWords> : <Trans>Applying</Trans>}
    </>
  );
}
