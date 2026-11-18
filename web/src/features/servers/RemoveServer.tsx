// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { AppsOnRemoval, type Change, ChangeService, ChangeState, type Server } from "@dockwell/api";
import { Button, cx, Icon, SelectField, Sheet } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type ReactNode, useId, useMemo, useState } from "react";

import { useApps } from "../apps/useServices";
import { useMay } from "../auth/may";
import { appsUnfitOn, type Unfit } from "../catalog/architectures";
import { type Opening, useChangeSheet } from "../changes/ChangeSheet";
import type { MenuAction } from "../shell/ActionMenu";
import { useFleetView } from "./useFleet";
import "../apps/Apps.css";

/** What a removal asks: what becomes of the server's apps, and whether it may be compromised. */
interface Ask {
  apps: AppsOnRemoval;
  moveTo: string;
  compromised: boolean;
}

/** The words a removal's plan says above it, made where t is, as t can't go into a helper. */
interface SwitchWords {
  label: string;
  on: string;
  off: string;
}

/**
 * The plan of a removal, with "It may be compromised" one step away above it, as a
 * switch that plans the other: compromised revokes the server now and takes no step
 * on it, so its apps can't be stopped, and stay running instead (ADR 0090).
 */
function removal(
  client: ReturnType<typeof createClient<typeof ChangeService>>,
  server: Server,
  ask: Ask,
  words: SwitchWords,
  onClose: (change: Change | undefined) => void,
): Opening {
  return {
    kind: "plan",
    make: () =>
      client.planRemoveServer({
        serverId: server.id,
        apps: ask.apps,
        moveTo: ask.apps === AppsOnRemoval.MOVE ? ask.moveTo : "",
        compromised: ask.compromised,
      }),
    choice: {
      label: words.label,
      on: ask.compromised,
      description: ask.compromised ? words.on : words.off,
      flip: () => {
        const compromised = !ask.compromised;
        const apps = compromised && ask.apps === AppsOnRemoval.STOP ? AppsOnRemoval.LEAVE : ask.apps;
        return removal(client, server, { ...ask, apps, compromised }, words, onClose);
      },
    },
    onClose,
  };
}

/**
 * Remove from fleet, in a server's header menu (ADR 0090, #800). A server that runs
 * apps asks what becomes of them first; then the plan is the confirmation, with the
 * passkey. The control server has none: the control plane runs on it.
 */
export function useRemoveServer(server: Server): { action: MenuAction | undefined; sheet: ReactNode } {
  const { t } = useLingui();
  const may = useMay()(ChangeService.method.planRemoveServer, { install: true });
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const transport = useTransport();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const apps = useApps();
  const [choosing, setChoosing] = useState(false);
  if (server.controlServer || !may.allowed) return { action: undefined, sheet: null };
  const here = (apps.data ?? []).filter((s) => s.deployments.some((d) => d.serverId === server.id));
  const words: SwitchWords = {
    label: t`It may be compromised`,
    on: t`Revoked now, with its keys, and no step on it`,
    off: t`Revoked once its steps have passed`,
  };
  // Once it's out of the fleet, its page has nothing to show.
  const closed = (change: Change | undefined) => {
    if (change?.state === ChangeState.APPLIED) void navigate({ to: "/servers" });
  };
  const start = () => {
    if (here.length > 0) {
      setChoosing(true);
      return;
    }
    sheet.open(removal(client, server, { apps: AppsOnRemoval.LEAVE, moveTo: "", compromised: false }, words, closed));
  };
  return {
    action: {
      id: "remove",
      icon: "trash",
      danger: true,
      label: t`Remove from fleet`,
      disabled: apps.isPending,
      off: may.off.off,
      onSelect: start,
    },
    sheet: choosing ? (
      <RemoveServerSheet
        server={server}
        apps={here.length}
        moving={here.map((s) => ({ name: s.resource?.name ?? s.id, architectures: s.spec?.architectures ?? [] }))}
        // The plan opens in the app's own sheet, which stays as the server leaves the fleet and its page goes.
        plan={(ask) => {
          setChoosing(false);
          sheet.open(removal(client, server, ask, words, closed));
        }}
        onClose={() => setChoosing(false)}
      />
    ) : null,
  };
}

/**
 * What becomes of a server's apps as it's removed: left running, the default, moved
 * to another server, or stopped. The plan comes next, as the confirmation.
 */
