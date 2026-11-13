// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type Backup, BackupCopy, BackupService, BackupState } from "@dockwell/api";
import { Button, Callout, Panel, PHONE_QUERY, StatusBadge, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useActorWords } from "../activity/actor";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { Page } from "../shell/Page";
import { RefLink } from "../shell/RefLink";
import { DrillLine, useBackupActions } from "./BackupsPage";
import { usePolicies } from "./PoliciesPanel";
import { backupStatus, useMethodWord, useStateWord } from "./words";
import "./Backups.css";

/**
 * One backup's own page: what it is of and where it's kept, how it was taken and
 * checked, and Restore, Check, Fetch and Delete, as the list of backups has them.
 * Every name on it opens its own page (#566).
 */
export function BackupPage({ backupId }: { backupId: string }) {
  const { t, i18n } = useLingui();
  const query = useQuery(BackupService.method.getBackup, { id: backupId });
  const backup = query.data?.backup;
  const phone = useMediaQuery(PHONE_QUERY);
  if (query.error) {
    return (
      <Page title={t`Backup`} back="/more">
        <Callout tone="critical" title={t`Dockwell can't read this backup`}>
          {sentence(query.error)}
        </Callout>
      </Page>
    );
  }
  if (!backup) {
    return (
      <Page title={t`Backup`} back="/more">
        <p className="backup-why" aria-busy="true">
          <Trans>Reading the backup</Trans>
        </p>
      </Page>
    );
  }
  const volume = backup.volume;
  const when = backup.startedAt ? formatDateTime(timestampDate(backup.startedAt), i18n.locale) : "";
  return (
    <Page
      title={t`Backup of ${volume}`}
      back="/more"
      breadcrumb={
        <nav className="backup-crumbs" aria-label={t`Breadcrumb`}>
          <Link to="/backups">
            <Trans>Backups</Trans>
          </Link>
          <span aria-hidden="true">/</span>
          <span aria-current="page">{when}</span>
        </nav>
      }
      lead={<Lead backup={backup} />}
      phoneSub={<Lead backup={backup} />}
      actions={<Actions backup={backup} />}
    >
      <Facts backup={backup} />
      {/* A phone's header has no room for actions, so they come after what it is. */}
      {phone ? (
        <div className="backup-actionbar">
          <Actions backup={backup} size="lg" />
        </div>
      ) : null}
    </Page>
  );
}

/** Its state in a word, and its why where it failed or is under way. */
function Lead({ backup }: { backup: Backup }) {
  const stateWord = useStateWord();
  return (
    <span className="backup-state">
      <StatusBadge status={backupStatus(backup)} label={stateWord(backup)} labelIsState />
      {backup.state === BackupState.FAILED ? <span className="backup-why">{backup.error}</span> : null}
      {backup.progress ? <span className="backup-why">{backup.progress}</span> : null}
    </span>
  );
}

/** What its actions are, each off, with why, where the person may not (ADR 0059). */
function Actions({ backup, size }: { backup: Backup; size?: "lg" }) {
  const { t } = useLingui();
  const actions = useBackupActions();
  const may = useMay();
  const at = { environment: backup.environmentId };
  const done = backup.state === BackupState.DONE;
  const sized = size ? { size } : {};
  return (
    <span className="backup-actions">
      {backup.onServer && done ? (
        <Button
          {...sized}
          icon="undo"
          onClick={() => actions.restore(backup)}
          {...may(BackupService.method.restoreBackup, at).off}
        >
          <Trans>Restore</Trans>
        </Button>
      ) : null}
      {backup.onServer && done ? (
        <Button
          {...sized}
          variant="secondary"
          loading={actions.checking.has(backup.id)}
          onClick={() => actions.check(backup)}
          {...may(BackupService.method.drillBackup, at).off}
        >
          <Trans>Check it</Trans>
        </Button>
      ) : null}
      {backup.copy === BackupCopy.STORED && !backup.onServer && backup.fetchOnto.length > 0 ? (
        <Button
          {...sized}
          variant="secondary"
          onClick={() => actions.fetchBack(backup)}
          {...may(BackupService.method.fetchBackup, at).off}
        >
          <Trans>Fetch</Trans>
        </Button>
      ) : null}
      {backup.state === BackupState.RUNNING ? null : (
        <Button
          {...sized}
          variant="ghost"
          icon="trash"
          aria-label={t`Delete this backup`}
          onClick={() => actions.remove(backup)}
          {...may(BackupService.method.deleteBackup, at).off}
        />
      )}
      {actions.refused.get(backup.id) ? (
        <span className="backup-why is-failed" role="alert">
          {t`Can't check it now: ${actions.refused.get(backup.id)}`}
        </span>
      ) : null}
      {actions.view}
    </span>
  );
}

