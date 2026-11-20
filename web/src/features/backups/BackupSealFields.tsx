// SPDX-License-Identifier: AGPL-3.0-only

import { Advanced, Callout, Switch, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { CopyButton } from "../system/CopyCommand";
import "./Backups.css";

/** A passphrase no one chose: 32 random bytes, as letters and digits a person can copy. */
export function makePassphrase(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  return btoa(String.fromCharCode(...bytes)).replace(/[+/=]/g, "");
}

export interface BackupSealState {
  prefix: string;
  setPrefix: (value: string) => void;
  made: string;
  own: boolean;
  setOwn: (value: boolean) => void;
  typed: string;
  setTyped: (value: string) => void;
  saved: boolean;
  setSaved: (value: boolean) => void;
  passphrase: string;
  ready: boolean;
}

/** State for sealing backups in a bucket: folder prefix (defaults to "backups") and passphrase. */
export function useBackupSeal(defaultPrefix = "backups"): BackupSealState {
  const [prefix, setPrefix] = useState(defaultPrefix);
  const [made] = useState(makePassphrase);
  const [own, setOwn] = useState(false);
  const [typed, setTyped] = useState("");
  const [saved, setSaved] = useState(false);
  const passphrase = own ? typed : made;
  const ready = own ? typed.length >= 16 : saved;
  return { prefix, setPrefix, made, own, setOwn, typed, setTyped, saved, setSaved, passphrase, ready };
}

/**
 * Passphrase and folder prefix fields shown inline when picking an unused S3
 * connection for backups: defaults first (generated passphrase + "backups" prefix
 * tucked under Advanced), and a switch for an existing passphrase.
 */
export function BackupSealFields({ seal }: { seal: BackupSealState }) {
  const { t } = useLingui();
  const folder = seal.prefix.trim().replace(/^\/+|\/+$/g, "");
  const summary = [
    folder ? t`in ${folder}/` : t`at the bucket's top`,
    seal.own ? t`your passphrase` : t`a passphrase made for you`,
  ].join(" · ");

  return (
    <div className="backup-seal">
      {seal.own ? null : (
        <Callout tone="warning" title={t`Keep this passphrase somewhere else`}>
          <p className="backup-sheet-line">{t`Nothing else opens these backups, and Dockwell shows it only now.`}</p>
          <code className="backup-passphrase">
            {Array.from(seal.made.matchAll(/.{1,6}/g), (group) => (
              <span key={group.index}>{group[0]}</span>
            ))}
          </code>
          <CopyButton
            text={seal.made}
            label={t`Copy the passphrase`}
            copiedLabel={t`Passphrase copied`}
            variant="secondary"
          />
          <label className="backup-saved">
            <input type="checkbox" checked={seal.saved} onChange={(e) => seal.setSaved(e.target.checked)} />
            <Trans>I've kept a copy of the passphrase</Trans>
          </label>
        </Callout>
      )}
      <Advanced summary={summary}>
        <TextField
          label={t`Folder in the bucket`}
          mono
          optional
          value={seal.prefix}
          placeholder="backups"
          onChange={(event) => seal.setPrefix(event.target.value)}
        />
        <Switch
          checked={seal.own}
          onChange={seal.setOwn}
          label={t`Use a passphrase I already have`}
          description={t`For storage an earlier install sealed with it.`}
        />
        {seal.own ? (
          <TextField
            label={t`Passphrase`}
            mono
            type="password"
            autoComplete="off"
            value={seal.typed}
            hint={t`At least 16 characters.`}
            onChange={(event) => seal.setTyped(event.target.value)}
          />
        ) : null}
      </Advanced>
    </div>
  );
}
