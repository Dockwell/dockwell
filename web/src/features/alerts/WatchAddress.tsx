// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService } from "@dockwell/api";
import { Button, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { useMay } from "../auth/may";
import type { MenuAction } from "../shell/ActionMenu";
import { checkOf, checkStatus, useChecks } from "./checks";
import { WatchSheet } from "./WatchSheet";
import "./Alerts.css";

/**
 * How the check that watches an address stands, and the way to start one: the
 * badge, linked to Alerts, once it's watched; until then, Watch, which opens a
 * check with the address filled in, and its sheet while open. Checks are an
 * admin's, so it's nothing to someone who can't list them. A phone's row puts
 * Watch in its menu and keeps the badge (#795).
 */
export function useWatchAddress({
  url,
  where,
  serving = true,
}: {
  url: string;
  where: string;
  /** Whether it answers yet: one still getting its certificate isn't offered a check, which would read down. */
  serving?: boolean;
}): { badge?: ReactNode; watch?: MenuAction & { label: string }; sheet: ReactNode } {
  const { t, i18n } = useLingui();
  const checks = useChecks();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const [open, setOpen] = useState(false);
  const sheet = open ? <WatchSheet url={url} onClose={() => setOpen(false)} /> : null;
  if (!checks.may.allowed || !checks.data) return { sheet };
  const watched = checkOf(checks.data.checks, url);
  if (watched) {
    const s = checkStatus(watched.state);
    const word = i18n._(s.word);
    return {
      sheet,
      badge: (
        <Link to="/alerts/checks" className="alerts-watched" aria-label={t`${where} is watched: ${word}`}>
          <StatusBadge status={s.status} label={t`Watched · ${word}`} labelIsState />
        </Link>
      ),
    };
  }
  if (!serving) return { sheet };
  return {
    sheet,
    watch: {
      id: "watch",
      icon: "eye",
      label: t`Watch`,
      off: mayPlan.off.off,
      disabled: mayPlan.off.disabled ?? false,
      onSelect: () => setOpen(true),
    },
  };
}

/** On a domain's row: how the check that watches it stands, or Watch (useWatchAddress). */
export function WatchAddress(props: { url: string; where: string; serving?: boolean }) {
  return <WatchAddressView state={useWatchAddress(props)} where={props.where} />;
}

/** What useWatchAddress gave, as a wide screen's row shows it: the badge, or Watch as a button. */
export function WatchAddressView({ state, where }: { state: ReturnType<typeof useWatchAddress>; where: string }) {
  const { t } = useLingui();
  const { badge, watch, sheet } = state;
  if (badge) return badge;
  if (!watch) return sheet;
  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        icon="eye"
        aria-label={t`Watch ${where}`}
        onClick={watch.onSelect}
        {...(watch.off ? { off: watch.off } : {})}
        {...(watch.disabled ? { disabled: true } : {})}
      >
        <Trans>Watch</Trans>
      </Button>
      {sheet}
    </>
  );
}
