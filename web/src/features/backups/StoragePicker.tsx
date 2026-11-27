// SPDX-License-Identifier: AGPL-3.0-only

import type { ConnectionInfo } from "@dockwell/api";
import { BrandMark, Button, cx } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useId } from "react";

import type { Offer } from "../auth/may";
import { ConnectionMark, DestinationMark } from "../connections/ProviderMark";
import { pickerStatus, type StorageItem } from "./storage";
import "./Backups.css";

/**
 * Picks where backups send a copy: S3 connections first, then destinations with
 * their own credentials, then local-only. Connections serving an app or whose keys an app
 * received stay visible and disabled with why, with the action that makes one
 * backups can use (ADR 0099, #831).
 */
export function StoragePicker({
  items,
  value,
  onChange,
  noneLabel,
  noneHint,
  mayPlanInstall,
  onAdd,
  onNewFrom,
  onRotateKeys,
}: {
  items: readonly StorageItem[];
  value: string;
  onChange: (value: string) => void;
  noneLabel: string;
  noneHint: string;
  mayPlanInstall: Offer;
  onAdd: () => void;
  onNewFrom: (connection: ConnectionInfo) => void;
  onRotateKeys: (connection: ConnectionInfo) => void;
}) {
  const { t, i18n } = useLingui();
  const groupId = useId();
  const listFormat = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" });

  // S3 connections first (usable before disabled), then destinations with their own credentials.
  const connItems = items.filter((i) => i.connection !== undefined);
  const standaloneItems = items.filter((i) => i.connection === undefined);
  const usableConns = connItems.filter((i) => {
    const s = pickerStatus(i).state;
    return s === "ready" || s === "available";
  });
  const blockedConns = connItems.filter((i) => {
    const s = pickerStatus(i).state;
    return s !== "ready" && s !== "available";
  });
  const ordered = [...usableConns, ...blockedConns, ...standaloneItems];

  return (
    <fieldset className="backup-storage-picker">
      <legend className="dw-field-label">
        <Trans>Send a copy to</Trans>
      </legend>
      <div className="backup-storage-options">
        {ordered.map((item) => {
          const status = pickerStatus(item);
          const conn = item.connection;
          const label = item.label;
          const host = item.host;
          const title = host ? t`Use ${label} (${host})` : t`Use ${label}`;

          let disabled = false;
          let detail = "";
          let action: React.ReactNode = null;

          if (status.state === "ready") {
            const prefix = item.destination?.prefix.trim().replace(/^\/+|\/+$/g, "");
            if (item.destination?.sftp) {
              detail = t`SFTP · ${item.destination.sftp.path}`;
            } else if (conn) {
              detail = prefix ? t`Encrypted under ${prefix}/` : t`Encrypted before upload`;
            } else if (item.bucket) {
              detail = prefix ? t`Bucket ${item.bucket} · encrypted under ${prefix}/` : t`Bucket ${item.bucket}`;
            } else {
              detail = prefix ? t`Encrypted under ${prefix}/` : t`Encrypted before upload`;
            }
          } else if (status.state === "available") {
            if (!mayPlanInstall.allowed) {
              disabled = true;
              detail = mayPlanInstall.reason || t`An admin sets up backup storage`;
            } else {
              detail = item.bucket ? t`Bucket ${item.bucket} · sealed with a passphrase` : t`Sealed with a passphrase`;
            }
          } else if (status.state === "serves-app" && conn) {
            disabled = true;
            const appNames = status.apps.map((a) => a.name);
            const who = appNames.length > 0 ? listFormat.format(appNames) : t`an app`;
            detail = t`Serves ${who}, whose key could delete the backups`;
            action = (
              <Button size="sm" variant="secondary" onClick={() => onNewFrom(conn)} {...mayPlanInstall.off}>
                <Trans>New connection from it</Trans>
              </Button>
            );
          } else if (status.state === "shared" && conn) {
            disabled = true;
            detail = t`Shared for apps, whose keys could delete the backups`;
            action = (
              <Button size="sm" variant="secondary" onClick={() => onNewFrom(conn)} {...mayPlanInstall.off}>
                <Trans>New connection from it</Trans>
              </Button>
            );
          } else if (status.state === "held" && conn) {
            disabled = true;
            detail = t`An app received this connection's keys`;
            action = (
              <Button size="sm" variant="secondary" onClick={() => onRotateKeys(conn)} {...mayPlanInstall.off}>
                <Trans>Paste a key made for backups</Trans>
              </Button>
            );
          }

          const picked = value === item.key && !disabled;
          return (
            <div key={item.key} className={cx("backup-storage-option", picked && "is-picked", disabled && "is-off")}>
              <label className="backup-storage-choice">
                <input
                  type="radio"
                  name={groupId}
                  value={item.key}
                  checked={picked}
                  disabled={disabled}
                  onChange={() => onChange(item.key)}
                />
                {conn ? (
                  <ConnectionMark connection={conn} />
                ) : item.destination ? (
                  <DestinationMark destination={item.destination} />
                ) : (
                  <BrandMark icon="archive" />
                )}
                <span className="backup-storage-text">
                  <span className="backup-storage-name">{title}</span>
                  {detail ? <span className="backup-storage-why">{detail}</span> : null}
                </span>
              </label>
              {action ? <div className="backup-storage-action">{action}</div> : null}
            </div>
          );
        })}

        <div className={cx("backup-storage-option", value === "" && "is-picked")}>
          <label className="backup-storage-choice">
            <input type="radio" name={groupId} value="" checked={value === ""} onChange={() => onChange("")} />
            <BrandMark icon="server" />
            <span className="backup-storage-text">
              <span className="backup-storage-name">{noneLabel}</span>
              <span className="backup-storage-why">{noneHint}</span>
            </span>
          </label>
        </div>
      </div>
      <div className="backup-storage-foot">
        <Button size="sm" variant="ghost" icon="plus" onClick={onAdd} {...mayPlanInstall.off}>
          <Trans>Add storage</Trans>
        </Button>
      </div>
    </fieldset>
  );
}
