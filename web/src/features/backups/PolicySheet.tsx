// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  BackupMethod,
  BackupService,
  BackupPolicy_Drill as Drill,
  BackupPolicy_Every as Every,
  SettingsService,
} from "@dockwell/api";
import { Button, Callout, SegmentedControl, SelectField, Sheet, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useChangeSheet } from "../changes/ChangeSheet";
import { destinationOptions } from "./DestinationsPanel";
import { useVolumeOptions, type VolumeChoice } from "./useBackups";
import { policyName, useWeekdays } from "./words";
import "./Backups.css";

type How = "hour" | "day" | "week";

const EVERY: Readonly<Record<How, Every>> = { hour: Every.HOUR, day: Every.DAY, week: Every.WEEK };

type Check = "week" | "month" | "never";
const DRILL: Readonly<Record<Check, Drill>> = { week: Drill.WEEKLY, month: Drill.MONTHLY, never: Drill.NEVER };

/**
 * The time zone at reads in, for the form to say: the install's from Settings, or
 * UTC where it has none, as the control plane gives a schedule that comes without
 * one (ADR 0042). Undefined until Settings answer, so it never says another.
 */
function useScheduleZone(): { zone: string | undefined; set: boolean } {
  const settings = useQuery(SettingsService.method.getSettings, {}).data?.settings;
  if (!settings) return { zone: undefined, set: false };
  return { zone: settings.timeZone || "UTC", set: settings.timeZone !== "" };
}

/**
 * A schedule of backups of one volume, and how many to keep, then its plan. It says
 * what that will cost on the disk before anyone commits to it (ADR 0033).
 */
