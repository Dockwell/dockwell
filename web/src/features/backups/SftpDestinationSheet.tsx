// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  BackupDestinationProblem,
  BackupService,
  ChangeState,
  Connection,
  type TestBackupDestinationResponse,
} from "@dockwell/api";
import { Button, Callout, CodeField, SelectField, Sheet, TextField, validPortNumber } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { CopyButton } from "../system/CopyCommand";
import { BackupSealFields, useBackupSeal } from "./BackupSealFields";

type SignIn = "keep" | "generated" | "private-key" | "password";

function base64(bytes: Uint8Array) {
  return btoa(String.fromCharCode(...bytes));
}

/** The private half stays in this form until submitted. SSH public keys encode two length-prefixed strings. */
async function makeSSHKey() {
  const pair = await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
  if (!("privateKey" in pair)) throw new Error("Ed25519 key generation returned no key pair");
  const algorithm = new TextEncoder().encode("ssh-ed25519");
  const raw = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  const wire = new Uint8Array(8 + algorithm.length + raw.length);
  const view = new DataView(wire.buffer);
  view.setUint32(0, algorithm.length);
  wire.set(algorithm, 4);
  view.setUint32(4 + algorithm.length, raw.length);
  wire.set(raw, 8 + algorithm.length);
  const pkcs8 = base64(new Uint8Array(await crypto.subtle.exportKey("pkcs8", pair.privateKey)));
  return {
    publicKey: `ssh-ed25519 ${base64(wire)} dockwell-backups`,
    privateKey: `-----BEGIN PRIVATE KEY-----\n${pkcs8}\n-----END PRIVATE KEY-----\n`,
  };
}

