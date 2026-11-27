// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  ChangeState,
  type ControlBackupPlace,
  ControlBackupService,
  type GetControlBackupResponse,
  StepUpAction,
} from "@dockwell/api";
import {
  Button,
  Callout,
  CopyButton,
  Loading,
  Panel,
  SelectField,
  Sheet,
  SkeletonRows,
  StatusBadge,
  TextField,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useEffect, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { useEventsHub } from "../activity/EventsHub";
import { useMay } from "../auth/may";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useControlPlaneNow } from "../servers/useFleet";
import { ReadFailed } from "../shell/ReadFailed";
import { BackupSealFields, useBackupSeal } from "./BackupSealFields";
import { useDestinations } from "./DestinationsPanel";
import { recoveryPhraseStrength } from "./recoveryPhrase";
import { freeDestinationName, pickerStatus, useStorage } from "./storage";
import "./ControlBackup.css";

function useControlBackup() {
  const enabled = useMay()(ControlBackupService.method.getControlBackup).allowed;
  const query = useQuery(
    ControlBackupService.method.getControlBackup,
    {},
    {
      enabled,
      refetchInterval: (q) => (q.state.data?.requested || q.state.data?.running ? 3_000 : false),
    },
  );
  const hub = useEventsHub();
  const { refetch } = query;
  useEffect(() => {
    if (!enabled) return;
    return hub.subscribe((msg) => {
      if (msg.events.some((e) => e.controlBackup)) void refetch();
    });
  }, [enabled, hub, refetch]);
  return { ...query, enabled };
}

export function ControlBackupPanel() {
  const { t } = useLingui();
  const query = useControlBackup();
  if (!query.enabled) return null;
  return query.data ? (
    <BackupStatus status={query.data} refresh={query.refetch} />
  ) : (
    <Panel title={t`Control plane`}>
      {query.error ? (
        <ReadFailed
          error={query.error}
          title={t`Can't read control plane backups`}
          actions={
            <Button size="sm" onClick={() => void query.refetch()}>
              <Trans>Try again</Trans>
            </Button>
          }
        />
      ) : (
        <Loading label={t`Loading control plane backups`}>
          <SkeletonRows rows={3} end="value" />
        </Loading>
      )}
    </Panel>
  );
}

type View = "destination" | "servers" | "show" | "set" | "recovery";

