// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, type CheckClaimResponse, Connection, type Server, SystemService } from "@dockwell/api";
import { Button, Callout, Logo, Panel, type Status, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useLocation, useNavigate } from "@tanstack/react-router";
import { type FormEvent, useEffect, useId, useState } from "react";

import { formatBytes } from "../../lib/format";
import { useNow, useTimeWords } from "../../lib/time";
import { useDocumentTitle } from "../../lib/useDocumentTitle";
import { createPasskey, passkeysAvailable } from "../../lib/webauthn";
import { CopyCommand } from "../system/CopyCommand";
import { StandalonePage } from "../system/StandalonePage";
import { useDeviceName } from "./device";
import { useAuthError } from "./messages";
import { useAuthState, useClaimCommand, useRefreshAuth } from "./useAuthState";
import "./auth.css";
import "./ClaimPage.css";

/** A claim token is 32 random bytes in base64url. */
const TOKEN_LENGTH = 43;

/** The token from a pasted claim link or code: whatever follows "#", if there is one. */
export function tokenFrom(input: string): string {
  const trimmed = input.trim();
  const hash = trimmed.indexOf("#");
  return hash >= 0 ? trimmed.slice(hash + 1) : trimmed;
}

/**
 * Reads a one-time token from the URL fragment once, then takes it out of
 * the address bar and history, so it isn't left behind once spent.
 */
export function useFragmentToken(path: "/claim" | "/recover" | "/invite"): string {
  const navigate = useNavigate();
  const hash = useLocation({ select: (l) => l.hash });
  const [token] = useState(() => tokenFrom(hash ? `#${hash}` : ""));
  useEffect(() => {
    if (hash) void navigate({ to: path, replace: true });
  }, [navigate, path, hash]);
  return token;
}

/**
 * The first visit: create the owner account with a passkey, using a claim
 * link from the server: the one the install printed, or dockwell claim's.
 */