export function SftpDestinationSheet({
  editing,
  close,
  onSaved,
}: {
  editing?: BackupDestinationInfo;
  close: () => void;
  onSaved: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const plan = useChangeSheet();
  const mayTest = useMay()(BackupService.method.testBackupDestination);
  const [name, setName] = useState(editing?.name ?? "");
  const [host, setHost] = useState(editing?.sftp?.host ?? "");
  const [port, setPort] = useState(String(editing?.sftp?.port || 22));
  const [user, setUser] = useState(editing?.sftp?.user ?? "");
  const [path, setPath] = useState(editing?.sftp?.path ?? "/backups");
  const [pin, setPin] = useState(editing?.sftp?.hostKeyFingerprint ?? "");
  const [hostKeyAlgorithm, setHostKeyAlgorithm] = useState(editing?.sftp?.hostKeyAlgorithm ?? "");
  const [mode, setMode] = useState<SignIn>(editing ? "keep" : "generated");
  const [chosenServer, setChosenServer] = useState("");
  const { servers } = useFleetView();
  const connected = (servers ?? []).filter((server) => server.connection === Connection.CONNECTED);
  const server = chosenServer || (connected.length === 1 ? (connected[0]?.id ?? "") : "");
  const [keyPair, setKeyPair] = useState<Awaited<ReturnType<typeof makeSSHKey>>>();
  const [keyError, setKeyError] = useState(false);
  const [privateKey, setPrivateKey] = useState("");
  const [keyPassphrase, setKeyPassphrase] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<TestBackupDestinationResponse>();
  const [error, setError] = useState<string>();
  const seal = useBackupSeal("");
  const numericPort = port === "" ? 22 : Number(port);
  const sftp = {
    host: host.trim(),
    port: numericPort,
    user: user.trim(),
    path: path.trim(),
    hostKeyFingerprint: pin,
    hostKeyAlgorithm,
    privateKey: mode === "generated" ? keyPair?.privateKey : mode === "private-key" ? privateKey.trim() : undefined,
    keyPassphrase: mode === "private-key" ? keyPassphrase : undefined,
    password: mode === "password" ? password : undefined,
  };
  const canKeep =
    !!editing?.sftp &&
    sftp.host.toLowerCase() === editing.sftp.host &&
    numericPort === (editing.sftp.port || 22) &&
    sftp.user === editing.sftp.user;
  const canTest =
    connected.some((candidate) => candidate.id === server) &&
    sftp.host !== "" &&
    validPortNumber(port || "22") &&
    sftp.user !== "" &&
    sftp.path.startsWith("/") &&
    !!(sftp.privateKey || sftp.password || (mode === "keep" && canKeep));
  const nameValid = /^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(name.trim());
  const tested = result?.sftp?.deleted && result.problem === BackupDestinationProblem.UNSPECIFIED;
  const fingerprint = result?.sftp?.hostKeyFingerprint;
  const presentedAlgorithm = result?.sftp?.hostKeyAlgorithm ?? "";
  const reviewFingerprint = fingerprint && !result?.sftp?.hostKeyVerified;

  useEffect(() => {
    if (mode !== "generated") return;
    let canceled = false;
    void makeSSHKey().then(
      (key) => {
        if (!canceled) setKeyPair((previous) => previous ?? key);
      },
      () => {
        if (!canceled) setKeyError(true);
      },
    );
    return () => {
      canceled = true;
    };
  }, [mode]);

  const changed = (address = false) => {
    setResult(undefined);
    setError(undefined);
    if (address) {
      setPin("");
      setHostKeyAlgorithm("");
    }
  };

  const test = async (fingerprintToUse = pin, algorithmToUse = hostKeyAlgorithm) => {
    setBusy(true);
    changed();
    try {
      const client = createClient(BackupService, transport);
      setResult(
        await client.testBackupDestination({
          server,
          existingDestination: mode === "keep" ? editing?.name : undefined,
          destination: { sftp: { ...sftp, hostKeyFingerprint: fingerprintToUse, hostKeyAlgorithm: algorithmToUse } },
        }),
      );
    } catch (err) {
      setError(sentence(err));
    } finally {
      setBusy(false);
    }
  };

  const save = () => {
    const destinationName = name.trim();
    plan.open({
      kind: "propose",
      summary: editing ? t`Update backup destination ${destinationName}` : t`Add backup destination ${destinationName}`,
      edits: [
        {
          resource: { kind: "backup.destination", name: destinationName },
          spec: {
            kind: {
              case: "backupDestination",
              value: {
                sftp: { ...sftp, hostKeyAlgorithm: result?.sftp?.hostKeyAlgorithm || hostKeyAlgorithm },
                passphrase: editing ? "" : seal.passphrase,
              },
            },
          },
        },
      ],
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) {
          onSaved();
          close();
        }
      },
    });
  };

  return (
    <Sheet
      open
      onClose={close}
      title={editing ? t`Edit SFTP destination` : t`Add SFTP destination`}
      footer={
        <>
          <Button onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            disabled={busy || !tested || !nameValid || (!editing && !seal.ready)}
            onClick={save}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <fieldset className="backup-sheet sftp-fields" disabled={busy}>
        <TextField
          label={t`Name`}
          value={name}
          placeholder="off-site"
          onChange={(event) => setName(event.target.value)}
          disabled={!!editing}
        />
        <div className="sftp-address">
          <TextField
            label={t`Host`}
            value={host}
            placeholder="example.com"
            onChange={(event) => {
              changed(true);
              setHost(event.target.value);
            }}
          />
          <TextField
            label={t`Port`}
            value={port}
            inputMode="numeric"
            placeholder="22"
            onChange={(event) => {
              changed(true);
              setPort(event.target.value);
            }}
          />
        </div>
        <TextField
          label={t`User`}
          value={user}
          placeholder="backups"
          onChange={(event) => {
            changed();
            setUser(event.target.value);
          }}
        />
        <TextField
          label={t`Remote folder`}
          value={path}
          mono
          onChange={(event) => {
            changed();
            setPath(event.target.value);
          }}
        />
        <SelectField
          label={t`Sign in with`}
          value={mode}
          options={[
            ...(editing ? [{ value: "keep", label: t`Keep saved sign-in` }] : []),
            { value: "generated", label: t`Generated SSH key` },
            { value: "private-key", label: t`Your SSH private key` },
            { value: "password", label: t`Password` },
          ]}
          onChange={(event) => {
            changed();
            setMode(event.target.value as SignIn);
          }}
        />
        {editing?.sftp?.publicKey && mode !== "generated" ? <SftpPublicKey publicKey={editing.sftp.publicKey} /> : null}
        {mode === "keep" ? (
          canKeep ? null : (
            <span className="backup-why">
              <Trans>Choose new sign-in details after changing the SSH account.</Trans>
            </span>
          )
        ) : mode === "generated" ? (
          keyPair ? (
            <SftpPublicKey publicKey={keyPair.publicKey} />
          ) : keyError ? (
            <Callout tone="warning" title={t`Couldn't generate an SSH key`}>
              <Trans>Choose your own SSH private key to continue.</Trans>
            </Callout>
          ) : (
            <span role="status">
              <Trans>Generating SSH key</Trans>
            </span>
          )
        ) : mode === "private-key" ? (
          <>
            <CodeField
              label={t`SSH private key`}
              autoComplete="off"
              spellCheck={false}
              lines={3}
              value={privateKey}
              onChange={(event) => {
                changed();
                setPrivateKey(event.target.value);
              }}
            />
            <TextField
              label={t`Private key passphrase`}
              hint={t`For an encrypted private key.`}
              type="password"
              autoComplete="off"
              optional
              value={keyPassphrase}
              onChange={(event) => {
                changed();
                setKeyPassphrase(event.target.value);
              }}
            />
          </>
        ) : (
          <TextField
            label={t`Password`}
            hint={t`Use a password when the server only supports password sign-in.`}
            type="password"
            autoComplete="off"
            value={password}
            onChange={(event) => {
              changed();
              setPassword(event.target.value);
            }}
          />
        )}
        <SelectField
          label={t`Test from server`}
          value={server}
          options={[
            { value: "", label: t`Choose a connected server` },
            ...connected.map((candidate) => ({
              value: candidate.id,
              label: candidate.name,
            })),
          ]}
          onChange={(event) => {
            changed();
            setChosenServer(event.target.value);
          }}
        />
        {pin && !reviewFingerprint && !result ? <HostKeyProof algorithm={hostKeyAlgorithm} fingerprint={pin} /> : null}
        {reviewFingerprint ? (
          <Callout
            tone="warning"
            title={
              result?.problem === BackupDestinationProblem.HOST_KEY_CHANGED
                ? t`The server's host key changed`
                : t`Verify the server's fingerprint`
            }
          >
            <div className="sftp-proof">
              {result?.problem === BackupDestinationProblem.HOST_KEY_CHANGED ? (
                <Trans>Dockwell stopped before signing in. Verify the new fingerprint with the server owner.</Trans>
              ) : (
                <Trans>Compare this fingerprint with the one from your SFTP server.</Trans>
              )}
              <HostKeyProof
                algorithm={presentedAlgorithm}
                fingerprint={fingerprint}
                server={result?.sftp?.serverName}
              />
              <Button
                variant="secondary"
                size="sm"
                {...mayTest.off}
                onClick={() => {
                  setPin(fingerprint);
                  setHostKeyAlgorithm(presentedAlgorithm);
                  if (mode === "keep" && !editing?.sftp?.publicKey) {
                    changed();
                    setMode("password");
                    setError(t`Enter the password again to test the new fingerprint.`);
                  } else {
                    void test(fingerprint, presentedAlgorithm);
                  }
                }}
              >
                <Trans>Trust fingerprint and test</Trans>
              </Button>
            </div>
          </Callout>
        ) : (
          <div className="backup-sheet-test">
            <Button variant="secondary" loading={busy} disabled={!canTest} {...mayTest.off} onClick={() => void test()}>
              <Trans>Test destination</Trans>
            </Button>
          </div>
        )}
        {result && !reviewFingerprint ? <Checks result={result} /> : null}
        {error ? (
          <Callout tone="critical" title={t`Couldn't test the destination`}>
            {error}
          </Callout>
        ) : null}
        {editing ? null : <BackupSealFields seal={seal} showPrefix={false} />}
      </fieldset>
    </Sheet>
  );
}

