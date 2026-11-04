// SPDX-License-Identifier: AGPL-3.0-only

import { durationFromMs, timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { StepUpAction, type Token, TokenScope, TokenService } from "@dockwell/api";
import { Button, Callout, cx, Icon, Panel, Sheet, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useId, useState } from "react";

import { formatDay } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow } from "../servers/useFleet";
import { CopyButton } from "../system/CopyCommand";
import { useAuthError } from "./messages";
import { stepUp } from "./stepUp";
import "./TokensPanel.css";

const DAY = 86_400_000;
/** A token that expires sooner than this gets a warning. */
const SOON = 14 * DAY;

type Expiry = "30" | "90" | "365" | "never";

/** A token just made, with its secret, which is shown only now. */
interface Made {
  name: string;
  secret: string;
  expires?: Date;
}

/**
 * Your API tokens, for scripts, the CLI and integrations: make one, see
 * where each was last used, and revoke any of them. A new token's secret is
 * shown once, right after it is made.
 */
export function TokensPanel() {
  const { t } = useLingui();
  const tokens = useQuery(TokenService.method.listTokens, {});
  const explain = useAuthError();
  const [creating, setCreating] = useState(false);
  const [made, setMade] = useState<Made>();
  // Last used and expiry are on the control plane's clock, as the server pages' times are.
  const now = useControlPlaneNow();

  let list = null;
  if (tokens.data) {
    list = tokens.data.tokens.length ? (
      <ul className="profile-list">
        {tokens.data.tokens.map((token) => (
          <TokenRow key={token.id} token={token} now={now} onRevoked={() => void tokens.refetch()} />
        ))}
      </ul>
    ) : (
      <p className="profile-empty">
        <Trans>No tokens yet. Make one for each script or integration, so you can revoke one alone.</Trans>
      </p>
    );
  } else if (tokens.error) {
    list = (
      <p className="profile-error" role="alert">
        {explain(tokens.error)}
      </p>
    );
  } else {
    list = (
      <p className="profile-empty" aria-busy="true">
        <Trans>Loading tokens</Trans>
      </p>
    );
  }

  return (
    <Panel
      title={t`API tokens`}
      className="profile-panel"
      flush
      actions={
        <Button size="sm" icon="plus" onClick={() => setCreating(true)}>
          <Trans>Create token</Trans>
        </Button>
      }
    >
      {made ? <NewToken made={made} onDone={() => setMade(undefined)} /> : null}
      {list}
      <p className="profile-note">
        <Trans>
          Tokens act for you from scripts, the CLI and integrations. None can remove data, cut off access or change how
          you sign in.
        </Trans>
      </p>
      <CreateTokenSheet
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={(m) => {
          setMade(m);
          setCreating(false);
          void tokens.refetch();
        }}
      />
    </Panel>
  );
}

/** The new token's secret, once, with a button that copies it. */
function NewToken({ made, onDone }: { made: Made; onDone: () => void }) {
  const { t, i18n } = useLingui();
  const { name, secret } = made;
  const expires = made.expires ? formatDay(made.expires, i18n.locale) : "";
  return (
    <Callout
      tone="healthy"
      className="tokens-made"
      title={t`Copy this token now. It won't be shown again.`}
      actions={
        <Button size="sm" variant="ghost" onClick={onDone}>
          <Trans>Done</Trans>
        </Button>
      }
    >
      <span>
        {expires ? (
          <Trans>
            Created <b>{name}</b>. Send it in the Authorization header as <code>Bearer</code> and the token. It expires{" "}
            {expires}.
          </Trans>
        ) : (
          <Trans>
            Created <b>{name}</b>. Send it in the Authorization header as <code>Bearer</code> and the token. It doesn't
            expire.
          </Trans>
        )}
      </span>
      <span className="copy-command tokens-secret">
        <code>{secret}</code>
        <CopyButton text={secret} label={t`Copy token`} copiedLabel={t`Token copied`} />
      </span>
    </Callout>
  );
}

function TokenRow({ token, now, onRevoked }: { token: Token; now: number; onRevoked: () => void }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const transport = useTransport();
  const explain = useAuthError();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const { name, hint, lastAddress } = token;
  const scope = token.scope === TokenScope.READ_WRITE ? t`Read and write` : t`Read only`;
  const ago = token.lastUsedTime ? words.ago(timestampDate(token.lastUsedTime), now) : "";
  const expires = token.expireTime ? timestampDate(token.expireTime) : undefined;
  const day = expires ? formatDay(expires, i18n.locale) : "";
  const left = expires ? expires.getTime() - now : Number.POSITIVE_INFINITY;
  const remaining = words.span(Math.max(0, left));

  const revoke = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(TokenService, transport).revokeToken({ id: token.id });
      onRevoked();
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
        <Icon name="terminal" size={16} />
      </span>
      <div className="profile-row-text">
        <span className="profile-row-name">{name}</span>
        <span className="profile-row-meta">
          <code>{hint}</code> · {scope} ·{" "}
          {ago ? (
            <Trans>
              used {ago} from <code>{lastAddress}</code>
            </Trans>
          ) : (
            <Trans>never used</Trans>
          )}{" "}
          · {expires ? <Trans>expires {day}</Trans> : <Trans>doesn't expire</Trans>}
        </span>
        {left < SOON ? <StatusBadge status="degraded" label={t`Expires in ${remaining}`} /> : null}
        {problem ? (
          <span className="profile-error" role="alert">
            {problem}
          </span>
        ) : null}
      </div>
      {confirming ? (
        <div className="profile-row-confirm">
          <Button size="sm" variant="danger" loading={busy} onClick={() => void revoke()}>
            <Trans>Revoke</Trans>
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
            <Trans>Keep</Trans>
          </Button>
        </div>
      ) : (
        <Button size="sm" variant="ghost" aria-label={t`Revoke ${name}`} onClick={() => setConfirming(true)}>
          <Trans>Revoke</Trans>
        </Button>
      )}
    </li>
  );
}

