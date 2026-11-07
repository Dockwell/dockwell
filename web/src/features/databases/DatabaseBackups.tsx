// SPDX-License-Identifier: AGPL-3.0-only

import type { Database } from "@dockwell/api";
import { Button, Callout, Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { BackUpSheet } from "../backups/BackUpSheet";
import { BackupRows } from "../backups/BackupsPage";
import { useDestinations } from "../backups/DestinationsPanel";
import { usePolicies } from "../backups/PoliciesPanel";
import { PolicySheet } from "../backups/PolicySheet";
import { useBackups, type VolumeChoice } from "../backups/useBackups";
import { useScheduleWords } from "../backups/words";
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
  const destinations = useDestinations().data?.destinations ?? [];
  const schedule = useScheduleWords();
  const [taking, setTaking] = useState(false);
  const [scheduling, setScheduling] = useState(false);
  const name = db.resource?.name ?? "";
  const engine = engineName(db.engine);
  const dumpOf = how.database || engine;
  const backups = query.data?.backups ?? [];
  const volumes: VolumeChoice[] = [{ id: db.volumeId, name: db.volume, environment: db.resource?.scope ?? "" }];
  const policy = policies[0];
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
        actions={
          policy?.spec ? (
            <StatusBadge status="healthy" label={schedule(policy.spec)} />
          ) : (
            <StatusBadge status="stopped" label={t`Only by hand`} />
          )
        }
      >
        <p className="database-line">
          {!how.known ? null : how.dump ? (
            <Trans>
              Each backup is {dumpOf}'s own dump of everything in it, taken while it runs, so nothing stops.
            </Trans>
          ) : (
            <Trans>
              Each backup is a copy of its files, which stops {engine} for an exact copy unless you choose to copy it
              running.
            </Trans>
          )}{" "}
          <Trans>
            A restore replaces what's there through a plan that says so, and keeps a copy of what it replaced for 7
            days.
          </Trans>
        </p>
        <div className="databases-panel-actions">
          {takeOne}
          {policy ? null : (
            <Button variant="secondary" icon="calendar" onClick={() => setScheduling(true)}>
              <Trans>Back up on a schedule</Trans>
            </Button>
          )}
        </div>
      </Panel>
      {query.isPending ? null : backups.length === 0 ? (
        <Panel>
          <EmptyState icon="archive" title={t`No backups of ${name} yet`}>
            <Trans>Back it up before you change what's in it, or upgrade it.</Trans>
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
        <PolicySheet volumes={volumes} destinations={destinations} onClose={() => setScheduling(false)} />
      ) : null}
    </div>
  );
}