function BackupStatus({ status, refresh }: { status: GetControlBackupResponse; refresh: () => Promise<unknown> }) {
  const { t } = useLingui();
  const now = useControlPlaneNow(15_000);
  const words = useTimeWords();
  const transport = useTransport();
  const client = createClient(ControlBackupService, transport);
  const may = useMay();
  const [view, setView] = useState<View>();
  const [failure, setFailure] = useState<string>();
  const [taking, setTaking] = useState(false);
  const offsite = status.offsite;
  const captured = offsite?.bundleAt ?? offsite?.sentAt;
  const age = captured ? words.ago(timestampDate(captured), now) : "";
  const received =
    offsite?.sentAt && captured && timestampMs(offsite.sentAt) - timestampMs(captured) >= 60_000
      ? words.ago(timestampDate(offsite.sentAt), now)
      : "";
  const next = status.nextAt ? timestampMs(status.nextAt) - now : 0;
  const take = async () => {
    setTaking(true);
    setFailure(undefined);
    try {
      await client.takeControlBackup({});
      await refresh();
    } catch (err) {
      setFailure(sentence(err));
    } finally {
      setTaking(false);
    }
  };
  return (
    <Panel
      title={t`Control plane`}
      flush
      className="control-backup"
      actions={
        <Button
          size="sm"
          variant="primary"
          icon="archive"
          loading={taking || status.running}
          disabled={status.requested || !may(ControlBackupService.method.takeControlBackup).allowed}
          onClick={() => void take()}
        >
          <Trans>Take one now</Trans>
        </Button>
      }
    >
      <div className="control-backup-lead">
        <div>
          <h3>
            <Trans>Newest off-site copy</Trans>
          </h3>
          <div className="control-backup-age">{age || (offsite ? t`None yet` : t`No destination`)}</div>
          {offsite ? (
            <div className="control-backup-caption">
              {offsite.name}
              {received ? ` · ${t`Received ${received}`}` : ""}
            </div>
          ) : null}
        </div>
        <div className="control-backup-tools">
          {offsite?.behind || !offsite || offsite.problem || offsite.previousPassphrase ? (
            <StatusBadge
              status="degraded"
              label={!offsite ? t`Off-site copy needed` : t`Needs attention`}
              labelIsState
            />
          ) : offsite?.sentAt ? (
            <StatusBadge status="healthy" label={t`Stored`} labelIsState />
          ) : null}
          {may(ControlBackupService.method.setControlBackupDestination).allowed ? (
            <Button size="sm" onClick={() => setView("destination")}>
              {offsite ? t`Change destination` : t`Pick a destination`}
            </Button>
          ) : null}
        </div>
        {!offsite && !may(ControlBackupService.method.setControlBackupDestination).allowed ? (
          <div className="control-backup-caption">
            <Trans>Ask the owner to choose storage for off-site copies.</Trans>
          </div>
        ) : null}
        {offsite?.problem ? <div className="control-backup-caption">{offsite.problem}</div> : null}
        {offsite?.previousPassphrase ? (
          <div className="control-backup-caption">
            <Trans>Older copies still open with the previous passphrase.</Trans>
          </div>
        ) : null}
      </div>
      {status.problem ? (
        <Callout tone="critical" title={t`The last backup failed`}>
          {status.problem}
        </Callout>
      ) : null}
      {failure ? (
        <Callout tone="critical" title={t`Couldn't request a backup`}>
          {failure}
        </Callout>
      ) : null}
      {status.hourly || status.requested || status.running ? (
        <div className="control-backup-schedule" role="status">
          {status.hourly ? (
            <span>
              <Trans>Over 256 MiB · Hourly at most</Trans>
            </span>
          ) : null}
          {status.running ? (
            <span>
              <Trans>Taking a backup</Trans>
            </span>
          ) : status.requested ? (
            <span>{next > 0 ? t`Queued · In about ${words.span(next)}` : t`Backup queued`}</span>
          ) : null}
        </div>
      ) : null}
      {status.eligibleServers.length > 0 && may(ControlBackupService.method.setControlBackupServers).allowed ? (
        <div className="control-backup-server-settings">
          <Button size="sm" variant="ghost" onClick={() => setView("servers")}>
            <Trans>Choose servers</Trans>
          </Button>
        </div>
      ) : null}
      {status.servers.length > 0 ? (
        <ul className="control-backup-places" aria-label={t`Server copies`}>
          {status.servers.map((place) => (
            <ServerCopy key={place.server} place={place} now={now} />
          ))}
        </ul>
      ) : (
        <div className="control-backup-schedule">
          <Trans>No copies on other servers</Trans>
        </div>
      )}
      {status.earlierDestinations.length > 0 ? (
        <EarlierDestinations destinations={status.earlierDestinations} now={now} refresh={refresh} />
      ) : null}
      <div className="control-backup-footer">
        <div>
          <strong>
            <Trans>Recovery passphrase</Trans>
          </strong>
          <span className="control-backup-caption">
            {status.passphraseKept ? t`Confirmed kept` : t`Keep it outside this install`}
          </span>
        </div>
        <div className="control-backup-tools">
          {may(ControlBackupService.method.showControlBackupPassphrase).allowed ? (
            <Button size="sm" onClick={() => setView("show")}>
              <Trans>Show passphrase</Trans>
            </Button>
          ) : null}
          {may(ControlBackupService.method.setControlBackupPassphrase).allowed ? (
            <Button size="sm" variant="ghost" onClick={() => setView("set")}>
              <Trans>Set passphrase</Trans>
            </Button>
          ) : null}
          <Button size="sm" icon="file" onClick={() => setView("recovery")}>
            <Trans>Recovery sheet</Trans>
          </Button>
        </div>
      </div>
      {view === "servers" ? <ServersSheet status={status} close={() => setView(undefined)} refresh={refresh} /> : null}
      {view === "destination" ? (
        <DestinationSheet status={status} close={() => setView(undefined)} refresh={refresh} />
      ) : null}
      {view === "show" || view === "set" ? (
        <PassphraseSheet
          key={view}
          mode={view}
          close={() => setView(undefined)}
          refresh={refresh}
          recovery={() => setView("recovery")}
          show={() => setView("show")}
        />
      ) : null}
      {view === "recovery" ? <RecoverySheet status={status} close={() => setView(undefined)} /> : null}
    </Panel>
  );
}

