// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  FirewallService,
  type Server,
  type ServerSsh,
  SshAnswer,
  type SshConfig,
  SshRootLogin,
  StepUpAction,
} from "@dockwell/api";
import { Button, Callout, SelectField, Sheet, Switch, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, type ReactNode, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useReadAgainOnceOver } from "../changes/follow";
import "./SshTools.css";

type View = "settings" | "keys" | "sessions" | "logins";

export function SshActions({ server, ssh, onChanged }: { server: Server; ssh: ServerSsh; onChanged: () => void }) {
  const { t } = useLingui();
  const [view, setView] = useState<View>();
  const may = useMay();
  const close = () => setView(undefined);
  return (
    <>
      <div className="ssh-actions">
        {ssh.dropInFolder && !ssh.editRefusal && ssh.adminAccounts.length > 0 ? (
          <Button variant="secondary" size="sm" onClick={() => setView("settings")}>
            <Trans>Edit settings</Trans>
          </Button>
        ) : null}
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setView("keys")}
          {...may(FirewallService.method.listServerSshKeys).off}
        >
          <Trans>Keys</Trans>
        </Button>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setView("sessions")}
          {...may(FirewallService.method.listServerSshSessions).off}
        >
          <Trans>Sessions</Trans>
        </Button>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setView("logins")}
          {...may(FirewallService.method.listServerSshLogins).off}
        >
          <Trans>Login log</Trans>
        </Button>
      </div>
      {ssh.editRefusal ? <p className="firewall-note">{ssh.editRefusal}</p> : null}
      {view === "settings" ? <SshSettings server={server} ssh={ssh} close={close} onChanged={onChanged} /> : null}
      {view === "keys" ? <SshKeys serverId={server.id} title={t`SSH keys on ${server.name}`} close={close} /> : null}
      {view === "sessions" ? (
        <SshSessions serverId={server.id} title={t`SSH sessions on ${server.name}`} close={close} />
      ) : null}
      {view === "logins" ? (
        <SshLogins serverId={server.id} title={t`SSH login log on ${server.name}`} close={close} />
      ) : null}
    </>
  );
}

