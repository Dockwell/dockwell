// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { BackupMethod, BackupService } from "@dockwell/api";
import { Button, Callout, Icon, PHONE_QUERY, SelectField, Sheet, Switch, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useVolumeOptions, type VolumeChoice } from "./useBackups";
import "./Backups.css";

/**
 * Backs up one volume now. It asks the server what that would take first, and says
 * so before anything runs: a database's dump stops nothing, and a copy of a running
 * app's files is only as good as what the app was doing, so stopping it is the
 * default, and a word first (ADR 0033).
 */
export function BackUpSheet({
  volumes,
  chosen,
  onClose,
}: {
  volumes: readonly VolumeChoice[];
  chosen?: string;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const queries = useQueryClient();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(BackupService, transport), [transport]);
  const [volumeId, setVolumeId] = useState(chosen ?? volumes[0]?.id ?? "");
  const [stop, setStop] = useState(true);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const preview = useQuery(
    BackupService.method.previewBackup,
    { volumeId },
    { enabled: volumeId !== "", retry: false },
  );
  const said = preview.data;
  const running = said?.running ?? [];
  const list = new Intl.ListFormat(undefined, { style: "long", type: "conjunction" }).format(running);
  const name = volumes.find((v) => v.id === volumeId)?.name ?? "";
  const options = useVolumeOptions(volumes);
  const risky = said?.method === BackupMethod.COPY_RUNNING;

  const submit = async () => {
    setBusy(true);
    setFailure(undefined);
    try {
      await client.createBackup({ volumeId, stop: risky && stop });
      await queries.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: BackupService.method.listBackups, cardinality: "finite" }),
      });
      onClose();
    } catch (error) {
      setFailure(sentence(error));
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Back up ${name}`}
      className={phone ? "is-phone" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            icon="archive"
            loading={busy}
            disabled={!said || volumeId === ""}
            onClick={() => void submit()}
          >
            <Trans>Back up</Trans>
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        {volumes.length > 1 ? (
          <SelectField
            label={t`Volume`}
            mono
            value={volumeId}
            options={options}
            onChange={(event) => {
              setVolumeId(event.target.value);
              setFailure(undefined);
            }}
          />
        ) : null}
        {preview.error ? (
          <Callout tone="critical" title={t`Dockwell can't reach what this would back up`}>
            {sentence(preview.error)}
          </Callout>
        ) : !said ? (
          <p className="backup-sheet-line" aria-busy="true">
            <Trans>Asking the server what this takes</Trans>
          </p>
        ) : said.method === BackupMethod.DUMP ? (
          <p className="backup-sheet-line">
            <Icon name="database" size={14} />
            <Trans>
              This uses {said.database}'s own dump, with the database running, so nothing stops. A dump restores on a
              newer version too.
            </Trans>
          </p>
        ) : risky ? (
          <>
            <Callout
              tone="warning"
              title={plural(running.length, {
                one: `${list} runs on this volume`,
                other: `${list} run on this volume`,
              })}
            >
              <Trans>A copy of a running app's files can hold half a write, and the app may not start from it.</Trans>
            </Callout>
            <Switch
              checked={stop}
              onChange={setStop}
              label={t`Stop ${list} while it copies`}
              description={
                stop
                  ? t`What stops starts again once the copy ends, however that goes. The copy is exact.`
                  : t`What runs keeps running, and the backup says it was copied running.`
              }
            />
          </>
        ) : (
          <p className="backup-sheet-line">
            <Icon name="archive" size={14} />
            <Trans>Nothing runs on this volume, so the copy is exact.</Trans>
          </p>
        )}
        <p className="backup-sheet-note">
          <Trans>
            It stays on the server's own disk, with owners and permissions as they are, so it goes if the server does.
          </Trans>
        </p>
        {failure ? (
          <Callout tone="critical" title={t`This didn't start`}>
            {failure}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
