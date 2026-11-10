// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService } from "@dockwell/api";
import { Button, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { useMay } from "../auth/may";
import { checkOf, checkStatus, useChecks } from "./checks";
import { WatchSheet } from "./WatchSheet";
import "./Alerts.css";

/**
 * On a domain's row: how the check that watches it stands, linked to Alerts, or
 * Watch, which opens a check with the address filled in. Checks are an admin's,
 * so it says nothing to someone who can't list them.
 */
export function WatchAddress({
  url,
  where,
  serving = true,
}: {
  url: string;
  where: string;
  /** Whether it answers yet: one still getting its certificate isn't offered a check, which would read down. */
  serving?: boolean;
}) {
  const { t, i18n } = useLingui();
  const checks = useChecks();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const [open, setOpen] = useState(false);
  if (!checks.may.allowed || !checks.data) return null;
  const watched = checkOf(checks.data.checks, url);
  if (watched) {
    const s = checkStatus(watched.state);
    const word = i18n._(s.word);
    return (
      <Link to="/alerts/checks" className="alerts-watched" aria-label={t`${where} is watched: ${word}`}>
        <StatusBadge status={s.status} label={t`Watched · ${word}`} labelIsState />
      </Link>
    );
  }
  if (!serving) return null;
  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        icon="eye"
        aria-label={t`Watch ${where}`}
        onClick={() => setOpen(true)}
        {...mayPlan.off}
      >
        <Trans>Watch</Trans>
      </Button>
      {open ? <WatchSheet url={url} onClose={() => setOpen(false)} /> : null}
    </>
  );
}