export function PolicySheet({
  volumes,
  destinations,
  onClose,
}: {
  volumes: readonly VolumeChoice[];
  destinations: readonly BackupDestinationInfo[];
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const weekdays = useWeekdays();
  const { zone, set: zoneSet } = useScheduleZone();
  const [volumeId, setVolumeId] = useState(volumes[0]?.id ?? "");
  const [how, setHow] = useState<How>("day");
  const [at, setAt] = useState("03:00");
  const [weekday, setWeekday] = useState("0");
  const [keep, setKeep] = useState("7");
  const [stop, setStop] = useState(true);
  const [destination, setDestination] = useState("");
  // A schedule checks that its backups restore weekly unless someone chooses otherwise (ADR 0068).
  const [check, setCheck] = useState<Check>("week");
  const volume = volumes.find((v) => v.id === volumeId);
  const options = useVolumeOptions(volumes);
  const preview = useQuery(
    BackupService.method.previewBackup,
    { volumeId },
    { enabled: volumeId !== "", retry: false },
  );
  const said = preview.data;
  const count = Number(keep);
  const validKeep = Number.isInteger(count) && count >= 1 && count <= 1000;
  const validAt = /^([01][0-9]|2[0-3]):[0-5][0-9]$/.test(at);
  const risky = said?.method === BackupMethod.COPY_RUNNING;
  const running = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(said?.running ?? []);
  const each = said?.estimateBytes ?? 0n;
  // A schedule holds one more while the next is taken, and a backup outgrows the last.
  const total = (each * BigInt(validKeep ? count + 1 : 0) * 6n) / 5n;
  const room = (said?.freeBytes ?? 0n) - (said?.reserveBytes ?? 0n);
  const toGive = formatBytes(room > 0n ? room : 0n, i18n.locale);

  const review = () => {
    if (!volume) return;
    const name = policyName(volume.name, how);
    sheet.open({
      kind: "propose",
      summary: t`Back up ${volume.name} on a schedule`,
      edits: [
        {
          resource: { kind: "backup.policy", scope: volume.environment, name },
          spec: {
            kind: {
              case: "backupPolicy",
              value: {
                volume: volume.name,
                every: EVERY[how],
                at,
                weekday: Number(weekday),
                // Left out, so the control plane gives it the install's zone, whichever client makes it.
                timeZone: "",
                keep: count,
                stop: risky && stop,
                destination,
                drill: DRILL[check],
              },
            },
          },
        },
      ],
      onClose: (change) => {
        if (change) onClose();
      },
    });
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Back up on a schedule`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!volume || !validKeep || !validAt} onClick={review}>
            <Trans>Review plan</Trans>
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
            onChange={(event) => setVolumeId(event.target.value)}
          />
        ) : null}
        <SegmentedControl<How>
          aria-label={t`How often`}
          value={how}
          onChange={setHow}
          options={[
            { value: "hour", label: t`Hourly` },
            { value: "day", label: t`Daily` },
            { value: "week", label: t`Weekly` },
          ]}
        />
        {how === "week" ? (
          <SelectField
            label={t`On`}
            value={weekday}
            options={weekdays.map((label, i) => ({ value: String(i), label }))}
            onChange={(event) => setWeekday(event.target.value)}
          />
        ) : null}
        <TextField
          label={how === "hour" ? t`Minutes past the hour, as 00:MM` : t`At`}
          mono
          value={at}
          hint={
            zone === undefined
              ? undefined
              : zoneSet
                ? t`In ${zone}`
                : t`In UTC. Set the install's time zone in Settings to use another.`
          }
          error={validAt ? undefined : t`Use 24-hour HH:MM, such as 03:00.`}
          onChange={(event) => setAt(event.target.value)}
        />
        <TextField
          label={t`Backups to keep`}
          inputMode="numeric"
          value={keep}
          hint={t`The oldest goes once a new one is whole. A failed run takes nothing away, and nothing you took by hand goes.`}
          error={validKeep ? undefined : t`Use a number from 1 to 1000.`}
          onChange={(event) => setKeep(event.target.value)}
        />
        <div className="backup-sheet-check">
          <SegmentedControl<Check>
            label={t`Check that it restores`}
            value={check}
            onChange={setCheck}
            options={[
              { value: "week", label: t`Check weekly` },
              { value: "month", label: t`Check monthly` },
              { value: "never", label: t`Never check` },
            ]}
          />
          {check !== "never" ? (
            <p className="backup-sheet-note">
              {each > 0n
                ? t`Restores the newest into a throwaway copy, about ${formatBytes(each, i18n.locale)}, an hour after its run, for up to 6 h.`
                : t`Restores the newest into a throwaway copy an hour after its run, for up to 6 h.`}
            </p>
          ) : null}
        </div>
        {destinations.length > 0 ? (
          <SelectField
            label={t`Send a copy to`}
            value={destination}
            options={destinationOptions(destinations, t`Nowhere else`)}
            onChange={(event) => setDestination(event.target.value)}
          />
        ) : null}
        {preview.error ? (
          <Callout tone="warning" title={t`Dockwell can't say what this costs`}>
            {sentence(preview.error)}
          </Callout>
        ) : said && each > 0n ? (
          total > room ? (
            <Callout tone="warning" title={t`This may not fit`}>
              {t`Keeping ${count} takes about ${formatBytes(total, i18n.locale)} while the next is taken, and the disk has ${formatBytes(room > 0n ? room : 0n, i18n.locale)} to give. A backup that won't fit fails, and says so on Activity.`}
            </Callout>
          ) : (
            <p className="backup-sheet-note">
              {plural(count, {
                one: `Each backup takes about ${formatBytes(each, i18n.locale)}, so keeping one takes about ${formatBytes(total, i18n.locale)} while the next is taken, and the disk has ${toGive} to give.`,
                other: `Each backup takes about ${formatBytes(each, i18n.locale)}, so keeping # takes about ${formatBytes(total, i18n.locale)} while the next is taken, and the disk has ${toGive} to give.`,
              })}
            </p>
          )
        ) : said ? (
          <p className="backup-sheet-note">
            <Trans>
              Dockwell can't say yet what each backup takes, as this volume is empty or too large to measure quickly.
              The disk has {toGive} to give.
            </Trans>
          </p>
        ) : null}
        {risky ? (
          <Switch
            checked={stop}
            onChange={setStop}
            label={t`Stop ${running} for each backup`}
            description={
              stop
                ? t`What stops starts again once the copy ends, however that goes. Each copy is exact.`
                : t`What runs keeps running, so each copy can hold half a write, and the app may not start from it.`
            }
          />
        ) : null}
      </div>
    </Sheet>
  );
}
