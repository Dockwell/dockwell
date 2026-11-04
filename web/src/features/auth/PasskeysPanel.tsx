// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, type Passkey } from "@dockwell/api";
import { Button, Icon, Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { formatDay } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { createPasskey, passkeysAvailable } from "../../lib/webauthn";
import { useControlPlaneNow } from "../servers/useFleet";
import { useDeviceName } from "./device";
import { useAuthError } from "./messages";

/** Adds a passkey for this address, and refreshes the list. */
export function useAddPasskey() {
  const transport = useTransport();
  const device = useDeviceName();
  const explain = useAuthError();
  const passkeys = useQuery(AuthService.method.listPasskeys, {});
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const run = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      const client = createClient(AuthService, transport);
      const begin = await client.beginAddPasskey({});
      const credentialJson = await createPasskey(begin.optionsJson);
      await client.finishAddPasskey({ ceremonyId: begin.ceremonyId, credentialJson, name: device });
      await passkeys.refetch();
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };
  return { busy, problem, run };
}

/** Your passkeys, which address each works on, and how to add or remove one. */
export function PasskeysPanel({ rpId }: { rpId: string }) {
  const { t } = useLingui();
  const passkeys = useQuery(AuthService.method.listPasskeys, {});
  const add = useAddPasskey();
  const canAdd = passkeysAvailable() && rpId !== "";
  // Last used is on the control plane's clock, as the server pages' times are.
  const now = useControlPlaneNow();
  return (
    <Panel
      title={t`Passkeys`}
      className="profile-panel"
      flush
      actions={
        <Button size="sm" icon="plus" loading={add.busy} disabled={!canAdd} onClick={() => void add.run()}>
          <Trans>Add passkey</Trans>
        </Button>
      }
    >
      {add.problem ? (
        <p className="profile-error" role="alert">
          {add.problem}
        </p>
      ) : null}
      {passkeys.data ? (
        <ul className="profile-list">
          {passkeys.data.passkeys.map((p) => (
            <PasskeyRow
              key={p.id}
              passkey={p}
              here={p.rpId === rpId}
              now={now}
              onRemoved={() => void passkeys.refetch()}
            />
          ))}
        </ul>
      ) : (
        <p className="profile-empty" aria-busy="true">
          <Trans>Loading passkeys</Trans>
        </p>
      )}
      <p className="profile-note">
        <Trans>Keep at least two, so losing one device never locks you out. Each works only on its address.</Trans>
      </p>
    </Panel>
  );
}

interface PasskeyRowProps {
  passkey: Passkey;
  here: boolean;
  now: number;
  onRemoved: () => void;
}

function PasskeyRow({ passkey, here, now, onRemoved }: PasskeyRowProps) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const transport = useTransport();
  const explain = useAuthError();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const name = passkey.name || t`Passkey`;
  const added = passkey.createTime ? formatDay(timestampDate(passkey.createTime), i18n.locale) : "";
  const used = passkey.lastUsedTime ? t`used ${words.ago(timestampDate(passkey.lastUsedTime), now)}` : t`never used`;
  const where = passkey.rpId;

  const remove = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(AuthService, transport).removePasskey({ id: passkey.id });
      onRemoved();
    } catch (err) {
      setProblem(explain(err));
      setConfirming(false);
    } finally {
      setBusy(false);
    }
  };

  return (
    <li className="profile-row">
      <span className="profile-row-icon" aria-hidden="true">
        <Icon name="key" size={16} />
      </span>
      <div className="profile-row-text">
        <span className="profile-row-name">{name}</span>
        <span className="profile-row-meta">
          {here ? <Trans>Works here</Trans> : <Trans>Works on {where}</Trans>} · <Trans>Added {added}</Trans> · {used}
          {passkey.synced ? (
            <>
              {" · "}
              <Trans>Synced</Trans>
            </>
          ) : null}
        </span>
        {passkey.suspectedCopy ? (
          <StatusBadge status="down" label={t`May have been copied; it no longer signs in`} />
        ) : null}
        {problem ? (
          <span className="profile-error" role="alert">
            {problem}
          </span>
        ) : null}
      </div>
      {confirming ? (
        <div className="profile-row-confirm">
          <Button size="sm" variant="danger" loading={busy} onClick={() => void remove()}>
            <Trans>Remove</Trans>
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
            <Trans>Keep</Trans>
          </Button>
        </div>
      ) : (
        <Button size="sm" variant="ghost" aria-label={t`Remove ${name}`} onClick={() => setConfirming(true)}>
          <Trans>Remove</Trans>
        </Button>
      )}
    </li>
  );
}
