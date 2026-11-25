// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  type Database,
  DatabaseEngine,
  type DatabaseReading,
  DatabaseService,
  StreamError,
  uploadDatabaseImportPart,
} from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, SelectField, Sheet, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useCallback, useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useShownName } from "../apps/names";
import { useMay } from "../auth/may";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { engineName } from "./useDatabases";
import "../apps/Apps.css";

/** Formats accepted per engine. */
function acceptedExtensions(engine: DatabaseEngine): readonly string[] {
  return engine === DatabaseEngine.POSTGRESQL ? [".sql", ".sql.gz", ".dump", ".tar"] : [".sql", ".sql.gz"];
}

function matchesAccepted(filename: string, extensions: readonly string[]): boolean {
  const lower = filename.toLowerCase();
  return extensions.some((ext) => lower.endsWith(ext));
}

/**
 * Stages a database dump file in bounded chunks through the streaming upload
 * path, then plans an import that backs the chosen database up first.
 */
export function ImportDumpSheet({
  db,
  read,
  initialDatabase,
  onClose,
}: {
  db: Database;
  read: DatabaseReading;
  initialDatabase?: string;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const may = useMay();
  const environment = db.resource?.scope ?? "";
  const mayBegin = may(DatabaseService.method.beginDatabaseImport, { environment });
  const mayUpload = may(DatabaseService.method.uploadDatabaseImport, { environment });
  const mayPlan = may(DatabaseService.method.planDatabaseImport, { environment });
  const mayCancel = may(DatabaseService.method.cancelDatabaseImport, { environment });
  const mayImport = !mayBegin.allowed ? mayBegin : !mayUpload.allowed ? mayUpload : mayPlan;
  const name = useShownName()(db, "database").title;
  const engine = engineName(db.engine);
  const extensions = acceptedExtensions(db.engine);
  const ownDatabases = read.databases.filter((d) => !d.system);
  const defaultDB =
    (initialDatabase && ownDatabases.some((d) => d.name === initialDatabase)
      ? initialDatabase
      : ownDatabases[0]?.name) ?? "";

  const [database, setDatabase] = useState(defaultDB);
  const [file, setFile] = useState<File>();
  const [fileError, setFileError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [uploading, setUploading] = useState(false);
  const [sentBytes, setSentBytes] = useState(0);
  const [plan, setPlan] = useState<Opening>();
  const abortRef = useRef<AbortController | null>(null);
  const stagedIdRef = useRef<string>("");
  const plannedChangeIdRef = useRef<string>("");
  const transportRef = useRef(transport);
  transportRef.current = transport;
  const mayCancelRef = useRef(mayCancel.allowed);
  mayCancelRef.current = mayCancel.allowed;
  const dbIdRef = useRef(db.id);
  dbIdRef.current = db.id;

  const cancelStaged = useCallback((importId: string) => {
    if (!importId || !mayCancelRef.current) return;
    stagedIdRef.current = "";
    void createClient(DatabaseService, transportRef.current)
      .cancelDatabaseImport({ id: dbIdRef.current, importId })
      .catch(() => {});
  }, []);

  const discardPlanned = useCallback((changeId: string) => {
    if (!changeId) return;
    plannedChangeIdRef.current = "";
    void createClient(ChangeService, transportRef.current)
      .discardChange({ id: changeId })
      .catch(() => {});
  }, []);

  useEffect(() => {
    return () => {
      abortRef.current?.abort();
      if (plannedChangeIdRef.current) discardPlanned(plannedChangeIdRef.current);
      if (stagedIdRef.current) cancelStaged(stagedIdRef.current);
    };
  }, [cancelStaged, discardPlanned]);

  const handleClose = () => {
    abortRef.current?.abort();
    if (plannedChangeIdRef.current) discardPlanned(plannedChangeIdRef.current);
    if (stagedIdRef.current) cancelStaged(stagedIdRef.current);
    onClose();
  };

  const onFilePick = (picked: File | undefined) => {
    setFile(picked);
    setFailure(undefined);
    if (!picked) {
      setFileError(undefined);
      return;
    }
    if (picked.size === 0) {
      setFileError(t`That dump file is empty. Choose a non-empty file.`);
      return;
    }
    if (!matchesAccepted(picked.name, extensions)) {
      const list = extensions.join(", ");
      setFileError(t`${engine} accepts ${list} dumps. Choose a matching file.`);
      return;
    }
    setFileError(undefined);
  };

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (!database) {
      setFailure(t`Create a database to import into.`);
      return;
    }
    if (!file) {
      setFileError(t`Choose a dump file to import.`);
      return;
    }
    if (fileError) return;

    const controller = new AbortController();
    abortRef.current = controller;
    setUploading(true);
    setSentBytes(0);
    setFailure(undefined);

    const client = createClient(DatabaseService, transport);
    let importId = "";
    try {
      const began = await client.beginDatabaseImport(
        {
          id: db.id,
          database,
          filename: file.name,
          sizeBytes: BigInt(file.size),
        },
        { signal: controller.signal },
      );
      importId = began.importId;
      stagedIdRef.current = importId;
      const partBytes = Math.max(256 * 1024, Number(began.partBytes || 8n * 1024n * 1024n));
      let offset = Number(began.offset);
      setSentBytes(offset);

      while (offset < file.size) {
        const end = Math.min(file.size, offset + partBytes);
        const chunk = file.slice(offset, end);
        const chunkStart = offset;
        const ack = await uploadDatabaseImportPart({
          id: db.id,
          importId,
          offset: chunkStart,
          part: chunk,
          signal: controller.signal,
          onSent: (loaded) => setSentBytes(Math.min(file.size, chunkStart + loaded)),
        });
        offset = Number(ack.offset);
        setSentBytes(offset);
        if (ack.complete) break;
      }

      const planned = await client.planDatabaseImport({ id: db.id, importId }, { signal: controller.signal });
      const change: Change | undefined = planned.change;
      plannedChangeIdRef.current = change?.id ?? "";
      setUploading(false);
      abortRef.current = null;
      setPlan({
        kind: "plan",
        make: () => Promise.resolve({ change }),
        retryLabel: t`Choose another file`,
        onDone: () => {
          stagedIdRef.current = "";
          plannedChangeIdRef.current = "";
          onClose();
        },
        onClose: (after) => {
          if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.FAILED) {
            setPlan(undefined);
            if ((!after || after.state === ChangeState.PLANNED) && plannedChangeIdRef.current) {
              discardPlanned(plannedChangeIdRef.current);
            } else {
              plannedChangeIdRef.current = "";
            }
            if (stagedIdRef.current && after?.state !== ChangeState.APPLIED) {
              cancelStaged(stagedIdRef.current);
            }
          } else {
            plannedChangeIdRef.current = "";
            if (after.state === ChangeState.DISCARDED && stagedIdRef.current) {
              cancelStaged(stagedIdRef.current);
            } else {
              stagedIdRef.current = "";
            }
            onClose();
          }
        },
      });
    } catch (err) {
      setUploading(false);
      abortRef.current = null;
      if (err instanceof DOMException && err.name === "AbortError") {
        if (importId) cancelStaged(importId);
        return;
      }
      if (importId) cancelStaged(importId);
      if (err instanceof StreamError) {
        setFailure(err.message || t`The upload was interrupted. Try again.`);
      } else {
        setFailure(sentence(err));
      }
    }
  };

  const pct = file && file.size > 0 ? Math.min(100, Math.round((sentBytes / file.size) * 100)) : 0;

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      title={t`Import dump into ${name}`}
      subtitle={t`Backs up the chosen database first, then loads as its owner. Admins only.`}
      onClose={handleClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes in the database until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={handleClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={uploading}
            disabled={uploading || !file || Boolean(fileError) || ownDatabases.length === 0}
            onClick={() => void submit()}
            {...mayImport.off}
          >
            {uploading ? <Trans>Uploading…</Trans> : <Trans>Upload and review plan</Trans>}
          </Button>
        </>
      }
    >
      <form className="app-form" onSubmit={(event) => void submit(event)}>
        <SelectField
          label={t`Target database`}
          value={database}
          disabled={uploading || ownDatabases.length === 0}
          options={ownDatabases.map((d) => ({ value: d.name, label: d.name }))}
          hint={t`Backed up automatically before the dump loads.`}
          onChange={(event) => setDatabase(event.target.value)}
        />
        <div className="dw-field">
          <label className="dw-label" htmlFor="database-import-file">
            <Trans>Dump file</Trans>
          </label>
          <input
            id="database-import-file"
            type="file"
            accept={extensions.join(",")}
            disabled={uploading}
            onChange={(event) => onFilePick(event.target.files?.[0])}
          />
          <p className="dw-hint">
            {file
              ? t`${file.name} (${formatBytes(BigInt(file.size), i18n.locale)})`
              : t`Accepts ${extensions.join(", ")}.`}
          </p>
          {fileError ? (
            <p className="dw-field-error" role="alert">
              {fileError}
            </p>
          ) : null}
        </div>
        {uploading && file ? (
          <div className="dw-field" aria-live="polite">
            <div className="databases-section-head">
              <span>{t`Uploading ${file.name}`}</span>
              <span className="databases-muted">
                {formatBytes(BigInt(sentBytes), i18n.locale)} / {formatBytes(BigInt(file.size), i18n.locale)} ({pct}%)
              </span>
            </div>
            <progress max={file.size} value={sentBytes} className="database-import-progress" />
          </div>
        ) : null}
        {failure ? (
          <Callout tone="critical" title={t`This dump can't be imported`}>
            {failure}
          </Callout>
        ) : null}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}
