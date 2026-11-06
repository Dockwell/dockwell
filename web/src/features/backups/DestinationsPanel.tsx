// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type BackupDestinationInfo, BackupService } from "@dockwell/api";
import { Button, Callout, Panel, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { useChangeSheet } from "../changes/ChangeSheet";
import "./Backups.css";

/** The storage backups can go to. */
export function useDestinations() {
  return useQuery(BackupService.method.listBackupDestinations, {});
}

/** The names a form offers: none is a choice too, so it starts there. */
export function destinationOptions(list: readonly BackupDestinationInfo[], none: string) {
  return [{ value: "", label: none }, ...list.map((d) => ({ value: d.name, label: d.name }))];
}

/** Each destination: where it is, what's there, and when something last got there. */
export function DestinationsPanel({ destinations }: { destinations: readonly BackupDestinationInfo[] }) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const [adding, setAdding] = useState(false);
  return (
    <Panel
      title={t`Destinations`}
      flush
      actions={
        <Button size="sm" variant="ghost" icon="plus" onClick={() => setAdding(true)}>
          <Trans>Add</Trans>
        </Button>
      }
    >
      {destinations.length === 0 ? (
        <p className="backup-policy-empty">
          <Trans>
            Backups stay on each server's disk until you add somewhere else to keep them. Any S3-compatible storage
            works, such as Cloudflare R2, Backblaze B2 or Amazon S3.
          </Trans>
        </p>
      ) : (
        <ul className="backup-policies">
          {destinations.map((d) => (
            <li key={d.id} className="backup-policy">
              <div className="backup-state">
                <span>{d.name}</span>
                <code className="backup-why">{d.bucket}</code>
              </div>
              <div className="backup-state">
                <span className="backup-why">{t`${d.backups} backups, ${formatBytes(d.usedBytes, i18n.locale)}`}</span>
                {d.lastUpload ? (
                  <span className="backup-why">
                    {t`Last upload ${formatDateTime(timestampDate(d.lastUpload), i18n.locale)}`}
                  </span>
                ) : null}
                <span className="backup-why">{t`Encrypted on each server before upload`}</span>
              </div>
              <Button
                size="sm"
                variant="ghost"
                icon="trash"
                aria-label={t`Remove this destination`}
                onClick={() =>
                  sheet.open({
                    kind: "propose",
                    summary: t`Stop sending backups to ${d.name}`,
                    edits: [{ resource: { kind: "backup.destination", name: d.name }, delete: true }],
                  })
                }
              />
            </li>
          ))}
        </ul>
      )}
      {adding ? <DestinationSheet onClose={() => setAdding(false)} /> : null}
    </Panel>
  );
}

/** A passphrase no one chose: 32 random bytes, as letters and digits a person can copy. */
function makePassphrase(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  return btoa(String.fromCharCode(...bytes)).replace(/[+/=]/g, "");
}

/** Adds storage. What signs in goes into the plan's change sealed, and shows nowhere. */
function DestinationSheet({ onClose }: { onClose: () => void }) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const [name, setName] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [bucket, setBucket] = useState("");
  const [region, setRegion] = useState("");
  const [accessKey, setAccessKey] = useState("");
  const [secretKey, setSecretKey] = useState("");
  const [passphrase] = useState(makePassphrase);
  const [saved, setSaved] = useState(false);
  const ready = name !== "" && endpoint !== "" && bucket !== "" && accessKey !== "" && secretKey !== "" && saved;
  const review = () =>
    sheet.open({
      kind: "propose",
      summary: t`Send backups to ${name}`,
      edits: [
        {
          resource: { kind: "backup.destination", name },
          spec: {
            kind: { case: "backupDestination", value: { endpoint, bucket, region, accessKey, secretKey, passphrase } },
          },
        },
      ],
      onClose: (change) => {
        if (change) onClose();
      },
    });
  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Add a destination`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!ready} onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        <TextField label={t`Name`} mono value={name} placeholder="r2" onChange={(e) => setName(e.target.value)} />
        <TextField
          label={t`Endpoint`}
          mono
          value={endpoint}
          placeholder="https://<account>.r2.cloudflarestorage.com"
          onChange={(e) => setEndpoint(e.target.value)}
        />
        <TextField label={t`Bucket`} mono value={bucket} onChange={(e) => setBucket(e.target.value)} />
        <TextField
          label={t`Region`}
          mono
          value={region}
          optional
          hint={t`Leave it out for storage that has none, such as R2.`}
          onChange={(e) => setRegion(e.target.value)}
        />
        <TextField
          label={t`Access key`}
          mono
          autoComplete="off"
          value={accessKey}
          onChange={(e) => setAccessKey(e.target.value)}
        />
        <TextField
          label={t`Secret key`}
          mono
          type="password"
          autoComplete="off"
          value={secretKey}
          hint={t`Dockwell keeps it encrypted, and sends it only to a server sending a backup.`}
          onChange={(e) => setSecretKey(e.target.value)}
        />
        <Callout tone="warning" title={t`Keep this passphrase somewhere else`}>
          <p className="backup-sheet-line">
            <Trans>
              Each backup is encrypted with this before it leaves its server, so your storage provider can't read it.
              Nothing else opens one, and Dockwell shows it only now. If you lose this install, you need it to open
              what's in the bucket.
            </Trans>
          </p>
          <code className="backup-passphrase">{passphrase}</code>
        </Callout>
        <label className="backup-saved">
          <input type="checkbox" checked={saved} onChange={(e) => setSaved(e.target.checked)} />
          <Trans>I've kept a copy of the passphrase</Trans>
        </label>
      </div>
    </Sheet>
  );
}