function HostKeyProof({ algorithm, fingerprint, server }: { algorithm: string; fingerprint: string; server?: string }) {
  const { t } = useLingui();
  const file =
    algorithm === "ssh-ed25519"
      ? "ed25519"
      : algorithm === "ssh-rsa"
        ? "rsa"
        : algorithm.startsWith("ecdsa-")
          ? "ecdsa"
          : "";
  const command = file ? `ssh-keygen -lf /etc/ssh/ssh_host_${file}_key.pub` : "";
  return (
    <div className="sftp-proof">
      {server ? <span>{t`Tested from ${server}`}</span> : null}
      <code>
        {algorithm} {fingerprint}
      </code>
      {command ? (
        <CopyButton text={command} label={t`Copy fingerprint command`} copiedLabel={t`Command copied`} />
      ) : null}
    </div>
  );
}

function SftpPublicKey({ publicKey }: { publicKey: string }) {
  const { t } = useLingui();
  const command = `mkdir -p ~/.ssh && echo '${publicKey}' >> ~/.ssh/authorized_keys`;
  return (
    <Callout tone="info" title={t`Public key`}>
      <div className="sftp-proof">
        <code>{publicKey}</code>
        <div className="backup-sheet-row">
          <CopyButton text={publicKey} label={t`Copy public key`} copiedLabel={t`Public key copied`} />
          <CopyButton text={command} label={t`Copy key install command`} copiedLabel={t`Command copied`} />
        </div>
      </div>
    </Callout>
  );
}