function SshSettings({
  server,
  ssh,
  close,
  onChanged,
}: {
  server: Server;
  ssh: ServerSsh;
  close: () => void;
  onChanged: () => void;
}) {
  const { t } = useLingui();
  const changes = useChangeSheet();
  const readAgain = useReadAgainOnceOver(onChanged);
  const original: Pick<SshConfig, "rootLogin" | "passwordLogin" | "keyLogin" | "port" | "listenAddresses" | "account"> =
    ssh.managed ?? { rootLogin: "", port: 0, listenAddresses: [], account: "" };
  const rootNow = {
    [SshRootLogin.UNSPECIFIED]: "",
    [SshRootLogin.YES]: "yes",
    [SshRootLogin.KEYS_ONLY]: "prohibit-password",
    [SshRootLogin.FORCED_COMMANDS_ONLY]: "",
    [SshRootLogin.NO]: "no",
  }[ssh.rootLogin];
  const passwordNow = ssh.passwordLogin === SshAnswer.YES;
  const keyNow = ssh.keyLogin === SshAnswer.YES;
  const portNow = ssh.ports[0] ?? 22;
  const [root, setRoot] = useState(rootNow);
  const [password, setPassword] = useState(passwordNow);
  const [keys, setKeys] = useState(keyNow);
  const [port, setPort] = useState(String(portNow));
  const [addresses, setAddresses] = useState(original.listenAddresses.join(", "));
  const [account, setAccount] = useState(original.account);
  const [error, setError] = useState("");
  const addressesChanged = addresses !== original.listenAddresses.join(", ");
  const changed =
    root !== rootNow || password !== passwordNow || keys !== keyNow || port !== String(portNow) || addressesChanged;
  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    const portNumber = Number(port);
    if (!Number.isInteger(portNumber) || portNumber < 1 || portNumber > 65_535) {
      setError(t`Choose a port from 1 to 65535.`);
      return;
    }
    if (!account || !changed) return;
    const editedAddresses = addresses.split(/[\s,]+/).filter(Boolean);
    if (addressesChanged && editedAddresses.length === 0) {
      setError(t`Enter a listen address. Use 0.0.0.0 and :: for all interfaces.`);
      return;
    }
    const spec = {
      account,
      rootLogin: root === rootNow ? original.rootLogin : root,
      passwordLogin: password === passwordNow ? original.passwordLogin : password,
      keyLogin: keys === keyNow ? original.keyLogin : keys,
      port: portNumber === portNow ? original.port : portNumber,
      listenAddresses: addressesChanged ? editedAddresses : original.listenAddresses,
    };
    close();
    changes.open({
      kind: "propose",
      summary: t`Update SSH settings on ${server.name}`,
      edits: [
        {
          resource: { kind: "ssh.config", name: "default", scope: server.id },
          spec: { kind: { case: "sshConfig", value: spec } },
        },
      ],
      onClose: readAgain,
    });
  };
  return (
    <Sheet
      className="ssh-sheet-shell"
      open
      title={t`SSH settings on ${server.name}`}
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="commit" disabled={!account || !changed} onClick={() => submit()}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form className="firewall-add" onSubmit={submit}>
        {error ? <Callout tone="critical">{error}</Callout> : null}
        <SelectField
          label={t`Root login`}
          value={root}
          onChange={(event) => setRoot(event.target.value)}
          options={[
            ...(!rootNow ? [{ value: "", label: t`Keep current setting` }] : []),
            { value: "yes", label: t`Yes` },
            { value: "prohibit-password", label: t`Keys only` },
            { value: "no", label: t`No` },
          ]}
        />
        <Switch checked={password} onChange={setPassword} label={t`Password login`} />
        <Switch checked={keys} onChange={setKeys} label={t`Key login`} />
        <TextField
          mono
          inputMode="numeric"
          label={t`Port`}
          value={port}
          onChange={(event) => setPort(event.target.value)}
          disabled={!!ssh.socketUnit}
        />
        <TextField
          mono
          label={t`Listen addresses`}
          value={addresses}
          placeholder={t`All interfaces`}
          onChange={(event) => setAddresses(event.target.value)}
          disabled={!!ssh.socketUnit}
        />
        {ssh.socketUnit ? (
          <p className="firewall-note">
            <Trans>The systemd socket sets the port and addresses.</Trans>
          </p>
        ) : null}
        <SelectField
          label={t`Administrator to test with`}
          value={account}
          onChange={(event) => setAccount(event.target.value)}
          options={[
            { value: "", label: t`Choose an account` },
            ...ssh.adminAccounts.map((name) => ({ value: name, label: name })),
          ]}
        />
      </form>
    </Sheet>
  );
}

function ActionSheet({ title, close, children }: { title: string; close: () => void; children: ReactNode }) {
  return (
    <Sheet
      className="ssh-sheet-shell"
      open
      title={title}
      onClose={close}
      footer={
        <Button variant="ghost" onClick={close}>
          <Trans>Close</Trans>
        </Button>
      }
    >
      <div className="ssh-sheet">{children}</div>
    </Sheet>
  );
}

function useSSHWrite() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const run = async (call: () => Promise<unknown>) => {
    setBusy(true);
    setError(undefined);
    try {
      await call();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, run };
}