function ServerCopy({ place, now }: { place: ControlBackupPlace; now: number }) {
  const { t } = useLingui();
  const words = useTimeWords();
  return (
    <li className="control-backup-place">
      <div>
        <strong>{place.name}</strong>
        {place.problem ? <span className="control-backup-caption">{place.problem}</span> : null}
        {place.previousPassphrase ? (
          <span className="control-backup-caption">
            <Trans>Older copies still open with the previous passphrase.</Trans>
          </span>
        ) : null}
      </div>
      <StatusBadge
        status={
          place.behind || place.problem || place.previousPassphrase ? "degraded" : place.sentAt ? "healthy" : "unknown"
        }
        label={place.sentAt ? words.ago(timestampDate(place.sentAt), now) : t`None yet`}
        labelIsState
      />
    </li>
  );
}

function EarlierDestinations({
  destinations,
  now,
  refresh,
}: {
  destinations: ControlBackupPlace[];
  now: number;
  refresh: () => Promise<unknown>;
}) {
  const { t } = useLingui();
  const words = useTimeWords();
  const mayDismiss = useMay()(ControlBackupService.method.setControlBackupDestination).allowed;
  const transport = useTransport();
  const [dismissing, setDismissing] = useState("");
  const [failure, setFailure] = useState<string>();
  const dismiss = async (pin: string) => {
    setDismissing(pin);
    setFailure(undefined);
    try {
      const client = createClient(ControlBackupService, transport);
      await withStepUpIfAsked(transport, StepUpAction.CONTROL_BACKUP_DESTINATION, `earlier:${pin}`, (proof) =>
        client.setControlBackupDestination({ dismissEarlierPin: pin, proof }),
      );
      await refresh();
    } catch (err) {
      setFailure(sentence(err));
    } finally {
      setDismissing("");
    }
  };
  return (
    <>
      <ul className="control-backup-places" aria-label={t`Earlier destinations`}>
        {destinations.map((place) => (
          <li className="control-backup-place" key={place.pin}>
            <div>
              <strong>{place.name || t`Earlier destination`}</strong>
              <span className="control-backup-caption">
                {place.sentAt
                  ? t`Earlier destination · Last copy ${words.ago(timestampDate(place.sentAt), now)}`
                  : t`Earlier destination`}
              </span>
              {place.previousPassphrase ? (
                <span className="control-backup-caption">
                  <Trans>Older copies still open with the previous passphrase.</Trans>
                </span>
              ) : (
                <span className="control-backup-caption">
                  <Trans>Opens with the current recovery passphrase.</Trans>
                </span>
              )}
            </div>
            {mayDismiss ? (
              <Button
                size="sm"
                variant="ghost"
                loading={dismissing === place.pin}
                disabled={!!dismissing}
                onClick={() => void dismiss(place.pin)}
              >
                <Trans>Dismiss</Trans>
              </Button>
            ) : null}
          </li>
        ))}
      </ul>
      {failure ? (
        <Callout tone="critical" title={t`Couldn't dismiss the destination`}>
          {failure}
        </Callout>
      ) : null}
    </>
  );
}