export function ClaimPage() {
  const { t } = useLingui();
  const linkToken = useFragmentToken("/claim");
  // Set once the claim here succeeds, so the owner it made isn't told the
  // install already has one on the way to the overview.
  const [claimedHere, setClaimedHere] = useState(false);
  const [code, setCode] = useState(linkToken);
  const token = tokenFrom(code);
  const auth = useAuthState();
  const version = useQuery(SystemService.method.getVersion, {});
  const check = useQuery(
    AuthService.method.checkClaim,
    { token },
    {
      enabled: token.length === TOKEN_LENGTH,
      retry: false,
      staleTime: Number.POSITIVE_INFINITY,
      refetchOnWindowFocus: false,
    },
  );

  const alreadyClaimed = Boolean(auth.data?.claimed) && !claimedHere;
  const server = check.data?.server;
  const name = server?.name || auth.data?.serverName || window.location.hostname;
  // The title for both pages. A component's effects run after its children's, so this is the one that stays.
  useDocumentTitle(alreadyClaimed ? t`Already claimed` : t`Claim ${name}`);

  if (alreadyClaimed) {
    return <AlreadyClaimed signedIn={Boolean(auth.data?.user)} />;
  }
  return (
    <div className="claim">
      <header className="claim-bar">
        <Logo variant="logotype" height={14} title="Dockwell" />
        {version.data?.build ? <span className="claim-version">{version.data.build.version}</span> : null}
      </header>
      <main className="claim-main">
        <div className="claim-column">
          <div className="claim-head">
            <h1 className="dw-text-display">
              <Trans>Claim {name}</Trans>
            </h1>
            <p className="claim-lead">
              <Trans>This panel controls {name}. Create the owner account with a passkey.</Trans>
            </p>
          </div>
          <div className="claim-grid">
            <OwnerForm
              name={name}
              code={code}
              onCode={setCode}
              token={token}
              linkProblem={check.isError ? check.error : undefined}
              domain={auth.data?.domain ?? ""}
              publicAddress={check.data?.publicAddress ?? ""}
              onClaimed={() => setClaimedHere(true)}
            />
            <div className="claim-aside">
              {server ? <ThisServer server={server} claim={check.data} /> : null}
              <WhatHappensNext />
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}

interface OwnerFormProps {
  name: string;
  code: string;
  onCode: (code: string) => void;
  token: string;
  linkProblem: unknown;
  domain: string;
  publicAddress: string;
  /** Called once the owner exists, before the sign-in state is read again. */
  onClaimed: () => void;
}

function OwnerForm({ name, code, onCode, token, linkProblem, domain, publicAddress, onClaimed }: OwnerFormProps) {
  const { t } = useLingui();
  const titleId = useId();
  const transport = useTransport();
  const navigate = useNavigate();
  const refreshAuth = useRefreshAuth();
  const claimCommand = useClaimCommand();
  const explain = useAuthError({ claimCommand });
  const [person, setPerson] = useState("");
  const [email, setEmail] = useState("");
  const [invalid, setInvalid] = useState<{ person?: string; email?: string; code?: string }>({});
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<unknown>();
  const device = useDeviceName();
  const canUsePasskeys = passkeysAvailable();

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const next = {
      person: person.trim() ? undefined : t`Enter your name.`,
      email: /^[^\s@]+@[^\s@]+$/.test(email.trim()) ? undefined : t`Enter an email address, such as mira@acme.dev.`,
      code: token.length === TOKEN_LENGTH ? undefined : t`Paste the claim link or code you got on the server.`,
    };
    setInvalid(next);
    if (next.person || next.email || next.code) return;
    setBusy(true);
    setProblem(undefined);
    try {
      const client = createClient(AuthService, transport);
      const begin = await client.beginClaim({ token, name: person, email });
      const credentialJson = await createPasskey(begin.optionsJson);
      await client.finishClaim({ ceremonyId: begin.ceremonyId, token, credentialJson, passkeyName: device });
      onClaimed();
      await refreshAuth();
      // The claim page has done its work: Back mustn't return to it.
      await navigate({ to: "/", replace: true });
    } catch (err) {
      setProblem(err);
    } finally {
      setBusy(false);
    }
  };

  const shownProblem = problem ?? linkProblem;
  return (
    <form className="dw-panel claim-form" aria-labelledby={titleId} onSubmit={(e) => void submit(e)} noValidate>
      <div className="dw-panel-head">
        <h2 className="dw-panel-title" id={titleId}>
          <Trans>Owner account</Trans>
        </h2>
        <span className="claim-scope">
          <Trans>Full access to {name}</Trans>
        </span>
      </div>
      <div className="claim-form-body">
        <div className="claim-names">
          <TextField
            label={t`Your name`}
            value={person}
            onChange={(e) => setPerson(e.target.value)}
            autoComplete="name"
            size="lg"
            error={invalid.person}
          />
          <TextField
            label={t`Email`}
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
            size="lg"
            error={invalid.email}
            hint={t`You sign in with it when you use a code.`}
          />
        </div>
        <TextField
          label={t`Claim code`}
          value={code}
          onChange={(e) => onCode(e.target.value)}
          mono
          icon="key"
          size="lg"
          autoComplete="off"
          error={invalid.code}
          hint={
            <Trans>
              From the claim link. Run <code>{claimCommand}</code> on {name} for a new one.
            </Trans>
          }
        />
        <div className="claim-rule" />
        <PanelAddress domain={domain} publicAddress={publicAddress} />
        <div className="claim-fill" />
        {shownProblem ? (
          <Callout tone="critical" title={t`The owner account wasn't created`}>
            {explain(shownProblem)}
          </Callout>
        ) : null}
        {canUsePasskeys ? null : (
          <Callout tone="warning" title={t`Passkeys don't work at this address`}>
            <Trans>
              Open the claim link through your SSH tunnel at <code>http://localhost</code>, or on the panel's domain
              over HTTPS.
            </Trans>
          </Callout>
        )}
        <div className="claim-actions">
          <Button type="submit" variant="primary" size="lg" icon="key" loading={busy} disabled={!canUsePasskeys}>
            {busy ? t`Creating passkey` : t`Create passkey`}
          </Button>
          <span className="claim-note">
            <Trans>Confirm with Touch ID, Windows Hello or a security key.</Trans>
          </span>
        </div>
      </div>
    </form>
  );
}

/** Where the panel answers, and how to give it a domain. */
function PanelAddress({ domain, publicAddress }: { domain: string; publicAddress: string }) {
  const { t } = useLingui();
  if (domain) {
    return (
      <TextField
        label={t`Panel address`}
        prefix="https://"
        value={domain}
        readOnly
        mono
        size="lg"
        hint={t`HTTPS is automatic. Your passkey works at this address.`}
      />
    );
  }
  // The control plane names a public address only when it knows one, and an
  // IPv6 one takes an AAAA record rather than an A record.
  const record = publicAddress.includes(":") ? "AAAA" : "A";
  return (
    <TextField
      label={t`Panel address`}
      prefix="http://"
      value={window.location.host}
      readOnly
      mono
      size="lg"
      hint={
        publicAddress ? (
          <Trans>
            Only through your SSH tunnel, and your passkey works only here. For HTTPS on your own domain, point its{" "}
            {record} record to <code>{publicAddress}</code>, then restart dockwell with <code>--domain</code>.
          </Trans>
        ) : (
          <Trans>
            Only through your SSH tunnel, and your passkey works only here. For HTTPS on your own domain, point its A or
            AAAA record to this server's public address, then restart dockwell with <code>--domain</code>.
          </Trans>
        )
      }
    />
  );
}

const CONNECTION: Record<Connection, Status> = {
  [Connection.UNSPECIFIED]: "stopped",
  [Connection.CONNECTED]: "healthy",
  [Connection.UNREACHABLE]: "down",
  [Connection.PENDING]: "deploying",
};