function SshKeys({ serverId, title, close }: { serverId: string; title: string; close: () => void }) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const client = createClient(FirewallService, transport);
  const may = useMay()(FirewallService.method.editServerSshKey);
  const [account, setAccount] = useState("");
  const [publicKey, setPublicKey] = useState("");
  const { busy, error, run } = useSSHWrite();
  const list = useQuery(FirewallService.method.listServerSshKeys, { serverId }, { retry: false });
  const read = useQuery(
    FirewallService.method.listServerSshKeys,
    { serverId, account },
    { enabled: !!account, retry: false },
  );
  const add = (event: FormEvent) => {
    event.preventDefault();
    const pasted = publicKey.trim();
    if (!pasted || busy) return;
    void run(async () => {
      if (pasted.includes("PRIVATE KEY") || pasted.startsWith("PuTTY-User-Key-File")) {
        setPublicKey("");
        throw new Error(t`This is a private key. Paste the public key instead.`);
      }
      await withStepUpIfAsked(transport, StepUpAction.EDIT_SSH_KEY, "", (proof) =>
        client.editServerSshKey({ serverId, account, proof, edit: { case: "publicKey", value: pasted } }),
      );
      setPublicKey("");
      await read.refetch();
    });
  };
  return (
    <ActionSheet title={title} close={close}>
      <SelectField
        label={t`Account`}
        value={account}
        onChange={(event) => {
          setAccount(event.target.value);
          setPublicKey("");
        }}
        disabled={busy}
        options={[
          { value: "", label: t`Choose an account` },
          ...(list.data?.accounts ?? []).map((name) => ({ value: name, label: name })),
        ]}
      />
      {list.error || read.error || error ? (
        <Callout tone="critical">{sentence(error ?? read.error ?? list.error)}</Callout>
      ) : null}
      {account && read.data ? (
        <>
          {!read.data.keys.length ? (
            <p className="firewall-note">
              <Trans>No authorized keys for this account.</Trans>
            </p>
          ) : null}
          <ul className="ssh-records">
            {read.data.keys.map((key, index) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: the same public key may appear with several restrictions
              <li key={`${key.fingerprint}:${index}`}>
                <div>
                  <strong>{key.comment || key.type}</strong>
                  <code>{key.fingerprint}</code>
                  <small>
                    {key.comment ? key.type : null}
                    {key.added ? ` · ${i18n.date(timestampDate(key.added), { dateStyle: "medium" })}` : null}
                  </small>
                </div>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy}
                  {...may.off}
                  onClick={() =>
                    void run(async () => {
                      await withStepUpIfAsked(transport, StepUpAction.EDIT_SSH_KEY, "", (proof) =>
                        client.editServerSshKey({
                          serverId,
                          account,
                          proof,
                          edit: { case: "removeFingerprint", value: key.fingerprint },
                        }),
                      );
                      await read.refetch();
                    })
                  }
                >
                  <Trans>Remove key</Trans>
                </Button>
              </li>
            ))}
          </ul>
          <form className="ssh-sheet" onSubmit={add}>
            <TextField
              mono
              label={t`OpenSSH public key`}
              placeholder="ssh-ed25519 …"
              value={publicKey}
              onChange={(event) => setPublicKey(event.target.value)}
              maxLength={16_384}
              autoComplete="off"
            />
            <Button variant="secondary" type="submit" disabled={busy || !publicKey.trim()} {...may.off}>
              <Trans>Add key</Trans>
            </Button>
          </form>
        </>
      ) : null}
    </ActionSheet>
  );
}

function SshSessions({ serverId, title, close }: { serverId: string; title: string; close: () => void }) {
  const { i18n } = useLingui();
  const transport = useTransport();
  const client = createClient(FirewallService, transport);
  const may = useMay()(FirewallService.method.disconnectServerSshSession);
  const { busy, error, run } = useSSHWrite();
  const read = useQuery(
    FirewallService.method.listServerSshSessions,
    { serverId },
    { retry: false, refetchInterval: 5000 },
  );
  return (
    <ActionSheet title={title} close={close}>
      {error || read.error ? <Callout tone="critical">{sentence(error ?? read.error)}</Callout> : null}
      {read.data?.sessions.length === 0 ? (
        <p className="firewall-note">
          <Trans>No active SSH sessions.</Trans>
        </p>
      ) : null}
      <ul className="ssh-records">
        {read.data?.sessions.map((session) => (
          <li key={session.id}>
            <div>
              <strong>{session.user}</strong>
              <code>{session.address}</code>
              <small>
                {session.since
                  ? i18n.date(timestampDate(session.since), { dateStyle: "short", timeStyle: "short" })
                  : null}
                {session.tty ? ` · ${session.tty}` : null}
              </small>
            </div>
            {session.canDisconnect ? (
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                {...may.off}
                onClick={() =>
                  void run(async () => {
                    await withStepUpIfAsked(transport, StepUpAction.DISCONNECT_SSH_SESSION, "", (proof) =>
                      client.disconnectServerSshSession({ serverId, sessionId: session.id, proof }),
                    );
                    await read.refetch();
                  })
                }
              >
                <Trans>Disconnect</Trans>
              </Button>
            ) : (
              <small>
                <Trans>Protected</Trans>
              </small>
            )}
          </li>
        ))}
      </ul>
    </ActionSheet>
  );
}

