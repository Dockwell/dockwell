// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type BackupDestinationInfo, BackupService, ChangeService } from "@dockwell/api";
import { Button, Loading, Panel, Sheet, SkeletonRows, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { StateLine } from "../apps/DomainStep";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { ConnectionMark, DestinationMark } from "../connections/ProviderMark";
import { ListPager } from "../shell/ListTools";
import { pagedRows } from "../shell/list";
import { CopyButton } from "../system/CopyCommand";
import { SftpDestinationSheet } from "./SftpDestinationSheet";
import { pickerStatus, useStorage } from "./storage";
import "./Backups.css";

/** Destinations per page in the side panel, keeping Recent runs close to the top. */
const DESTINATIONS_PAGE_SIZE = 8;

/** Show the search box once there are more destinations than fit at a glance. */
const DESTINATIONS_FILTER_FROM = 6;

/** The storage backups can go to. */
export function useDestinations() {
  return useQuery(BackupService.method.listBackupDestinations, {});
}

/** Storage where backups go: backup destinations and unused S3 connections that can take backups. */
export function DestinationsPanel({
  destinations,
  pending = false,
  offerSFTP = false,
}: {
  destinations: readonly BackupDestinationInfo[];
  /** The destinations are on their way, so none isn't known to be none yet. */
  pending?: boolean;
  offerSFTP?: boolean;
}) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const storage = useStorage(destinations);
  const [adding, setAdding] = useState<"choose" | "s3" | "sftp">();
  const [editing, setEditing] = useState<BackupDestinationInfo>();
  const add = () => setAdding(offerSFTP ? "choose" : "s3");
  const [typed, setTyped] = useState("");
  const [pageNumber, setPageNumber] = useState(0);
  // Storage belongs to no project, so adding or removing it is an admin's: off, with why, otherwise (ADR 0059).
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  // Leave out connections that can't take backups (app-serving, shared, or key-held); the picker shows them disabled with the way out.
  const usable = useMemo(
    () =>
      storage.items.filter((item) => {
        const state = pickerStatus(item).state;
        return state === "ready" || state === "available";
      }),
    [storage.items],
  );
  const query = typed.trim().toLowerCase();
  const matched = useMemo(
    () =>
      query === ""
        ? usable
        : usable.filter((item) => item.name.toLowerCase().includes(query) || item.bucket.toLowerCase().includes(query)),
    [usable, query],
  );
  const page = pagedRows(matched, pageNumber, DESTINATIONS_PAGE_SIZE);
  const showSearch = usable.length >= DESTINATIONS_FILTER_FROM || typed !== "";

  return (
    <Panel
      title={t`Destinations`}
      flush
      actions={
        // While there's none, the empty state's Add storage is the one way in.
        usable.length > 0 ? (
          <Button size="sm" variant="ghost" icon="plus" onClick={add} {...mayPlan.off}>
            <Trans>Add</Trans>
          </Button>
        ) : undefined
      }
    >
      {pending || storage.isPending ? (
        <Loading label={t`Loading destinations`}>
          <SkeletonRows rows={2} lead="mark" />
        </Loading>
      ) : usable.length === 0 ? (
        // A state with its one action, not an empty page, so this column stays as short as its neighbour (#793).
        <div className="backup-destinations-none">
          <StateLine
            status="degraded"
            action={
              <button
                type="button"
                className="domain-step-act"
                onClick={add}
                disabled={!mayPlan.allowed}
                title={mayPlan.reason || undefined}
              >
                <Trans>Add storage</Trans>
              </button>
            }
          >
            {t`Only on each server's disk, unencrypted`}
          </StateLine>
        </div>
      ) : (
        <>
          {showSearch ? (
            <div className="backup-destinations-search">
              <TextField
                icon="search"
                type="search"
                size="sm"
                aria-label={t`Filter destinations`}
                placeholder={t`Filter by name or bucket`}
                value={typed}
                onChange={(event) => {
                  setTyped(event.target.value);
                  setPageNumber(0);
                }}
              />
            </div>
          ) : null}
          {matched.length === 0 ? (
            <div className="backup-destinations-none">
              <StateLine
                status="unknown"
                action={
                  <button
                    type="button"
                    className="domain-step-act"
                    onClick={() => {
                      setTyped("");
                      setPageNumber(0);
                    }}
                  >
                    <Trans>Clear the filter</Trans>
                  </button>
                }
              >
                {t`No destinations match`}
              </StateLine>
            </div>
          ) : (
            <>
              <ul className="backup-policies">
                {page.rows.map((item) => {
                  const d = item.destination;
                  const c = item.connection;
                  return (
                    <li key={item.key} className="backup-policy">
                      <div className="backup-dest-head">
                        {c ? <ConnectionMark connection={c} /> : d ? <DestinationMark destination={d} /> : null}
                        <div className="backup-state">
                          {c ? (
                            <Link to="/connections/$name" params={{ name: c.name }} className="backup-dest-link">
                              {c.name}
                            </Link>
                          ) : (
                            <span>{item.name}</span>
                          )}
                          {d?.sftp ? (
                            <span className="backup-why sftp-location">
                              SFTP ·{" "}
                              {d.sftp.host.includes(":") && d.sftp.port !== 22 ? `[${d.sftp.host}]` : d.sftp.host}
                              {d.sftp.port && d.sftp.port !== 22 ? `:${d.sftp.port}` : ""}
                              {d.sftp.path}
                            </span>
                          ) : item.bucket ? (
                            <code className="backup-why">{item.bucket}</code>
                          ) : null}
                        </div>
                      </div>
                      <div className="backup-state">
                        {d ? (
                          <>
                            <span className="backup-why">{t`${d.backups} backups, ${formatBytes(d.usedBytes, i18n.locale)}`}</span>
                            {d.lastUpload ? (
                              <span className="backup-why">
                                {t`Last upload ${formatDateTime(timestampDate(d.lastUpload), i18n.locale)}`}
                              </span>
                            ) : null}
                            <span className="backup-why">{t`Encrypted on each server before upload`}</span>
                            {d.sftp?.hostKeyFingerprint ? (
                              <code className="backup-why sftp-location">
                                {d.sftp.hostKeyAlgorithm} {d.sftp.hostKeyFingerprint}
                              </code>
                            ) : null}
                            {d.sftp?.publicKey ? (
                              <div className="sftp-public-key">
                                <code className="backup-why" title={d.sftp.publicKey}>
                                  {d.sftp.publicKey}
                                </code>
                                <CopyButton
                                  text={d.sftp.publicKey}
                                  label={t`Copy public key`}
                                  copiedLabel={t`Public key copied`}
                                />
                              </div>
                            ) : null}
                          </>
                        ) : (
                          <span className="backup-why">{t`Connection · pick in a schedule or Back up now`}</span>
                        )}
                      </div>
                      {d ? (
                        <div className="sftp-destination-actions">
                          {offerSFTP && d.sftp ? (
                            <Button
                              size="sm"
                              variant="ghost"
                              aria-label={t`Edit this SFTP destination`}
                              {...mayPlan.off}
                              onClick={() => setEditing(d)}
                            >
                              <Trans>Edit</Trans>
                            </Button>
                          ) : null}
                          <Button
                            size="sm"
                            variant="ghost"
                            icon="trash"
                            aria-label={t`Remove this destination`}
                            {...mayPlan.off}
                            onClick={() =>
                              sheet.open({
                                kind: "propose",
                                summary: t`Stop sending backups to ${d.name}`,
                                edits: [{ resource: { kind: "backup.destination", name: d.name }, delete: true }],
                              })
                            }
                          />
                        </div>
                      ) : c ? (
                        <Link
                          to="/connections/$name"
                          params={{ name: c.name }}
                          className="dw-btn dw-btn-ghost dw-btn-sm"
                        >
                          <Trans>Manage</Trans>
                        </Link>
                      ) : null}
                    </li>
                  );
                })}
              </ul>
              {page.pages > 1 ? (
                <div className="backup-destinations-pager">
                  <ListPager page={page} onPage={setPageNumber} />
                </div>
              ) : null}
            </>
          )}
        </>
      )}
      {adding === "choose" ? (
        <Sheet open title={t`Add backup storage`} onClose={() => setAdding(undefined)}>
          <div className="backup-storage-options">
            <div className="backup-storage-type">
              <Button onClick={() => setAdding("s3")}>
                <Trans>S3-compatible</Trans>
              </Button>
              <span>
                <Trans>Store backups in an object storage bucket.</Trans>
              </span>
            </div>
            <div className="backup-storage-type">
              <Button onClick={() => setAdding("sftp")}>SFTP</Button>
              <span>
                <Trans>Store backups in a folder on an SSH server.</Trans>
              </span>
            </div>
          </div>
        </Sheet>
      ) : null}
      {adding === "s3" ? (
        <ConnectionSheet
          kind="storage"
          existing={storage.allConnections}
          onClose={() => setAdding(undefined)}
          onSaved={() => void storage.refetch()}
        />
      ) : null}
      {adding === "sftp" || editing ? (
        <SftpDestinationSheet
          editing={editing}
          close={() => {
            setAdding(undefined);
            setEditing(undefined);
          }}
          onSaved={() => void storage.refetch()}
        />
      ) : null}
    </Panel>
  );
}
