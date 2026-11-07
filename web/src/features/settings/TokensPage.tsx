// SPDX-License-Identifier: AGPL-3.0-only

import { durationFromMs, timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { StepUpAction, type Token, TokenGrant, TokenScope, TokenService } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Panel,
  PHONE_QUERY,
  SelectField,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { formatDay } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useControlPlaneNow } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { CopyButton } from "../system/CopyCommand";
import "./Settings.css";

const DAY = 86_400_000;
/** A token that expires sooner than this gets a warning. */
export const SOON = 14 * DAY;
/** The most characters a token's name holds, as the control plane takes. */
const MAX_NAME = 100;

type Expiry = "30" | "90" | "365" | "never";

/** A token just made, with its secret, which is shown only now. */
export interface Made {
  id: string;
  name: string;
  secret: string;
  /** What the list shows of it, such as "dwt_4f9K…x9Ta". */
  hint: string;
  expires?: Date | undefined;
}

/**
 * API tokens, as API-Tokens.dc.html has them: the one just made, shown once;
 * every token that still works, where it was last used and when it expires,
 * each revocable; and a form to make another, which your passkey confirms
 * (ADR 0016, ADR 0022).
 */
export function TokensPage() {
  const { t } = useLingui();
  const tokens = useQuery(TokenService.method.listTokens, {});
  const explain = useAuthError();
  const phone = useMediaQuery(PHONE_QUERY);
  const now = useControlPlaneNow();
  const [made, setMade] = useState<Made>();
  const list = tokens.data?.tokens ?? [];
  const soon = list.filter((token) => expiresWithin(token, now, SOON)).length;
  const count = list.length === 1 ? t`1 token` : t`${list.length} tokens`;
  let meta = "";
  if (tokens.data)
    meta = soon === 0 ? count : soon === 1 ? t`${count} · 1 expires soon` : t`${count} · ${soon} expire soon`;

  let body: ReactNode;
  if (tokens.error && !tokens.data) {
    body = (
      <Callout
        tone="critical"
        title={t`Dockwell can't list your tokens`}
        actions={
          <Button size="sm" icon="refresh" loading={tokens.isFetching} onClick={() => void tokens.refetch()}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        {explain(tokens.error)}
      </Callout>
    );
  } else if (!tokens.data) {
    body = (
      <div className="tokens-skeleton" aria-busy="true">
        <span className="dw-sr">{t`Loading tokens`}</span>
      </div>
    );
  } else if (list.length === 0) {
    body = (
      <div className="dw-panel tokens-empty">
        <span className="tokens-empty-icon">
          <Icon name="key" size={20} />
        </span>
        <p>
          <Trans>
            No tokens yet. Make one for each script or integration below, so you can revoke one without stopping the
            rest.
          </Trans>
        </p>
      </div>
    );
  } else if (phone) {
    body = (
      <ul className="dw-panel tokens-list" aria-label={t`Your API tokens`}>
        {list.map((token) => (
          <li key={token.id} className="tokens-item">
            <TokenFacts token={token} now={now} isNew={token.id === made?.id} />
            <RevokeButton token={token} onRevoked={() => void tokens.refetch()} />
          </li>
        ))}
      </ul>
    );
  } else {
    body = <TokensTable tokens={list} now={now} newId={made?.id} onRevoked={() => void tokens.refetch()} />;
  }

  return (
    <Page
      title={t`API tokens`}
      lead={t`Tokens act for you from CI, scripts and the CLI, with only the scope you give them`}
      back="/settings"
    >
      {made ? <NewToken made={made} onDone={() => setMade(undefined)} /> : null}
      <section className="tokens-section" aria-labelledby="tokens-active">
        <div className="cp-section-head">
          <h2 id="tokens-active" className="dw-text-heading">
            <Trans>Active tokens</Trans>
          </h2>
          {meta ? <span className="settings-meta">{meta}</span> : null}
        </div>
        {body}
      </section>
      <CreateToken
        onCreated={(m) => {
          setMade(m);
          void tokens.refetch();
        }}
      />
    </Page>
  );
}

/** True for a token that stops working within ms of now. */
export function expiresWithin(token: Token, now: number, ms: number): boolean {
  return token.expireTime !== undefined && timestampDate(token.expireTime).getTime() - now < ms;
}

/** What a token may do, as tags: every token reads, and some write or read logs too. */
function Scopes({ token }: { token: Token }) {
  return (
    <span className="tokens-tags">
      <Tag label="read" value="*" />
      {token.scope === TokenScope.READ_WRITE ? <Tag label="write" value="undoable" /> : null}
      {token.grants.includes(TokenGrant.LOGS) ? <Tag label="logs" value="read" /> : null}
    </span>
  );
}

/** When it was last used, and where from. */
function LastUsed({ token, now }: { token: Token; now: number }) {
  const words = useTimeWords();
  if (!token.lastUsedTime) {
    return (
      <span className="settings-muted">
        <Trans>Never</Trans>
      </span>
    );
  }
  const ago = words.ago(timestampDate(token.lastUsedTime), now);
  return (
    <span className="dw-cell">
      <span>{ago}</span>
      {token.lastAddress ? (
        <span className="dw-cell-sub tokens-sans">
          <Trans>
            from <code>{token.lastAddress}</code>
          </Trans>
        </span>
      ) : null}
    </span>
  );
}

/** When it stops working: a warning within two weeks of it. */
function Expires({ token, now }: { token: Token; now: number }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  if (!token.expireTime) {
    return (
      <span className="settings-muted">
        <Trans>Never</Trans>
      </span>
    );
  }
  const when = timestampDate(token.expireTime);
  const day = formatDay(when, i18n.locale);
  if (when.getTime() - now < SOON) {
    return <StatusBadge status="degraded" label={day} meta={t`in ${words.span(Math.max(0, when.getTime() - now))}`} />;
  }
  return <span>{day}</span>;
}

/** The name, with "New" beside the one just made, and the hint that tells it apart. */
function TokenName({ token, isNew }: { token: Token; isNew: boolean }) {
  return (
    <span className="dw-cell">
      <span className="dw-cell-title tokens-title">
        {/* Two lines at most, so a long name leaves the dates and Revoke in view; the whole name is its tooltip. */}
        <span className="tokens-title-text" title={token.name}>
          {token.name}
        </span>
        {isNew ? (
          <span className="tokens-new">
            <Trans>New</Trans>
          </span>
        ) : null}
      </span>
      <span className="dw-cell-sub">{token.hint}</span>
    </span>
  );
}

/** When it was made. Each person sees only their own tokens, so who made it goes without saying. */
function Created({ token }: { token: Token }) {
  const { i18n } = useLingui();
  return <span>{token.createTime ? formatDay(timestampDate(token.createTime), i18n.locale) : ""}</span>;
}

function TokensTable({
  tokens,
  now,
  newId,
  onRevoked,
}: {
  tokens: readonly Token[];
  now: number;
  newId: string | undefined;
  onRevoked: () => void;
}) {
  const { t } = useLingui();
  const columns: TableColumn<Token>[] = [
    { key: "name", header: t`Token`, render: (token) => <TokenName token={token} isNew={token.id === newId} /> },
    { key: "scopes", header: t`Scopes`, width: 272, render: (token) => <Scopes token={token} /> },
    { key: "created", header: t`Created`, width: 96, render: (token) => <Created token={token} /> },
    { key: "used", header: t`Last used`, width: 128, render: (token) => <LastUsed token={token} now={now} /> },
    { key: "expires", header: t`Expires`, width: 148, render: (token) => <Expires token={token} now={now} /> },
    {
      key: "act",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 144,
      align: "right",
      render: (token) => <RevokeButton token={token} onRevoked={onRevoked} />,
    },
  ];
  return (
    <Table
      className="tokens-table"
      columns={columns}
      rows={tokens}
      selectedKey={newId ?? null}
      caption={t`Your API tokens`}
    />
  );
}

/** A token as a phone shows it: a stacked row, its facts one under another. */
function TokenFacts({ token, now, isNew }: { token: Token; now: number; isNew: boolean }) {
  const { t } = useLingui();
  return (
    <div className="tokens-facts">
      <TokenName token={token} isNew={isNew} />
      <Scopes token={token} />
      <dl className="settings-facts tokens-facts-list">
        <dt>{t`Last used`}</dt>
        <dd>
          <LastUsed token={token} now={now} />
        </dd>
        <dt>{t`Expires`}</dt>
        <dd>
          <Expires token={token} now={now} />
        </dd>
      </dl>
    </div>
  );
}

/** Revoke, then a word to confirm it: a revoked token stops at once, and can't come back. */
function RevokeButton({ token, onRevoked }: { token: Token; onRevoked: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
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
    <span className="tokens-revoke">
      {confirming ? (
        <>
          <Button size="sm" variant="danger" loading={busy} onClick={() => void revoke()}>
            <Trans>Revoke</Trans>
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
            <Trans>Keep</Trans>
          </Button>
        </>
      ) : (
        <Button size="sm" variant="ghost" aria-label={t`Revoke ${token.name}`} onClick={() => setConfirming(true)}>
          <Trans>Revoke</Trans>
        </Button>
      )}
      {problem ? (
        <span className="tokens-problem" role="alert">
          {problem}
        </span>
      ) : null}
    </span>
  );
}

/** The new token's secret, once: partly hidden until asked, with a button that copies it whole. */
function NewToken({ made, onDone }: { made: Made; onDone: () => void }) {
  const { t, i18n } = useLingui();
  const [shown, setShown] = useState(false);
  const { name, secret } = made;
  const expires = made.expires ? formatDay(made.expires, i18n.locale) : "";
  // Hidden as the list will show it, from its hint, which never holds the checksum: that
  // follows from the random digits, and would give more of them away.
  const [head = "", tail = ""] = made.hint.split("…");
  return (
    <Callout tone="healthy" className="tokens-made" title={t`Copy this token now. It won't be shown again.`}>
      <span>
        {expires ? (
          <Trans>
            Created <b>{name}</b>. Send it in the Authorization header, as <code>Bearer</code> and the token. It expires{" "}
            {expires}.
          </Trans>
        ) : (
          <Trans>
            Created <b>{name}</b>. Send it in the Authorization header, as <code>Bearer</code> and the token. It doesn't
            expire.
          </Trans>
        )}
      </span>
      <span className="tokens-secret-row">
        <span className="tokens-secret">
          {shown ? (
            <code>{secret}</code>
          ) : (
            <code>
              <span className="dw-sr">{t`The new token, partly hidden:`} </span>
              {head}
              <span className="tokens-dots" aria-hidden="true">
                ••••••••••••••••••••
              </span>
              {tail}
            </code>
          )}
          <Button
            size="sm"
            variant="ghost"
            icon="eye"
            aria-pressed={shown}
            aria-label={shown ? t`Hide the token` : t`Show the whole token`}
            onClick={() => setShown(!shown)}
          />
        </span>
        <CopyButton text={secret} label={t`Copy token`} copiedLabel={t`Token copied`} size="md" />
        <Button variant="ghost" onClick={onDone}>
          <Trans>Done</Trans>
        </Button>
      </span>
    </Callout>
  );
}

/** Names the token, picks what it may do and when it stops, and makes it, which your passkey confirms. */
function CreateToken({ onCreated }: { onCreated: (made: Made) => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const formId = useId();
  const [name, setName] = useState("");
  const [writes, setWrites] = useState(false);
  const [readsLogs, setReadsLogs] = useState(false);
  const [expiry, setExpiry] = useState<Expiry>("90");
  const [nameError, setNameError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();

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
      // Your browser asks for your passkey, to confirm it's you making it, once nothing else stops it.
      const res = await withStepUpIfAsked(transport, StepUpAction.CREATE_TOKEN, "", (proof) =>
        createClient(TokenService, transport).createToken({
          name: trimmed,
          scope: writes ? TokenScope.READ_WRITE : TokenScope.READ_ONLY,
          grants: readsLogs ? [TokenGrant.LOGS] : [],
          proof,
          // A ttl, so the control plane's clock sets the expiry, whatever the browser's says.
          expiration:
            expiry === "never" ? { case: undefined } : { case: "ttl", value: durationFromMs(Number(expiry) * DAY) },
        }),
      );
      onCreated({
        id: res.token?.id ?? "",
        name: res.token?.name ?? trimmed,
        secret: res.secret,
        hint: res.token?.hint ?? "",
        expires: res.token?.expireTime ? timestampDate(res.token.expireTime) : undefined,
      });
      setName("");
      setWrites(false);
      setReadsLogs(false);
      setExpiry("90");
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  const expiries = [
    { value: "90", label: t`In 90 days` },
    { value: "30", label: t`In 30 days` },
    { value: "365", label: t`In 1 year` },
    { value: "never", label: t({ message: "Never", context: "token expiry" }) },
  ];

  return (
    <Panel
      title={t`Create a token`}
      className="settings-panel tokens-create"
      actions={<span className="settings-meta">{t`Each new address it's used from goes in the audit log`}</span>}
    >
      <form id={formId} className="tokens-form" onSubmit={(e) => void create(e)} noValidate>
        <div className="tokens-fields">
          <TextField
            className="tokens-name"
            label={t`Name`}
            placeholder={t`What will use this token?`}
            value={name}
            maxLength={MAX_NAME}
            error={nameError}
            onChange={(e) => setName(e.target.value)}
          />
          <SelectField
            className="tokens-expiry"
            label={t`Expires`}
            value={expiry}
            options={expiries}
            onChange={(e) => setExpiry(e.target.value as Expiry)}
          />
        </div>
        <fieldset className="tokens-scopes">
          <legend className="dw-field-label">
            <Trans>Scopes</Trans>
          </legend>
          <div className="tokens-scope-grid">
            <ScopeCard
              label="read"
              value="*"
              checked
              disabled
              text={t`Read servers, apps, plans and activity. Every token can, and changes nothing.`}
            />
            <ScopeCard
              label="write"
              value="undoable"
              checked={writes}
              onChange={setWrites}
              text={t`Plan and apply changes that can be undone. Never removes data or cuts off access.`}
            />
            <ScopeCard
              label="logs"
              value="read"
              checked={readsLogs}
              onChange={setReadsLogs}
              text={t`Read apps' logs, which often hold what apps shouldn't print.`}
            />
          </div>
          <span className="dw-field-hint">
            <Trans>Start with read access, and add only what the job needs.</Trans>
          </span>
        </fieldset>
        {problem ? (
          <Callout tone="critical" title={t`The token wasn't created`}>
            {problem}
          </Callout>
        ) : null}
        <div className="tokens-foot">
          <Button type="submit" variant="primary" icon="key" loading={busy}>
            <Trans>Create token</Trans>
          </Button>
          <span className="settings-meta">
            <Trans>Your passkey confirms it. You see the token once, right after you create it.</Trans>
          </span>
        </div>
      </form>
    </Panel>
  );
}

interface ScopeCardProps {
  label: string;
  value: string;
  text: string;
  checked: boolean;
  disabled?: boolean;
  onChange?: (checked: boolean) => void;
}

/** One thing a token may do, as a card with a checkbox, as the mock draws it. */
function ScopeCard({ label, value, text, checked, disabled = false, onChange }: ScopeCardProps) {
  const hintId = useId();
  return (
    <label className={cx("tokens-scope", checked && "is-on", disabled && "is-fixed")}>
      {/* Named by its tag, as a person reads the card, and described by the sentence under it. */}
      <input
        type="checkbox"
        className="dw-sr"
        checked={checked}
        disabled={disabled}
        aria-label={`${label}:${value}`}
        aria-describedby={hintId}
        onChange={(e) => onChange?.(e.target.checked)}
      />
      <span className="tokens-box" aria-hidden="true">
        {checked ? <Icon name="check" size={12} /> : null}
      </span>
      <span className="tokens-scope-text">
        <span>
          <Tag label={label} value={value} />
        </span>
        <span className="tokens-scope-hint" id={hintId}>
          {text}
        </span>
      </span>
    </label>
  );
}