function RemoveServerSheet({
  server,
  apps,
  moving,
  plan,
  onClose,
}: {
  server: Server;
  apps: number;
  /** The apps on it, with the architectures each runs on, which a server they move to must be (ADR 0102). */
  moving: readonly { name: string; architectures: readonly string[] }[];
  plan: (ask: Ask) => void;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const name = useId();
  const { servers } = useFleetView();
  const others = (servers ?? []).filter((s) => s.id !== server.id);
  // A server some app on it can't run on can't take them, which it says beside its
  // name; the first that fits is the one offered (#771).
  const unfit = new Map<string, Unfit>();
  for (const s of others) {
    const u = appsUnfitOn(moving, s);
    if (u) unfit.set(s.id, u);
  }
  const fitting = others.filter((s) => !unfit.has(s.id));
  const firstUnfit = unfit.values().next().value;
  const unfitApp = firstUnfit?.app ?? "";
  const unfitRunsOn = firstUnfit?.runsOn ?? "";
  // Why none fits, as it is (#771's review, M3): the apps run on different
  // architectures, so no one server takes them all; or one architecture no other
  // server has said it is yet, where some haven't reported; or one no other server is.
  const limited = moving.filter((a) => a.architectures.length > 0);
  const kinds = new Set(limited.map((a) => [...a.architectures].sort().join(" or ")));
  const differ = kinds.size > 1;
  const differing = new Intl.ListFormat(i18n.locale, { type: "conjunction" }).format(limited.map((a) => a.name));
  const unreported = others.some((s) => !s.architecture);
  const noneFitWords = differ
    ? t`${differing} run on different architectures, so no one server takes them all`
    : unreported
      ? t`${unfitApp} runs on ${unfitRunsOn} alone, and no other server has said it's ${unfitRunsOn} yet`
      : t`${unfitApp} runs on ${unfitRunsOn} alone, and no other server is ${unfitRunsOn}`;
  const whyWords = (u: Unfit) => (u.why === "unknown" ? t`Architecture not known yet` : u.why);
  const [ask, setAsk] = useState<Ask>({ apps: AppsOnRemoval.LEAVE, moveTo: fitting[0]?.id ?? "", compromised: false });
  const only = others.length === 1 ? others[0] : undefined;
  const onlyName = only?.name ?? "";
  const serverName = server.name;
  const choices: { value: AppsOnRemoval; label: string; hint: string; disabled?: boolean }[] = [
    { value: AppsOnRemoval.LEAVE, label: t`Leave them running`, hint: t`Nothing on the server changes` },
    ...(others.length > 0
      ? [
          {
            value: AppsOnRemoval.MOVE,
            label: only ? t`Move them to ${onlyName}` : t`Move them to another server`,
            hint: fitting.length === 0 ? noneFitWords : t`Volumes start empty there, and their data stays`,
            disabled: fitting.length === 0,
          },
        ]
      : []),
    { value: AppsOnRemoval.STOP, label: t`Stop them`, hint: t`Their data stays on the server` },
  ];
  const count = plural(apps, { one: "# app runs on it", other: "# apps run on it" });
  return (
    <Sheet
      open
      title={t`Remove ${serverName}`}
      subtitle={count}
      onClose={onClose}
      className="app-sheet"
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" onClick={() => plan(ask)}>
            <Trans>Plan removal</Trans>
          </Button>
        </>
      }
    >
      <fieldset className="app-front server-remove-apps">
        <legend className="dw-field-label">
          <Trans>Its apps</Trans>
        </legend>
        {choices.map((choice) => (
          <label key={choice.value} className={cx("app-front-choice", ask.apps === choice.value && "is-on")}>
            <input
              type="radio"
              name={name}
              checked={ask.apps === choice.value}
              disabled={choice.disabled}
              onChange={() => setAsk({ ...ask, apps: choice.value })}
            />
            <span className="app-front-text">
              <span className="app-front-name">{choice.label}</span>
              <span className="app-front-hint">{choice.hint}</span>
            </span>
          </label>
        ))}
      </fieldset>
      {ask.apps === AppsOnRemoval.MOVE && !only ? (
        <SelectField
          label={t`Move them to`}
          value={ask.moveTo}
          options={others.map((s) => {
            const u = unfit.get(s.id);
            return u
              ? { value: s.id, label: `${s.name} · ${whyWords(u)}`, disabled: true }
              : { value: s.id, label: s.name };
          })}
          hint={unfit.size > 0 ? t`${unfitApp} runs on ${unfitRunsOn} alone.` : undefined}
          onChange={(event) => setAsk({ ...ask, moveTo: event.target.value })}
        />
      ) : null}
    </Sheet>
  );
}
