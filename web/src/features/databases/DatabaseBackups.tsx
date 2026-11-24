// SPDX-License-Identifier: AGPL-3.0-only

import type { BackupPolicyInfo, Database } from "@dockwell/api";
import { BackupPolicy_Drill, ChangeService } from "@dockwell/api";
import { Button, Callout, Loading, Panel, SkeletonRows, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { useShownName } from "../apps/names";
import { useMay } from "../auth/may";
import { BackUpSheet } from "../backups/BackUpSheet";
import { BackupRows } from "../backups/BackupsPage";
import { usePolicies } from "../backups/PoliciesPanel";
import { PolicySheet, useBackUpDaily } from "../backups/PolicySheet";
import { useStorage } from "../backups/storage";
import { useBackups, type VolumeChoice } from "../backups/useBackups";
import { useScheduleWords } from "../backups/words";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { EmptyState } from "../shell/EmptyState";
import { useThisDatabase } from "./DatabaseLayout";
import { backsUp, NoBackups } from "./DatabaseOverview";
import { engineName, policiesOf, useBackupHow } from "./useDatabases";

/**
 * The Backups tab: every backup of the database's volume, newest first, each of which
 * restores through a plan, with the way to take one now and to take them on a schedule.
 * It's the Backups section, for one database.
 */
export function DatabaseBackups() {
  const db = useThisDatabase();
  // With no volume of Dockwell's there's nothing to list, and asking for no volume's
  // backups would list every volume's.
  return backsUp(db) ? (
    <VolumeBackups db={db} />
  ) : (
    <div className="database-overview">
      <NoBackups db={db} />
    </div>
  );
}

function VolumeBackups({ db }: { db: Database }) {
  const { t } = useLingui();
  const query = useBackups(db.volumeId);
  const how = useBackupHow(db);
  const policies = policiesOf(db, usePolicies().data?.policies ?? []);
  const storage = useStorage();
  const destinations = storage.destinations;
  const schedule = useScheduleWords();
  const [taking, setTaking] = useState(false);
  const [scheduling, setScheduling] = useState<{ editing?: BackupPolicyInfo }>();
  const [addingStorage, setAddingStorage] = useState(false);
  const backUpDaily = useBackUpDaily();
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { environment: db.resource?.scope ?? "" });
  const mayAddStorage = may(ChangeService.method.proposeChange, { install: true });
  // A state's action is a plain button: off, with why on hover, where a plan isn't theirs (ADR 0059).
  const actOff = mayPlan.allowed ? {} : { disabled: true, title: mayPlan.reason || undefined };
  const addOff = mayAddStorage.allowed ? {} : { disabled: true, title: mayAddStorage.reason || undefined };
  // As its page is titled: an install's database by the install's name (#796).
  const name = useShownName()(db, "database").title;
  const engine = engineName(db.engine);
  const dumpOf = how.database || engine;
  const backups = query.data?.backups ?? [];
  const volumes: VolumeChoice[] = [{ id: db.volumeId, name: db.volume, environment: db.resource?.scope ?? "" }];
  const policy = policies[0];
  const sentTo = policy?.spec?.destination ?? "";
  const kept = policy?.spec?.keep ?? 0;
  const takeOne = (
    <Button variant="primary" icon="archive" onClick={() => setTaking(true)}>
      <Trans>Back up now</Trans>
    </Button>
  );
  return (
    <div className="database-overview">
      {query.error ? (
        <Callout tone="critical" title={t`Dockwell can't read the backups of ${name}`}>
          {sentence(query.error)}
        </Callout>
      ) : null}
      <Panel
        title={t`How it's backed up`}
        className="database-backups-card"
        actions={
          policy?.spec ? (
            <StatusBadge status="healthy" label={schedule(policy.spec)} />
          ) : (
            <StatusBadge status="stopped" label={t`Only by hand`} />
          )
        }
      >
        <dl className="databases-facts">
          {!policy ? (
            <div className="databases-fact">
              <dt>{t`Schedule`}</dt>
              <dd className="databases-fact-inline">
                <span>{t`Not backed up on a schedule`}</span>
                <span className="databases-muted" aria-hidden="true">
                  ·
                </span>
                <button
                  type="button"
                  className="databases-inline-act"
                  onClick={() => backUpDaily(volumes, destinations)}
                  {...actOff}
                >
                  <Trans>Back up daily</Trans>
                </button>
              </dd>
            </div>
          ) : policy.spec ? (
            <>
              <div className="databases-fact">
                <dt>{t`Keeps`}</dt>
                <dd>{t`The newest ${kept}`}</dd>
              </div>
              <div className="databases-fact">
                <dt>{t`Test restore`}</dt>
                <dd>
                  {policy.spec.drill === BackupPolicy_Drill.WEEKLY
                    ? t`Weekly`
                    : policy.spec.drill === BackupPolicy_Drill.MONTHLY
                      ? t`Monthly`
                      : t`Never`}
                </dd>
              </div>
              <div className="databases-fact">
                <dt>{t`Copies`}</dt>
                <dd className="databases-fact-inline">
                  {policy.spec.destination ? (
                    t`Sent to ${sentTo}, encrypted`
                  ) : (
                    <>
                      <span>{t`Kept only on the server, unencrypted`}</span>
                      <span className="databases-muted" aria-hidden="true">
                        ·
                      </span>
                      {storage.items.length > 0 ? (
                        <button
                          type="button"
                          className="databases-inline-act"
                          onClick={() => setScheduling({ editing: policy })}
                          {...actOff}
                        >
                          <Trans>Send copies off the server</Trans>
                        </button>
                      ) : (
                        <button
                          type="button"
                          className="databases-inline-act"
                          onClick={() => setAddingStorage(true)}
                          {...addOff}
                        >
                          <Trans>Add storage</Trans>
                        </button>
                      )}
                    </>
                  )}
                </dd>
              </div>
            </>
          ) : null}
          {how.known ? (
            <div className="databases-fact">
              <dt>{t`How`}</dt>
              <dd>
                {how.dump
                  ? t`${dumpOf}'s own dump, while it runs`
                  : t`A copy of its files, which stops ${engine} a moment`}
              </dd>
            </div>
          ) : null}
          <div className="databases-fact">
            <dt>{t`Restoring`}</dt>
            <dd>{t`A plan, which keeps what it replaces for 7 days`}</dd>
          </div>
        </dl>
        <div className="databases-panel-actions">
          {takeOne}
          {policy ? (
            <Button variant="secondary" icon="edit" onClick={() => setScheduling({ editing: policy })} {...mayPlan.off}>
              <Trans>Edit schedule</Trans>
            </Button>
          ) : (
            <Button variant="secondary" icon="calendar" onClick={() => setScheduling({})} {...mayPlan.off}>
              <Trans>Choose a schedule</Trans>
            </Button>
          )}
        </div>
      </Panel>
      {query.isPending ? (
        <Loading label={t`Loading backups of ${name}`}>
          <Panel flush>
            <SkeletonRows rows={3} lead="icon" end="pill" />
          </Panel>
        </Loading>
      ) : backups.length === 0 ? (
        <Panel>
          <EmptyState icon="archive" title={t`No backups of ${name} yet`}>
            <Trans>A backup before a change or an upgrade lets you put it back.</Trans>
          </EmptyState>
        </Panel>
      ) : (
        <BackupRows backups={backups} caption={t`Backups of ${name}, newest first`} />
      )}
      {taking ? (
        <BackUpSheet
          volumes={volumes}
          destinations={destinations}
          chosen={db.volumeId}
          onClose={() => setTaking(false)}
        />
      ) : null}
      {scheduling ? (
        <PolicySheet
          volumes={volumes}
          destinations={destinations}
          editing={scheduling.editing}
          onClose={() => setScheduling(undefined)}
        />
      ) : null}
      {addingStorage ? (
        <ConnectionSheet kind="storage" existing={storage.allConnections} onClose={() => setAddingStorage(false)} />
      ) : null}
    </div>
  );
}