function Checks({ result }: { result: TestBackupDestinationResponse }) {
  const { t, i18n } = useLingui();
  const sftp = result.sftp;
  if (!sftp) return null;
  const failure = result.problem;
  let reason = result.said;
  switch (failure) {
    case BackupDestinationProblem.UNREACHABLE:
      reason = t`SSH couldn't connect. Check the host, port and network, then try again.`;
      break;
    case BackupDestinationProblem.KEYS_REFUSED:
      reason = t`The server refused the sign-in. Check the user and SSH key or password, then test again.`;
      break;
    case BackupDestinationProblem.STORAGE_ERROR:
      reason = t`The server didn't open SFTP. Enable it for this user, then test again.`;
      break;
    case BackupDestinationProblem.CANT_OPEN_FOLDER:
      reason = t`Dockwell couldn't open or create the folder. Check the path and the user's permissions.`;
      break;
    case BackupDestinationProblem.CANT_WRITE:
      reason = t`Dockwell couldn't write the test file. Check the connection and write permission, then test again.`;
      break;
    case BackupDestinationProblem.CANT_READ:
      reason = t`Dockwell couldn't read back the test file. Check the connection and read permission, then test again.`;
      break;
    case BackupDestinationProblem.CANT_DELETE:
      reason = t`Dockwell couldn't delete the test file. Check delete permission before sending backups.`;
      break;
  }
  const rows = [
    { label: t`Reach server`, passed: sftp.reached, failed: failure === BackupDestinationProblem.UNREACHABLE },
    { label: t`Verify host key`, passed: sftp.hostKeyVerified, failed: false },
    { label: t`Sign in`, passed: result.signedIn, failed: failure === BackupDestinationProblem.KEYS_REFUSED },
    {
      label: t`Open folder`,
      passed: sftp.folderReady,
      failed:
        failure === BackupDestinationProblem.CANT_OPEN_FOLDER || failure === BackupDestinationProblem.STORAGE_ERROR,
    },
    { label: t`Write test file`, passed: result.wrote, failed: failure === BackupDestinationProblem.CANT_WRITE },
    { label: t`Read test file`, passed: sftp.readBack, failed: failure === BackupDestinationProblem.CANT_READ },
    { label: t`Delete test file`, passed: sftp.deleted, failed: !!result.testFile },
  ];
  return (
    <div className="sftp-proof" aria-live="polite">
      {failure === BackupDestinationProblem.UNSPECIFIED ? (
        <strong>
          <Trans>Destination test passed</Trans>
        </strong>
      ) : (
        <Callout tone="critical" title={t`Destination test failed`}>
          {reason}
        </Callout>
      )}
      {sftp.hostKeyFingerprint ? (
        <HostKeyProof
          algorithm={sftp.hostKeyAlgorithm}
          fingerprint={sftp.hostKeyFingerprint}
          server={sftp.serverName}
        />
      ) : sftp.serverName ? (
        <span>{t`Tested from ${sftp.serverName}`}</span>
      ) : null}
      <dl className="sftp-checks">
        {rows.map(({ label, passed, failed }) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{failed ? t`Failed` : passed ? t`Passed` : t`Not run`}</dd>
          </div>
        ))}
        {sftp.folderReady ? (
          <div>
            <dt>
              <Trans>Free space</Trans>
            </dt>
            <dd>{sftp.freeBytes === undefined ? t`Unavailable` : formatBytes(sftp.freeBytes, i18n.locale)}</dd>
          </div>
        ) : null}
      </dl>
      {result.testFile ? <code>{result.testFile}</code> : null}
    </div>
  );
}