/** Names the token, picks its scope and expiry, and makes it. */
function CreateTokenSheet({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: (made: Made) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const formId = useId();
  const expiryId = useId();
  const scopeName = useId();
  const [name, setName] = useState("");
  const [scope, setScope] = useState(TokenScope.READ_ONLY);
  const [expiry, setExpiry] = useState<Expiry>("90");
  const [nameError, setNameError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();

  // Each time the sheet opens, it starts over.
  useEffect(() => {
    if (open) return;
    setName("");
    setScope(TokenScope.READ_ONLY);
    setExpiry("90");
    setNameError(undefined);
    setProblem(undefined);
  }, [open]);

  const create = async (event: FormEvent) => {
    event.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) {
      setNameError(t`Name the token after what will use it, such as GitHub Actions deploy.`);
      return;
    }
    setNameError(undefined);
    setBusy(true);
    setProblem(undefined);
    try {
      // Your browser asks for your passkey, to confirm it's you making it.
      const proof = await stepUp(transport, StepUpAction.CREATE_TOKEN);
      const res = await createClient(TokenService, transport).createToken({
        name: trimmed,
        scope,
        proof,
        // A ttl, so the control plane's clock sets the expiry, whatever the browser's says.
        expiration:
          expiry === "never" ? { case: undefined } : { case: "ttl", value: durationFromMs(Number(expiry) * DAY) },
      });
      onCreated({
        name: res.token?.name ?? trimmed,
        secret: res.secret,
        expires: res.token?.expireTime ? timestampDate(res.token.expireTime) : undefined,
      });
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  const scopes = [
    {
      value: TokenScope.READ_ONLY,
      label: t`Read only`,
      hint: t`Reads servers and their state. Changes nothing.`,
    },
    {
      value: TokenScope.READ_WRITE,
      label: t`Read and write`,
      hint: t`Also plans and applies changes that can be undone. Never removes data or cuts off access.`,
    },
  ];

  return (
    <Sheet
      open={open}
      onClose={onClose}
      title={t`Create an API token`}
      subtitle={t`For a script, the CLI or an integration`}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>You see the token once, right after you create it.</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button type="submit" form={formId} variant="primary" icon="key" loading={busy}>
            <Trans>Create token</Trans>
          </Button>
        </>
      }
    >
      <form id={formId} className="tokens-form" onSubmit={(e) => void create(e)} noValidate>
        <TextField
          label={t`Name`}
          placeholder={t`What will use this token?`}
          value={name}
          maxLength={100}
          error={nameError}
          onChange={(e) => setName(e.target.value)}
        />
        <fieldset className="tokens-scopes">
          <legend className="dw-field-label">
            <Trans>Scope</Trans>
          </legend>
          {scopes.map((option) => (
            <label key={option.value} className={cx("tokens-scope", scope === option.value && "is-on")}>
              <input
                type="radio"
                name={scopeName}
                checked={scope === option.value}
                onChange={() => setScope(option.value)}
              />
              <span className="tokens-scope-text">
                <span className="tokens-scope-name">{option.label}</span>
                <span className="tokens-scope-hint">{option.hint}</span>
              </span>
            </label>
          ))}
          <span className="dw-field-hint">
            <Trans>Start with read only, and allow writes only when the job needs them.</Trans>
          </span>
        </fieldset>
        <div className="dw-field">
          <label className="dw-field-label" htmlFor={expiryId}>
            <Trans>Expires</Trans>
          </label>
          <div className="dw-input dw-input-md tokens-select">
            <select
              id={expiryId}
              className="dw-input-el"
              value={expiry}
              onChange={(e) => setExpiry(e.target.value as Expiry)}
            >
              <option value="30">{t`In 30 days`}</option>
              <option value="90">{t`In 90 days`}</option>
              <option value="365">{t`In 1 year`}</option>
              <option value="never">{t({ message: "Never", context: "token expiry" })}</option>
            </select>
            <Icon name="chevron-up-down" size={14} className="tokens-select-icon" />
          </div>
        </div>
        {problem ? (
          <Callout tone="critical" title={t`The token wasn't created`}>
            {problem}
          </Callout>
        ) : null}
      </form>
    </Sheet>
  );
}