function DestinationSheet({
  status,
  close,
  refresh,
}: {
  status: GetControlBackupResponse;
  close: () => void;
  refresh: () => Promise<unknown>;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = createClient(ControlBackupService, transport);
  const destinations = useDestinations();
  const storage = useStorage(destinations.data?.destinations);
  const plan = useChangeSheet();
  const seal = useBackupSeal();
  const [picked, setPicked] = useState(status.destination || undefined);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const existing = destinations.data?.destinations ?? [];
  const options = storage.items.filter((item) => pickerStatus(item).state === "available");
  const name = picked ?? existing[0]?.name ?? (options[0] ? options[0].key : "");
  const connection = options.find((item) => item.key === name)?.connection;
  const select = async (selected: string) => {
    setBusy(true);
    setFailure(undefined);
    try {
      await withStepUpIfAsked(transport, StepUpAction.CONTROL_BACKUP_DESTINATION, `destination:${selected}`, (proof) =>
        client.setControlBackupDestination({ destination: selected, proof }),
      );
      await refresh();
      close();
    } catch (err) {
      setFailure(sentence(err));
    } finally {
      setBusy(false);
    }
  };
  const save = () => {
    if (!connection) {
      void select(name);
      return;
    }
    const destName = freeDestinationName(connection.name, existing);
    plan.open({
      kind: "propose",
      summary: t`Add backup destination ${destName}`,
      edits: [
        {
          resource: { kind: "backup.destination", name: destName },
          spec: {
            kind: {
              case: "backupDestination",
              value: {
                connection: connection.name,
                prefix: seal.prefix.trim(),
                passphrase: seal.passphrase,
              },
            },
          },
        },
      ],
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) {
          void destinations.refetch();
          setPicked(destName);
        }
      },
    });
  };
  return (
    <Sheet
      open
      onClose={close}
      title={t`Off-site destination`}
      footer={
        <>
          <Button onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={destinations.isPending || (!name && !status.destination) || (!!connection && !seal.ready)}
            onClick={save}
          >
            {connection ? t`Review plan` : name ? t`Save destination` : t`Remove destination`}
          </Button>
        </>
      }
    >
      <div className="control-backup-form">
        <SelectField
          label={t`Destination`}
          value={name}
          onChange={(e) => setPicked(e.target.value)}
          options={[
            { value: "", label: t`No destination` },
            ...existing.map(destinationOption),
            ...options.map((item) => ({ value: item.key, label: t`Set up ${item.label}` })),
          ]}
        />
        {connection ? <BackupSealFields seal={seal} /> : null}
        <Callout tone="info" title={t`Keep off-site copies protected`}>
          <Trans>Use bucket versioning or object lock and a key that can't delete.</Trans>
        </Callout>
        {!destinations.isPending && existing.length === 0 && options.length === 0 ? (
          <RouterButtonLink to="/connections">
            <Trans>Add an S3 connection</Trans>
          </RouterButtonLink>
        ) : null}
        {failure ? (
          <Callout tone="critical" title={t`Couldn't save the destination`}>
            {failure}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}

function destinationOption(destination: BackupDestinationInfo) {
  return { value: destination.name, label: destination.name };
}

function ServersSheet({
  status,
  close,
  refresh,
}: {
  status: GetControlBackupResponse;
  close: () => void;
  refresh: () => Promise<unknown>;
}) {
  const { t } = useLingui();
  const client = createClient(ControlBackupService, useTransport());
  const [preferred, setPreferred] = useState([status.preferredServers[0] ?? "", status.preferredServers[1] ?? ""]);
  const [excluded, setExcluded] = useState(status.excludedServers);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const save = async () => {
    setBusy(true);
    setFailure(undefined);
    try {
      await client.setControlBackupServers({ preferred: preferred.filter(Boolean), excluded });
      await refresh();
      close();
    } catch (err) {
      setFailure(sentence(err));
      setBusy(false);
    }
  };
  const options = (slot: number) => [
    { value: "", label: t`Choose by available space` },
    ...status.eligibleServers
      .filter((s) => !excluded.includes(s.id) && s.id !== preferred[1 - slot])
      .map((s) => ({ value: s.id, label: s.name })),
  ];
  return (
    <Sheet
      open
      onClose={close}
      title={t`Servers that keep copies`}
      footer={
        <>
          <Button onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" loading={busy} onClick={() => void save()}>
            <Trans>Save servers</Trans>
          </Button>
        </>
      }
    >
      <div className="control-backup-form">
        <SelectField
          label={t`First copy`}
          value={preferred[0]}
          options={options(0)}
          onChange={(e) => setPreferred([e.target.value, preferred[1] ?? ""])}
        />
        <SelectField
          label={t`Second copy`}
          value={preferred[1]}
          options={options(1)}
          onChange={(e) => setPreferred([preferred[0] ?? "", e.target.value])}
        />
        <fieldset className="control-backup-excludes">
          <legend>
            <Trans>Leave out</Trans>
          </legend>
          {status.eligibleServers.map((server) => (
            <label key={server.id}>
              <input
                type="checkbox"
                checked={excluded.includes(server.id)}
                disabled={preferred.includes(server.id)}
                onChange={(e) =>
                  setExcluded((was) => (e.target.checked ? [...was, server.id] : was.filter((id) => id !== server.id)))
                }
              />
              {server.name}
            </label>
          ))}
        </fieldset>
        {failure ? (
          <Callout tone="critical" title={t`Couldn't save servers`}>
            {failure}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}

function PassphraseSheet({
  mode,
  close,
  refresh,
  recovery,
  show,
}: {
  mode: "show" | "set";
  close: () => void;
  refresh: () => Promise<unknown>;
  recovery: () => void;
  show: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = createClient(ControlBackupService, transport);
  const may = useMay();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [phrase, setPhrase] = useState("");
  const [typed, setTyped] = useState("");
  const [custom, setCustom] = useState(false);
  const strength = recoveryPhraseStrength(typed);
  const run = async (action: () => Promise<void>) => {
    setBusy(true);
    setFailure(undefined);
    try {
      await action();
    } catch (err) {
      setFailure(sentence(err));
    } finally {
      setBusy(false);
    }
  };
  const reveal = () =>
    run(async () => {
      const response = await withStepUpIfAsked(transport, StepUpAction.CONTROL_BACKUP_SHOW_PASSPHRASE, "", (proof) =>
        client.showControlBackupPassphrase({ proof }),
      );
      setPhrase(response.passphrase);
    });
  const save = () =>
    run(async () => {
      await withStepUpIfAsked(transport, StepUpAction.CONTROL_BACKUP_SET_PASSPHRASE, "", (proof) =>
        client.setControlBackupPassphrase({ passphrase: custom ? typed : "", proof }),
      );
      await refresh();
      setTyped("");
      show();
    });
  const confirm = () =>
    run(async () => {
      await client.confirmControlBackupPassphrase({ passphrase: typed });
      await refresh();
      recovery();
    });
  return (
    <Sheet
      open
      onClose={close}
      title={mode === "show" ? t`Recovery passphrase` : t`Set recovery passphrase`}
      footer={
        <>
          <Button onClick={close}>
            <Trans>Close</Trans>
          </Button>
          {mode === "set" ? (
            <Button
              variant="primary"
              loading={busy}
              disabled={custom && strength !== "ready"}
              onClick={() => void save()}
            >
              {custom ? t`Set passphrase` : t`Generate passphrase`}
            </Button>
          ) : phrase ? (
            <Button
              variant="primary"
              loading={busy}
              disabled={!typed || !may(ControlBackupService.method.confirmControlBackupPassphrase).allowed}
              onClick={() => void confirm()}
            >
              <Trans>Confirm and open recovery sheet</Trans>
            </Button>
          ) : (
            <Button variant="primary" loading={busy} onClick={() => void reveal()}>
              <Trans>Show passphrase</Trans>
            </Button>
          )}
        </>
      }
    >
      <div className="control-backup-form">
        {mode === "show" ? (
          <>
            <Callout tone="warning" title={t`Keep the passphrase outside this install`}>
              <Trans>It opens every secret in your control plane backup.</Trans>
            </Callout>
            {phrase ? (
              <>
                <div className="control-backup-secret">
                  <code>{phrase}</code>
                  <CopyButton text={phrase} label={t`Copy passphrase`} copiedLabel={t`Passphrase copied`} />
                </div>
                <TextField
                  label={t`Type the passphrase to confirm`}
                  type="password"
                  autoComplete="off"
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                />
              </>
            ) : null}
          </>
        ) : (
          <>
            <Callout tone="warning" title={t`Older copies may still open with the old passphrase`}>
              <Trans>Each place needs a new copy before it can remove older ones.</Trans>
            </Callout>
            <SelectField
              label={t`Passphrase`}
              value={custom ? "custom" : "generated"}
              options={[
                { value: "generated", label: t`Generate a stronger passphrase` },
                { value: "custom", label: t`Use your own` },
              ]}
              onChange={(e) => setCustom(e.target.value === "custom")}
            />
            {custom ? (
              <>
                <TextField
                  label={t`New passphrase`}
                  type="password"
                  autoComplete="new-password"
                  hint={t`At least 20 characters and up to 256 bytes, on one line`}
                  maxLength={256}
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                />
                <div role="status" className="control-backup-caption">
                  {typed
                    ? strength === "ready"
                      ? t`Strength: meets the requirement`
                      : strength === "weak"
                        ? t`Strength: too easy to guess`
                        : t`Strength: enter a longer phrase on one line`
                    : null}
                </div>
              </>
            ) : null}
          </>
        )}
        {failure ? (
          <Callout tone="critical" title={t`Couldn't use the recovery passphrase`}>
            {failure}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}

const shellArg = (word: string) => `'${word.replaceAll("'", "'\"'\"'")}'`;

function recoveryCommands(status: GetControlBackupResponse) {
  const storage = status.storage;
  let offsite: string | undefined;
  if (storage) {
    const prefix = `s3://${storage.bucket}/${storage.prefix}`;
    const options = `--endpoint-url ${shellArg(storage.endpoint)}${storage.region && storage.region !== "auto" ? ` --region ${shellArg(storage.region)}` : ""}`;
    offsite = `newest=$(aws s3 ls ${shellArg(prefix)} ${options} | awk '$4 ~ /[.]dwcb$/ { print $4 }' | LC_ALL=C sort | tail -n 1)
aws s3 cp ${shellArg(prefix)}"\${newest:?No control plane bundle found}" ./control-backup.dwcb ${options}`;
  }
  const readAgent =
    'newest=$(find "$1" -maxdepth 1 -type f -name "*.dwcb" | LC_ALL=C sort | tail -n 1); test -n "$newest" && exec cat -- "$newest"';
  const agents = status.servers
    .filter((place) => place.path)
    .map((place) => {
      const remote = `root() { if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo -n "$@"; fi; }; root sh -c ${shellArg(readAgent)} sh ${shellArg(place.path)}`;
      return {
        name: place.name,
        server: place.server,
        command: `ssh -T "\${backup_ssh:?Set backup_ssh to your SSH login}" ${shellArg(remote)} > ./control-backup.dwcb`,
      };
    });
  return {
    offsite,
    agents,
    open: "dockwell open-backup ./control-backup.dwcb --output ./recovered-control --passphrase-file ./recovery-passphrase",
  };
}

function RecoverySheet({ status, close }: { status: GetControlBackupResponse; close: () => void }) {
  const { t } = useLingui();
  const storage = status.storage;
  const copies = status.servers.filter((p) => p.path);
  const commands = recoveryCommands(status);
  return (
    <Sheet
      open
      onClose={close}
      title={t`Recovery sheet`}
      footer={
        <>
          <Button onClick={close}>
            <Trans>Close</Trans>
          </Button>
          <Button variant="primary" onClick={() => window.print()}>
            <Trans>Print or save as PDF</Trans>
          </Button>
        </>
      }
    >
      <div className="control-backup-form control-recovery-print">
        <h2 className="control-recovery-print-title">
          <Trans>Control plane recovery sheet</Trans>
        </h2>
        <dl className="control-backup-facts">
          <RecoveryFact label={t`Address`}>{status.address}</RecoveryFact>
          <RecoveryFact label={t`Install ID`}>{status.install}</RecoveryFact>
          <RecoveryFact label={t`Generation`}>{String(status.generation)}</RecoveryFact>
          {storage ? (
            <>
              <RecoveryFact label={t`Endpoint`}>{storage.endpoint}</RecoveryFact>
              <RecoveryFact label={t`Region`}>{storage.region || "auto"}</RecoveryFact>
              <RecoveryFact label={t`Bucket`}>{storage.bucket}</RecoveryFact>
              <RecoveryFact label={t`Prefix`}>{storage.prefix}</RecoveryFact>
              <RecoveryFact label={t`Access key ID`}>{storage.accessKeyId}</RecoveryFact>
            </>
          ) : null}
          {copies.map((place) => (
            <RecoveryFact key={place.server} label={place.name}>
              {place.path}
            </RecoveryFact>
          ))}
        </dl>
        <Callout tone="warning" title={t`Keep the recovery passphrase separately`}>
          <Trans>Opening a bundle needs it. Off-site access also needs your storage credentials.</Trans>
        </Callout>
        <div className="control-recovery-commands">
          {commands.offsite ? (
            <div className="control-recovery-step">
              <strong>
                <Trans>Fetch the newest off-site copy</Trans>
              </strong>
              <code>{commands.offsite}</code>
            </div>
          ) : null}
          {commands.agents.map(({ name, command, server }) => (
            <div className="control-recovery-step" key={server}>
              <strong>
                <Trans>Copy from {name}</Trans>
              </strong>
              <span className="control-backup-caption">
                <Trans>Set backup_ssh to a root SSH login or one with passwordless sudo on this server.</Trans>
              </span>
              <code>{command}</code>
            </div>
          ))}
          <div className="control-recovery-step">
            <strong>
              <Trans>Open on the machine with the full Dockwell build</Trans>
            </strong>
            <code>{commands.open}</code>
          </div>
        </div>
        <CopyButton
          className="control-recovery-copy"
          text={[commands.offsite || commands.agents[0]?.command, commands.open].filter(Boolean).join("\n")}
          label={t`Copy recovery commands`}
          copiedLabel={t`Commands copied`}
        />
      </div>
    </Sheet>
  );
}

function RecoveryFact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>
        <code>{children}</code>
      </dd>
    </>
  );
}

export function OverviewControlBackup() {
  const { t } = useLingui();
  const status = useControlBackup().data;
  const now = useControlPlaneNow(30_000);
  const time = useTimeWords();
  if (!status) return null;
  const offsite = status.offsite;
  const captured = offsite?.bundleAt ?? offsite?.sentAt;
  const age = captured ? time.ago(timestampDate(captured), now) : offsite ? t`None yet` : t`No destination`;
  const bad = !offsite || offsite.behind || !!offsite.problem || offsite.previousPassphrase;
  return (
    <Panel
      title={t`Control plane backup`}
      actions={
        <Link to="/settings/backups">
          <Trans>Backups</Trans>
        </Link>
      }
    >
      <div className="control-backup-overview">
        <span>
          <Trans>Newest off-site copy</Trans>
        </span>
        <StatusBadge status={bad ? "degraded" : offsite.sentAt ? "healthy" : "unknown"} label={age} labelIsState />
        {!status.passphraseKept ? (
          <span className="control-backup-caption">
            <Trans>Recovery passphrase still needs to be kept</Trans>
          </span>
        ) : null}
      </div>
    </Panel>
  );
}