/** What it is, in facts, each name a link to its page. */
function Facts({ backup }: { backup: Backup }) {
  const { t, i18n } = useLingui();
  const method = useMethodWord();
  const time = useTimeWords();
  const actor = useActorWords();
  const policy = (usePolicies().data?.policies ?? []).find((p) => p.id === backup.policyId);
  const facts: [string, ReactNode][] = [
    [
      t`Volume`,
      backup.volumeId ? (
        <RefLink key="volume" kind="volume" id={backup.volumeId}>
          {backup.volume}
        </RefLink>
      ) : (
        <code key="volume">{backup.volume}</code>
      ),
    ],
    [t`Project`, <PlaceLink key="place" environment={backup.environmentId} />],
    [
      t`Server`,
      <RefLink key="server" kind="server" id={backup.serverId}>
        {backup.serverName}
      </RefLink>,
    ],
    [t`How`, method(backup.method, backup.database)],
  ];
  if (backup.sizeBytes > 0n) facts.push([t`Size`, formatBytes(backup.sizeBytes, i18n.locale)]);
  if (backup.startedAt) facts.push([t`Started`, formatDateTime(timestampDate(backup.startedAt), i18n.locale)]);
  if (backup.startedAt && backup.finishedAt) {
    facts.push([t`Took`, time.span(timestampMs(backup.finishedAt) - timestampMs(backup.startedAt))]);
  }
  if (backup.actor) {
    const by = actor(backup.actor);
    facts.push([
      t`By`,
      <span key="by" title={by.full === by.name ? undefined : by.full}>
        {by.name}
      </span>,
    ]);
  }
  if (policy) facts.push([t`Schedule`, policy.name]);
  const destination = backup.destination;
  const kept =
    backup.copy === BackupCopy.STORED
      ? backup.onServer
        ? t`The server's disk, and ${destination}`
        : destination
      : backup.onServer
        ? t`The server's disk`
        : t`Nowhere it can be restored from now`;
  facts.push([t`Kept on`, kept]);
  if (backup.sha256)
    facts.push([
      t`Fingerprint`,
      <code key="hash" className="backup-hash">
        {backup.sha256.slice(0, 16)}
      </code>,
    ]);
  return (
    <Panel title={t`What it is`}>
      <dl className="backup-facts">
        {facts.map(([label, value]) => (
          <div key={label} className="backup-fact">
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
        <div className="backup-fact">
          <dt>{t`Checked`}</dt>
          <dd>{backup.drill ? <DrillLine backup={backup} /> : <span className="backup-why">{t`Not yet`}</span>}</dd>
        </div>
      </dl>
    </Panel>
  );
}

/** The project an environment is in, as a link to its page, by how the panel shows it. */
export function PlaceLink({ environment }: { environment: string }) {
  const places = usePlaces();
  const place = places.place(environment);
  const label = places.label(environment);
  return place?.scope ? (
    <RefLink kind="project" id={place.scope} look="plain">
      {label}
    </RefLink>
  ) : (
    label
  );
}
