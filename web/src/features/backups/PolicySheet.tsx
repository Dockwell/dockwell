// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  BackupMethod,
  type BackupPolicyInfo,
  BackupPolicySchema,
  BackupService,
  ChangeService,
  type ConnectionInfo,
  BackupPolicy_Drill as Drill,
  BackupPolicy_Every as Every,
  SettingsService,
} from "@dockwell/api";
import { Button, Callout, SegmentedControl, SelectField, Sheet, SkeletonField, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { StateLine } from "../apps/DomainStep";
import { useMay } from "../auth/may";
import { useSignedInUser } from "../auth/useAuthState";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { BackupSealFields, useBackupSeal } from "./BackupSealFields";
import { StoragePicker } from "./StoragePicker";
import {
  firstAvailableStorage,
  freeDestinationName,
  pickerStatus,
  storageKeyForDestination,
  useStorage,
} from "./storage";
import { useVolumeOptions, type VolumeChoice } from "./useBackups";
import { policyName, useWeekdays } from "./words";
import "./Backups.css";

type How = "hour" | "day" | "week";

const EVERY: Readonly<Record<How, Every>> = { hour: Every.HOUR, day: Every.DAY, week: Every.WEEK };

type Check = "week" | "month" | "never";
const DRILL: Readonly<Record<Check, Drill>> = { week: Drill.WEEKLY, month: Drill.MONTHLY, never: Drill.NEVER };

const HOW_OF: Readonly<Partial<Record<Every, How>>> = {
  [Every.HOUR]: "hour",
  [Every.DAY]: "day",
  [Every.WEEK]: "week",
};
const CHECK_OF: Readonly<Partial<Record<Drill, Check>>> = {
  [Drill.WEEKLY]: "week",
  [Drill.MONTHLY]: "month",
  [Drill.NEVER]: "never",
};

/** Where a new schedule sends its copies: off the server, to the first storage there is, or nowhere else. */
export function firstDestination(destinations: readonly BackupDestinationInfo[]): string {
  return destinations[0]?.name ?? "";
}

/**
 * Plans a daily backup of each volume in one step, as a database starts with (#793):
 * at 03:00 in the install's zone, 7 kept, a restore tested weekly, a copy sent off the
 * server where there's storage. The plan is the review.
 */
export function useBackUpDaily(): (
  volumes: readonly VolumeChoice[],
  destinations: readonly BackupDestinationInfo[],
) => void {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const userZone = useSignedInUser()?.timeZone ?? "";
  return (volumes, destinations) => {
    const name = volumes[0]?.name ?? "";
    sheet.open({
      kind: "propose",
      summary:
        volumes.length === 1
          ? t`Back up ${name} daily`
          : plural(volumes.length, { one: "Back up # volume daily", other: "Back up # volumes daily" }),
      edits: volumes.map((volume) => ({
        resource: { kind: "backup.policy", scope: volume.environment, name: policyName(volume.name, "day") },
        spec: {
          kind: {
            case: "backupPolicy" as const,
            value: {
              volume: volume.name,
              every: Every.DAY,
              at: "03:00",
              timeZone: userZone,
              keep: 7,
              drill: Drill.WEEKLY,
              destination: firstDestination(destinations),
            },
          },
        },
      })),
    });
  };
}

/**
 * The time zone at reads in, for the form to say: an existing schedule's own zone,
 * else the person's own profile zone where set (#834), else the install's from
 * Settings, or UTC where it has none (ADR 0042). Undefined until Settings answer
 * when falling back to the install's, so it never says another.
 */
function useScheduleZone(existingZone?: string): { zone: string | undefined; set: boolean } {
  const userZone = useSignedInUser()?.timeZone ?? "";
  const settings = useQuery(SettingsService.method.getSettings, {}).data?.settings;
  if (existingZone) return { zone: existingZone, set: true };
  if (userZone) return { zone: userZone, set: true };
  if (!settings) return { zone: undefined, set: false };
  return { zone: settings.timeZone || "UTC", set: settings.timeZone !== "" };
}

type ConnectionModal =
  | { mode: "add" }
  | { mode: "from"; connection: ConnectionInfo }
  | { mode: "rotate"; connection: ConnectionInfo };

/**
 * A schedule of backups of one volume, and how many to keep, then its plan. It says
 * what that will cost on the disk before anyone commits to it (ADR 0033).
 */
export function PolicySheet({
  volumes,
  destinations,
  editing,
  onClose,
}: {
  volumes: readonly VolumeChoice[];
  destinations: readonly BackupDestinationInfo[];
  /** A schedule to change, as it is now; unset makes a new one (#793). */
  editing?: BackupPolicyInfo;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const weekdays = useWeekdays();
  const userZone = useSignedInUser()?.timeZone ?? "";
  const { zone, set: zoneSet } = useScheduleZone(editing?.spec?.timeZone);
  const mayPlanInstall = useMay()(ChangeService.method.proposeChange, { install: true });
  const storage = useStorage(destinations);
  const seal = useBackupSeal();
  const was = editing?.spec;
  const editedVolume = volumes.find((v) =>
    was?.websiteFolder
      ? Boolean(v.websiteId) && v.environment === editing?.environmentId
      : v.name === was?.volume && v.environment === editing?.environmentId,
  );
  const [volumeId, setVolumeId] = useState(editedVolume?.id ?? volumes[0]?.id ?? "");
  const [how, setHow] = useState<How>((was && HOW_OF[was.every]) || "day");
  const [at, setAt] = useState(was?.at || "03:00");
  const [weekday, setWeekday] = useState(String(was?.weekday ?? 0));
  const [keep, setKeep] = useState(String(was?.keep || 7));
  const [stop, setStop] = useState(was ? was.stop : true);
  // Off the server by default, to the first usable storage there is (#793, #831).
  const [pickedStorage, setPickedStorage] = useState(() =>
    was
      ? storageKeyForDestination(storage.items, was.destination)
      : firstAvailableStorage(storage.items, mayPlanInstall.allowed),
  );
  const [touchedStorage, setTouchedStorage] = useState(false);
  const [connModal, setConnModal] = useState<ConnectionModal>();
  // When storage finishes loading before the user touches the picker, settle onto the matching or default choice.
  const settled = useRef(storage.ready);
  useEffect(() => {
    if (!settled.current && storage.ready && !touchedStorage) {
      settled.current = true;
      setPickedStorage(
        was
          ? storageKeyForDestination(storage.items, was.destination)
          : firstAvailableStorage(storage.items, mayPlanInstall.allowed),
      );
    }
  }, [storage.ready, storage.items, touchedStorage, was, mayPlanInstall.allowed]);
  // A schedule tests that its backups restore weekly unless someone chooses otherwise (ADR 0068).
  const [check, setCheck] = useState<Check>((was && CHECK_OF[was.drill]) || "week");
  const volume = volumes.find((v) => v.id === volumeId);
  const options = useVolumeOptions(volumes);
  const preview = useQuery(
    BackupService.method.previewBackup,
    volume?.websiteId ? { websiteId: volume.websiteId } : { volumeId },
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

  const selectedItem = pickedStorage ? storage.items.find((i) => i.key === pickedStorage) : undefined;
  const selectedStatus = selectedItem ? pickerStatus(selectedItem) : undefined;
  const needsDestination =
    selectedItem?.connection !== undefined &&
    selectedItem.destination === undefined &&
    selectedStatus?.state === "available";
  const storageBlocked =
    selectedItem !== undefined &&
    selectedStatus !== undefined &&
    selectedStatus.state !== "ready" &&
    selectedStatus.state !== "available";
  const storageReady = !storageBlocked && (!needsDestination || (seal.ready && mayPlanInstall.allowed));

  const review = () => {
    if (!volume || !storageReady) return;
    // A schedule changed keeps what this form doesn't say, as an app's link (ADR 0084).
    const value = was
      ? clone(BackupPolicySchema, was)
      : volume.websiteId
        ? create(BackupPolicySchema, { websiteFolder: true, timeZone: userZone })
        : create(BackupPolicySchema, { volume: volume.name, timeZone: userZone });
    value.every = EVERY[how];
    value.at = at;
    value.weekday = Number(weekday);
    value.keep = count;
    value.stop = risky && stop;
    value.drill = DRILL[check];

    const edits: Parameters<typeof sheet.open>[0] extends infer O
      ? O extends { kind: "propose"; edits: infer E }
        ? E
        : never
      : never = [];

    if (needsDestination && selectedItem?.connection) {
      const destName = freeDestinationName(selectedItem.connection.name, storage.destinations);
      value.destination = destName;
      edits.push({
        resource: { kind: "backup.destination", name: destName },
        spec: {
          kind: {
            case: "backupDestination",
            value: {
              connection: selectedItem.connection.name,
              prefix: seal.prefix.trim(),
              passphrase: seal.passphrase,
            },
          },
        },
      });
    } else if (selectedItem?.destination) {
      value.destination = selectedItem.destination.name;
    } else if (pickedStorage.startsWith("dest:")) {
      value.destination = pickedStorage.slice(5);
    } else {
      value.destination = "";
    }

    edits.push({
      resource: {
        kind: "backup.policy",
        scope: editing?.environmentId ?? volume.environment,
        name: editing?.name ?? policyName(volume.websiteId ? "website-folder" : volume.name, how),
      },
      spec: { kind: { case: "backupPolicy", value } },
    });

    const volumeName = volume.name;
    sheet.open({
      kind: "propose",
      summary: editing ? t`Change how ${volumeName} is backed up` : t`Back up ${volumeName} on a schedule`,
      edits,
      onClose: (change) => {
        if (change) onClose();
      },
    });
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`Change the schedule` : t`Back up on a schedule`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!volume || !validKeep || !validAt || !storageReady} onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        {volumes.length > 1 && !editing ? (
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
          hint={t`The oldest goes once a new one is whole`}
          error={validKeep ? undefined : t`Use a number from 1 to 1000.`}
          onChange={(event) => setKeep(event.target.value)}
        />
        <div className="backup-sheet-check">
          <SegmentedControl<Check>
            label={t`Test restore`}
            value={check}
            onChange={setCheck}
            options={[
              { value: "week", label: t`Weekly` },
              { value: "month", label: t`Monthly` },
              { value: "never", label: t`Never` },
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
        {storage.isPending ? (
          <SkeletonField height={96} />
        ) : storage.items.length > 0 ? (
          <StoragePicker
            items={storage.items}
            value={pickedStorage}
            onChange={(next) => {
              setPickedStorage(next);
              setTouchedStorage(true);
            }}
            noneLabel={t`Only this server`}
            noneHint={t`Kept only on the server, unencrypted`}
            mayPlanInstall={mayPlanInstall}
            onAdd={() => setConnModal({ mode: "add" })}
            onNewFrom={(connection) => setConnModal({ mode: "from", connection })}
            onRotateKeys={(connection) => setConnModal({ mode: "rotate", connection })}
          />
        ) : (
          // With no storage, the copies stay on the server's disk, and the way off it is here (#793).
          <StateLine
            status="degraded"
            action={
              <button
                type="button"
                className="domain-step-act"
                onClick={() => setConnModal({ mode: "add" })}
                disabled={!mayPlanInstall.allowed}
                title={mayPlanInstall.reason || undefined}
              >
                <Trans>Add storage</Trans>
              </button>
            }
          >
            {t`Kept only on the server, unencrypted`}
          </StateLine>
        )}
        {needsDestination ? <BackupSealFields seal={seal} /> : null}
        {preview.error ? (
          <Callout tone="warning" title={t`Dockwell can't say what this costs`}>
            {sentence(preview.error)}
          </Callout>
        ) : said && each > 0n ? (
          total > room ? (
            <Callout tone="warning" title={t`This may not fit`}>
              {t`Keeping ${count} takes about ${formatBytes(total, i18n.locale)} while the next is taken; the disk has ${formatBytes(room > 0n ? room : 0n, i18n.locale)}. One that won't fit fails.`}
            </Callout>
          ) : (
            <p className="backup-sheet-note">
              {plural(count, {
                one: `Each backup takes about ${formatBytes(each, i18n.locale)}; keeping one, about ${formatBytes(total, i18n.locale)}. The disk has ${toGive} to give.`,
                other: `Each backup takes about ${formatBytes(each, i18n.locale)}; keeping #, about ${formatBytes(total, i18n.locale)}. The disk has ${toGive} to give.`,
              })}
            </p>
          )
        ) : said ? (
          <p className="backup-sheet-note">
            <Trans>
              Dockwell can't size a backup yet: this volume is empty or too large to measure quickly. The disk has{" "}
              {toGive}.
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
      {connModal ? (
        <ConnectionSheet
          kind="storage"
          existing={storage.allConnections}
          editing={connModal.mode === "rotate" ? connModal.connection : undefined}
          rotateForBackups={connModal.mode === "rotate"}
          prefill={
            connModal.mode === "from"
              ? {
                  endpoint: connModal.connection.endpoint,
                  region: connModal.connection.region,
                  pathStyle: connModal.connection.pathStyle,
                }
              : undefined
          }
          onClose={() => setConnModal(undefined)}
          onSaved={(name) => {
            setConnModal(undefined);
            setPickedStorage(`conn:${name}`);
            setTouchedStorage(true);
            void storage.refetch();
          }}
        />
      ) : null}
    </Sheet>
  );
}