function SshLogins({ serverId, title, close }: { serverId: string; title: string; close: () => void }) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const client = createClient(FirewallService, transport);
  const may = useMay()(FirewallService.method.banServerSshAddress);
  const { busy, error, run } = useSSHWrite();
  const [user, setUser] = useState("");
  const [address, setAddress] = useState("");
  const [hours, setHours] = useState("24");
  const [result, setResult] = useState("");
  const [banned, setBanned] = useState<string[]>([]);
  const read = useQuery(
    FirewallService.method.listServerSshLogins,
    {
      serverId,
      hours: Number(hours),
      user: user.trim(),
      address: address.trim(),
      accepted: result ? result === "accepted" : undefined,
    },
    { retry: false, refetchInterval: 30_000 },
  );
  return (
    <ActionSheet title={title} close={close}>
      <div className="ssh-filters">
        <SelectField
          label={t`Time range`}
          value={hours}
          onChange={(event) => setHours(event.target.value)}
          options={[
            { value: "1", label: t`Last hour` },
            { value: "24", label: t`Last 24 hours` },
            { value: "168", label: t`Last 7 days` },
          ]}
        />
        <SelectField
          label={t`Result`}
          value={result}
          onChange={(event) => setResult(event.target.value)}
          options={[
            { value: "", label: t`All` },
            { value: "accepted", label: t`Accepted` },
            { value: "failed", label: t`Failed` },
          ]}
        />
        <TextField label={t`User`} value={user} onChange={(event) => setUser(event.target.value)} />
        <TextField label={t`Address`} mono value={address} onChange={(event) => setAddress(event.target.value)} />
      </div>
      {error || read.error ? <Callout tone="critical">{sentence(error ?? read.error)}</Callout> : null}
      {read.data?.bounded ? (
        <p className="firewall-note">
          <Trans>Showing recent matches within the log limit.</Trans>
        </p>
      ) : null}
      {read.data?.logins.length === 0 ? (
        <p className="firewall-note">
          <Trans>No SSH logins match these filters.</Trans>
        </p>
      ) : null}
      <ul className="ssh-records">
        {read.data?.logins.map((login, index) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: log lines in the same second can have identical details
          <li key={`${login.time?.seconds}:${index}`}>
            <div>
              <strong>{login.user}</strong>
              <code>{login.address}</code>
              <small>
                {login.time ? i18n.date(timestampDate(login.time), { dateStyle: "short", timeStyle: "short" }) : null} ·{" "}
                {login.method}
              </small>
            </div>
            <small>{login.accepted ? t`Accepted` : t`Failed`}</small>
          </li>
        ))}
      </ul>
      {read.data?.failures.length ? (
        <>
          <h3>
            <Trans>Failures by address</Trans>
          </h3>
          <ul className="ssh-records">
            {read.data.failures.map(({ address: peer, count }) => (
              <li key={peer}>
                <div>
                  <code>{peer}</code>
                  <small>
                    <Trans>{count} failed</Trans>
                  </small>
                </div>
                {read.data?.banAvailable ? (
                  banned.includes(peer) ? (
                    <small>
                      <Trans>Banned</Trans>
                    </small>
                  ) : (
                    <Button
                      variant="ghost"
                      size="sm"
                      disabled={busy}
                      {...may.off}
                      onClick={() =>
                        void run(async () => {
                          await withStepUpIfAsked(transport, StepUpAction.BAN_SSH_ADDRESS, "", (proof) =>
                            client.banServerSshAddress({ serverId, address: peer, proof }),
                          );
                          setBanned((old) => [...old, peer]);
                        })
                      }
                    >
                      <Trans>Ban in Fail2ban</Trans>
                    </Button>
                  )
                ) : null}
              </li>
            ))}
          </ul>
        </>
      ) : null}
    </ActionSheet>
  );
}