function ThisServer({ server, claim }: { server: Server; claim: CheckClaimResponse | undefined }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useNow();
  const inv = server.inventory;
  const disk = inv?.disks[0];
  const size = [
    inv?.cpuCount ? t`${inv.cpuCount} vCPU` : "",
    inv?.memoryBytes ? formatBytes(inv.memoryBytes, i18n.locale) : "",
    disk ? t`${formatBytes(disk.sizeBytes, i18n.locale)} disk` : "",
  ].filter(Boolean);
  const label = {
    [Connection.UNSPECIFIED]: t`Agent not connected`,
    [Connection.CONNECTED]: t`Agent connected`,
    [Connection.UNREACHABLE]: t`Agent not connected`,
    [Connection.PENDING]: t`Agent starting`,
  }[server.connection];
  return (
    <Panel title={t`This server`} actions={<StatusBadge status={CONNECTION[server.connection]} label={label} />}>
      <dl className="claim-facts">
        <dt>
          <Trans>Name</Trans>
        </dt>
        <dd>
          <code>{server.name}</code>
        </dd>
        {claim?.publicAddress ? (
          <>
            <dt>
              <Trans>Public IP</Trans>
            </dt>
            <dd>
              <code>{claim.publicAddress}</code>
            </dd>
          </>
        ) : null}
        {inv?.osName ? (
          <>
            <dt>
              <Trans>System</Trans>
            </dt>
            <dd>{inv.osName}</dd>
          </>
        ) : null}
        {size.length > 0 ? (
          <>
            <dt>
              <Trans context="server">Size</Trans>
            </dt>
            <dd>{size.join(" · ")}</dd>
          </>
        ) : null}
        {server.agentBuild?.version ? (
          <>
            <dt>
              <Trans>Agent</Trans>
            </dt>
            <dd>
              <code>{server.agentBuild.version}</code>
            </dd>
          </>
        ) : null}
        {server.createTime ? (
          <>
            <dt>
              <Trans>Installed</Trans>
            </dt>
            <dd>
              <time dateTime={timestampDate(server.createTime).toISOString()}>
                {words.ago(timestampDate(server.createTime), now)}
              </time>
            </dd>
          </>
        ) : null}
        {claim?.expireTime ? (
          <>
            <dt>
              <Trans>Link expires</Trans>
            </dt>
            <dd>
              <LinkExpiry at={timestampDate(claim.expireTime).getTime()} />
            </dd>
          </>
        ) : null}
      </dl>
    </Panel>
  );
}

/** When the claim link stops working, kept current: "in 23 h", "in 4 min", "Expired". */
function LinkExpiry({ at }: { at: number }) {
  const { t } = useLingui();
  const now = useNow(30_000);
  const ms = at - now;
  if (ms <= 0) return <>{t`Expired`}</>;
  const hours = Math.floor(ms / 3_600_000);
  if (hours >= 1) return <>{t`in ${hours} h`}</>;
  const minutes = Math.floor(ms / 60_000);
  return <>{minutes >= 1 ? t`in ${minutes} min` : t`in under a minute`}</>;
}

function WhatHappensNext() {
  const { t } = useLingui();
  const steps = [
    { title: t`Claim this server`, detail: t`You become the owner. Nobody else can sign in.` },
    { title: t`Deploy your first app`, detail: t`From Git, a Docker image, a Compose file or the catalog.` },
    { title: t`It's live with HTTPS`, detail: t`Your domain gets a certificate automatically.` },
  ];
  return (
    <Panel title={t`What happens next`} className="claim-next">
      <ol className="claim-steps">
        {steps.map((step, i) => (
          <li key={step.title} className={i === 0 ? "is-current" : undefined}>
            <span className="claim-step-number" aria-hidden="true">
              {i + 1}
            </span>
            <div>
              <span className="claim-step-title">{step.title}</span>
              <span className="claim-step-detail">{step.detail}</span>
            </div>
          </li>
        ))}
      </ol>
    </Panel>
  );
}

/** A claim link opened after the install already has an owner. */
function AlreadyClaimed({ signedIn }: { signedIn: boolean }) {
  const { t } = useLingui();
  const navigate = useNavigate();
  return (
    <StandalonePage title={t`Already claimed`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>This install already has an owner</Trans>
        </h1>
        <p className="standalone-lead">
          <Trans>A claim link works only until someone claims the install. Sign in instead.</Trans>
        </p>
      </header>
      <div className="auth-actions">
        <Button
          variant="primary"
          icon={signedIn ? "home" : "key"}
          onClick={() => void navigate({ to: signedIn ? "/" : "/sign-in" })}
        >
          {signedIn ? <Trans>Go to the overview</Trans> : <Trans>Sign in</Trans>}
        </Button>
      </div>
      <p className="auth-small">
        <Trans>Lost your passkey? Run this on the server over SSH. It prints a sign-in link that works once.</Trans>
      </p>
      <CopyCommand command="dockwell recover" />
    </StandalonePage>
  );
}
