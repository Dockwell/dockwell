// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type Backup, BackupService, CatalogService } from "@dockwell/api";
import { Button, Icon, Sheet, Tag, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useEffect, useMemo, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { SERVICE_NAME } from "../apps/app";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { nextFree } from "../catalog/freeName";
import { useChangeSheet } from "../changes/ChangeSheet";
import "../apps/Apps.css";
import "./Backups.css";

/** What a whole restore of a shared server asks to be typed: the server's name, from what it says. */
function nameToType(said: string): string {
  return /Type (\S+) to confirm/.exec(said)?.[1] ?? "";
}

/** The name a restore into a new database suggests: the original's, restored, the next that's free. */
export function restoredName(project: string, taken: readonly string[]): string {
  const first = `${project}-restored`;
  return taken.includes(first) ? nextFree(first, taken) : first;
}

/** How long a typed name settles before the control plane is asked whether it's free. */
const NAME_SETTLES_MS = 300;

/**
 * Restores a backup of a whole volume as a plan. A backup of a catalog database asks
 * first where to, of whoever may make a project (an admin, as an install takes): a
 * new database beside it, which is the choice it starts on, or in place of what it
 * holds (ADR 0106). Anyone else replaces what it holds, as before. A database server other projects' apps share
 * asks for its name typed before it's replaced, as a whole restore takes back what
 * each wrote since, which it says (ADR 0084).
 */
export function useRestoreWhole(): { restore: (backup: Backup) => void; confirming: ReactNode } {
  const { t } = useLingui();
  const places = usePlaces();
  const installs = useQuery(CatalogService.method.listCatalogInstalls, {}).data?.installs;
  // Which installs are a database entry, as the catalog's category says, as the database's page reads it.
  const ids = useMemo(() => [...new Set((installs ?? []).map((i) => i.pin?.entry ?? "").filter(Boolean))], [installs]);
  const entries = useQuery(
    CatalogService.method.listCatalogEntries,
    { ids },
    { enabled: ids.length > 0, staleTime: 30_000 },
  ).data?.entries;
  const mayInstall = useMay()(CatalogService.method.installCatalogEntry).allowed;
  const [choosing, setChoosing] = useState<{ backup: Backup; project: string; entry: string; catalog: string }>();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const [asking, setAsking] = useState<{ id: string; said: string; name: string }>();
  const [typed, setTyped] = useState("");
  const client = createClient(BackupService, transport);
  const replace = (id: string) => {
    void (async () => {
      try {
        const planned = await client.restoreBackup({ id });
        sheet.open({ kind: "plan", make: () => Promise.resolve(planned) });
      } catch (error) {
        if (reasonOf(error) === "confirm_name") {
          const said = sentence(error);
          setTyped("");
          setAsking({ id, said, name: nameToType(said) });
          return;
        }
        sheet.open({ kind: "plan", make: () => Promise.reject(error) });
      }
    })();
  };
  // A new database is the catalog entry the original was installed from, as it runs,
  // so it's offered for a backup of a catalog database's whole volume alone: not one
  // app's database, not an app that ships its own database, and not one whose install
  // runs with what other apps give it. A new database is a new project, which only an
  // admin makes.
  const restore = (backup: Backup) => {
    const place = places.place(backup.environmentId);
    const install = installs?.find((i) => i.environmentId === backup.environmentId);
    const offered =
      mayInstall &&
      backup.database !== "" &&
      backup.databaseName === "" &&
      install !== undefined &&
      entries?.find((e) => e.id === install.pin?.entry)?.category === "databases" &&
      Object.keys(install.pin?.needs ?? {}).length === 0;
    if (!offered || !place || !install) {
      replace(backup.id);
      return;
    }
    setChoosing({
      backup,
      project: place.project,
      entry: install.pin?.entry ?? "",
      catalog: install.pin?.catalog ?? "",
    });
  };
  const close = () => setAsking(undefined);
  const server = asking?.name ?? "";
  const confirming = asking ? (
    <Sheet
      open
      onClose={close}
      title={t`Restore ${server} whole`}
      footer={
        <>
          <Button variant="secondary" onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="danger"
            disabled={typed !== asking.name}
            onClick={() => {
              const { id, name } = asking;
              close();
              sheet.open({ kind: "plan", make: () => client.restoreBackup({ id, confirm: name }) });
            }}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <p>{asking.said}</p>
      <TextField
        label={t`Type ${server} to confirm`}
        value={typed}
        onChange={(e) => setTyped(e.currentTarget.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </Sheet>
  ) : choosing ? (
    <RestoreSheet
      backup={choosing.backup}
      project={choosing.project}
      entry={choosing.entry}
      catalog={choosing.catalog}
      taken={places.projects}
      onClose={() => setChoosing(undefined)}
      onReplace={() => {
        const { id } = choosing.backup;
        setChoosing(undefined);
        replace(id);
      }}
      onInto={(into) => {
        const { id } = choosing.backup;
        setChoosing(undefined);
        sheet.open({ kind: "plan", make: () => client.restoreBackup({ id, into }) });
      }}
    />
  ) : null;
  return { restore, confirming };
}

/**
 * Where a backup of a catalog database restores to, as Backup-Restore lays it out:
 * a new database beside the one that runs, chosen first, with its name filled in and
 * checked as it's typed, or in place of what that one holds (ADR 0106).
 */
function RestoreSheet({
  backup,
  project,
  entry,
  catalog,
  taken,
  onClose,
  onReplace,
  onInto,
}: {
  backup: Backup;
  project: string;
  /** The catalog entry it was installed from, and its catalog, which say what names are free. */
  entry: string;
  catalog: string;
  taken: readonly string[];
  onClose: () => void;
  onReplace: () => void;
  onInto: (into: string) => void;
}) {
  const { t, i18n } = useLingui();
  const [to, setTo] = useState<"new" | "replace">("new");
  const [name, setName] = useState(() => restoredName(project, taken));
  const typed = name.trim();
  const when = backup.startedAt ? formatDateTime(timestampDate(backup.startedAt), i18n.locale) : "";
  const size = formatBytes(backup.sizeBytes, i18n.locale);
  // A name is checked as it's typed: against the projects there are at once, then by
  // the control plane, which knows the names used before, whose data may come back.
  const [settled, setSettled] = useState(typed);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(typed), NAME_SETTLES_MS);
    return () => clearTimeout(timer);
  }, [typed]);
  const valid = typed !== "" && SERVICE_NAME.test(typed);
  const here = taken.includes(typed);
  const asked = to === "new" && valid && !here && entry !== "" && settled === typed;
  const answer = useQuery(
    CatalogService.method.getCatalogEntry,
    { id: entry, catalog, project: settled },
    { enabled: asked },
  );
  // The entry may have left the catalog, which the plan's own check then covers.
  const checking =
    to === "new" &&
    valid &&
    !here &&
    entry !== "" &&
    !answer.isError &&
    (settled !== typed || answer.data === undefined);
  const before = asked && answer.data !== undefined && answer.data.project !== typed;
  const free = here ? nextFree(typed, taken) : before ? (answer.data?.project ?? "") : "";
  const problem =
    to !== "new"
      ? undefined
      : typed === ""
        ? t`Name the new database.`
        : !SERVICE_NAME.test(typed)
          ? t`Use lowercase letters, digits and dashes, starting with a letter, such as shop.`
          : here
            ? t`A project named ${typed} is here already. ${free} is free.`
            : before
              ? t`A project named ${typed} was here before, and its data may come back. ${free} is free.`
              : undefined;
  const choices = [
    {
      value: "new" as const,
      label: (
        <>
          <Trans>To a new database</Trans> <Tag words>{t`Recommended`}</Tag>
        </>
      ),
      hint: t`${project} keeps running. Check the data, then switch apps over.`,
    },
    {
      value: "replace" as const,
      label: <Trans>Replace {project}</Trans>,
      hint: t`What uses it stops while it loads, and what was made since goes.`,
    },
  ];
  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Restore ${project}`}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            disabled={problem !== undefined || checking}
            onClick={() => (to === "new" ? onInto(typed) : onReplace())}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <p className="restore-point">
        <Icon name="clock" size={16} />
        <span>{t`The backup from ${when}, ${size}`}</span>
      </p>
      <fieldset className="app-front">
        <legend className="dw-field-label">
          <Trans>Restore to</Trans>
        </legend>
        {choices.map((choice) => (
          <label key={choice.value} className={to === choice.value ? "app-front-choice is-on" : "app-front-choice"}>
            <input type="radio" name="restore-to" checked={to === choice.value} onChange={() => setTo(choice.value)} />
            <span className="app-front-text">
              <span className="app-front-name">{choice.label}</span>
              <span className="app-front-hint">{choice.hint}</span>
            </span>
          </label>
        ))}
      </fieldset>
      {to === "new" ? (
        <TextField
          label={t`Name`}
          value={name}
          error={problem}
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setName(e.currentTarget.value)}
        />
      ) : null}
    </Sheet>
  );
}
