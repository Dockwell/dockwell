// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type AlertCheckInfo, type AlertCheckRun, AlertCheckState, ChangeService } from "@dockwell/api";
import { Button, cx, Icon, PHONE_QUERY, StatusBadge, Table, type TableColumn, Tag, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { formatDateTime, formatMoment } from "../../lib/format";
import { useMay } from "../auth/may";
import { EmptyState } from "../shell/EmptyState";
import { ReadFailed } from "../shell/ReadFailed";
import { AlertsPage } from "./AlertsPage";
import { checkStatus, hostOf, tookMs, useChecks } from "./checks";
import { WatchSheet } from "./WatchSheet";

/** A run in a word: its status and how long it took, or why it failed. */
function useRunWords() {
  const { t } = useLingui();
  return (run: AlertCheckRun) => {
    if (!run.ok) return run.said;
    const ms = tookMs(run);
    return ms === undefined ? String(run.status) : t`${run.status} in ${ms} ms`;
  };
}

/** A check's last runs, oldest first, each a mark that says how it went, and the newest in words. */
function LastAnswers({ check }: { check: AlertCheckInfo }) {
  const { t, i18n } = useLingui();
  const words = useRunWords();
  const runs = [...check.runs].reverse();
  const newest = check.runs[0];
  // A check Dockwell can't run says why, whatever it last answered.
  if (check.problem) return <span className="alerts-run-said">{check.problem}</span>;
  if (!newest) return <span className="alerts-muted">{t`No answer yet`}</span>;
  return (
    <span className="alerts-runs">
      <span className="alerts-runs-marks" role="img" aria-label={t`Last ${runs.length} runs`}>
        {runs.map((run, i) => {
          const at = run.time ? formatMoment(timestampDate(run.time), i18n.locale) : "";
          const said = words(run);
          return (
            <span
              // Runs have no ID of their own; their order is what they are.
              // biome-ignore lint/suspicious/noArrayIndexKey: a run is its place in the list
              key={i}
              className={cx("alerts-run", run.skipped ? "is-skipped" : run.ok ? "is-ok" : "is-failed")}
              title={`${at} · ${said}`}
            />
          );
        })}
      </span>
      <span className={newest.ok || newest.skipped ? "alerts-muted" : "alerts-run-said"}>{words(newest)}</span>
    </span>
  );
}

/** The Checks tab: every address Dockwell asks, how it stands, and its last answers. */
export function ChecksTab() {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const checks = useChecks();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const [editing, setEditing] = useState<AlertCheckInfo>();
  const [making, setMaking] = useState(false);
  const list = checks.data?.checks ?? [];
  const every = (c: AlertCheckInfo) => {
    switch (c.spec?.every) {
      case "30s":
        return t`Every 30 s`;
      case "5m":
        return t`Every 5 min`;
      case "15m":
        return t`Every 15 min`;
      default:
        return t`Every minute`;
    }
  };
  // How often, and where from: "Every minute from the control server".
  const runs = (c: AlertCheckInfo) => {
    const often = every(c);
    // Where a check runs from comes in the control plane's words; its own place is said here.
    const from = c.from === "the control server" ? t`the control server` : c.from;
    return from ? t`${often} from ${from}` : often;
  };
  const state = (c: AlertCheckInfo) => {
    const s = checkStatus(c.state);
    const since =
      c.since && (c.state === AlertCheckState.DOWN || c.state === AlertCheckState.FAILING)
        ? formatDateTime(timestampDate(c.since), i18n.locale)
        : undefined;
    return (
      <StatusBadge status={s.status} label={i18n._(s.word)} labelIsState meta={since ? t`since ${since}` : undefined} />
    );
  };
  const columns: TableColumn<AlertCheckInfo>[] = [
    {
      key: "check",
      header: t`Address`,
      render: (c) => (
        <div className="dw-cell">
          <span className="dw-cell-title">
            {hostOf(c.spec?.address ?? c.name)}
            {c.spec?.insideNetwork ? (
              <>
                {" "}
                <Tag words>{t`Inside the network`}</Tag>
              </>
            ) : null}
          </span>
          <code className="alerts-channel-sub">{c.spec?.address}</code>
        </div>
      ),
    },
    { key: "state", header: t`State`, width: 168, render: state },
    { key: "runs", header: t`Last answers`, width: 300, render: (c) => <LastAnswers check={c} /> },
    {
      key: "how",
      header: t({ context: "how a check runs", message: "Runs" }),
      width: 240,
      muted: true,
      render: runs,
    },
  ];
  const add = (
    <Button variant="secondary" icon="plus" onClick={() => setMaking(true)} {...mayPlan.off}>
      <Trans>Watch an address</Trans>
    </Button>
  );
  return (
    <AlertsPage
      actions={list.length > 0 ? add : undefined}
      phoneAction={
        list.length > 0 && mayPlan.allowed ? (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`Watch an address`}
            onClick={() => setMaking(true)}
          >
            <Icon name="plus" size={20} />
          </button>
        ) : undefined
      }
    >
      {checks.error ? (
        <ReadFailed error={checks.error} title={t`Can't load the checks`} />
      ) : !checks.data ? null : list.length === 0 ? (
        <EmptyState icon="eye" title={t`No address is watched yet`} action={add}>
          {t`Watch one from its app's or website's domain, and Dockwell alerts when it stops answering.`}
        </EmptyState>
      ) : phone ? (
        <ul className="alerts-phone-list" aria-label={t({ context: "checks of a URL", message: "Checks" })}>
          {list.map((c) => (
            <li key={c.id} className="alerts-phone-row">
              <button type="button" className="alerts-phone-item alerts-phone-button" onClick={() => setEditing(c)}>
                <span className="alerts-phone-title">
                  {hostOf(c.spec?.address ?? c.name)}
                  {c.spec?.insideNetwork ? (
                    <>
                      {" "}
                      <Tag words>{t`Inside the network`}</Tag>
                    </>
                  ) : null}
                </span>
                <code className="alerts-channel-sub">{c.spec?.address}</code>
                <span className="alerts-phone-meta">
                  {state(c)}
                  <LastAnswers check={c} />
                </span>
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <Table
          columns={columns}
          rows={list}
          caption={t({ context: "checks of a URL", message: "Checks" })}
          onRowClick={(c) => setEditing(c)}
        />
      )}
      {editing ? <WatchSheet check={editing} onClose={() => setEditing(undefined)} /> : null}
      {making ? <WatchSheet onClose={() => setMaking(false)} /> : null}
    </AlertsPage>
  );
}
