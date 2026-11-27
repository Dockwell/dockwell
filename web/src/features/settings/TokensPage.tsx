// SPDX-License-Identifier: AGPL-3.0-only

import { durationFromMs, timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  GitProviderService,
  ResourceService,
  Role,
  StepUpAction,
  type Token,
  TokenGrant,
  TokenRole,
  TokenScope,
  TokenService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Panel,
  PHONE_QUERY,
  SelectField,
  Sheet,
  Skeleton,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { formatDay } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { highestRole, ranksAbove, useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedIn } from "../auth/useAuthState";
import { useControlPlaneNow } from "../servers/useFleet";
import { ActionMenu } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { ListPager, type ListPlace, ListToolbar, sortableColumns, useListPlace } from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { CopyButton } from "../system/CopyCommand";
import "./Settings.css";

const DAY = 86_400_000;
/** A token that expires sooner than this gets a warning. */
export const SOON = 14 * DAY;
/** The most characters a token's name holds, as the control plane takes. */
const MAX_NAME = 100;

type Expiry = "30" | "90" | "365" | "never";

/** The new token's name field, which an empty list's Make a token goes to. */
const NEW_TOKEN_NAME = "new-token-name";
const SORT_KEYS = ["name", "scopes", "created", "used", "expires"] as const;

export interface TokensSearch extends ListSearch {
  scope?: "read" | "write" | "deploy";
}

function focusNewToken() {
  const field = document.getElementById(NEW_TOKEN_NAME);
  field?.scrollIntoView?.({ block: "center", behavior: "smooth" });
  field?.focus({ preventScroll: true });
}

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
export function TokensPage({ search, onSearch }: { search: TokensSearch; onSearch: (next: TokensSearch) => void }) {
  const { t } = useLingui();
  const tokens = useQuery(TokenService.method.listTokens, {});
  const explain = useAuthError();
  const phone = useMediaQuery(PHONE_QUERY);
  const now = useControlPlaneNow();
  const [made, setMade] = useState<Made>();
  const [editingRepo, setEditingRepo] = useState<Token>();
  const list = tokens.data?.tokens ?? [];
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const rows = sortedRows(
    list.filter(
      (token) =>
        (!search.scope ||
          (token.scope === TokenScope.READ_WRITE ? "write" : token.scope === TokenScope.DEPLOY ? "deploy" : "read") ===
            search.scope) &&
        `${token.name} ${token.hint}`.toLowerCase().includes(place.query),
    ),
    place.sort,
    (token, key) => {
      switch (key) {
        case "scopes":
          return token.scope;
        case "created":
          return token.createTime ? timestampDate(token.createTime).getTime() : undefined;
        case "used":
          return token.lastUsedTime ? timestampDate(token.lastUsedTime).getTime() : undefined;
        case "expires":
          return token.expireTime ? timestampDate(token.expireTime).getTime() : undefined;
        default:
          return token.name;
      }
    },
  );
  const page = pagedRows(rows, place.page);
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
      <Table
        className="tokens-table"
        columns={[
          { key: "name", header: t`Token`, render: () => null, skeleton: "titled" },
          { key: "scopes", header: t`Scopes`, width: 200, render: () => null, skeleton: "pill" },
          { key: "created", header: t`Created`, width: 96, render: () => null, skeleton: "short" },
          { key: "used", header: t`Last used`, width: 112, render: () => null, skeleton: "short" },
          { key: "expires", header: t`Expires`, width: 148, render: () => null, skeleton: "short" },
          { key: "act", header: <span className="dw-sr">{t`Actions`}</span>, width: 112, render: () => null },
        ]}
        rows={[]}
        loading
        loadingRows={2}
        loadingLabel={t`Loading tokens`}
      />
    );
  } else if (list.length === 0) {
    body = (
      <EmptyState
        icon="key"
        title={<Trans>No tokens yet</Trans>}
        action={
          <Button icon="plus" onClick={focusNewToken}>
            <Trans>Make a token</Trans>
          </Button>
        }
      >
        <p>
          <Trans>Make one for each script or integration, so you can revoke one without stopping the rest.</Trans>
        </p>
      </EmptyState>
    );
  } else if (rows.length === 0) {
    body = (
      <EmptyState
        icon="search"
        title={t`Nothing matches`}
        action={
          <Button
            onClick={() => {
              place.clearQuery();
              onSearch({ ...search, q: undefined, scope: undefined, page: undefined });
            }}
          >
            <Trans>Clear the filter</Trans>
          </Button>
        }
      />
    );
  } else if (phone) {
    body = (
      <ul className="dw-panel tokens-list" aria-label={t`Your API tokens`}>
        {page.rows.map((token) => (
          <li key={token.id} className="tokens-item">
            <TokenFacts token={token} now={now} isNew={token.id === made?.id} />
            <RevokeButton token={token} onRevoked={() => void tokens.refetch()} onEditRepo={setEditingRepo} />
          </li>
        ))}
      </ul>
    );
  } else {
    body = (
      <TokensTable
        place={place}
        tokens={page.rows}
        now={now}
        newId={made?.id}
        onRevoked={() => void tokens.refetch()}
        onEditRepo={setEditingRepo}
      />
    );
  }

  return (
    <Page
      width="narrow"
      title={t`API tokens`}
      lead={t`Tokens act for you from CI, scripts and the CLI, with only the scope you give them`}
      back="/settings"
      actions={
        <Button icon="plus" variant="primary" onClick={focusNewToken}>
          <Trans>New token</Trans>
        </Button>
      }
    >
      {made ? <NewToken made={made} onDone={() => setMade(undefined)} /> : null}
      <section className="tokens-section" aria-labelledby="tokens-active">
        <div className="cp-section-head">
          <h2 id="tokens-active" className="dw-text-heading">
            <Trans>Active tokens</Trans>
          </h2>
          {meta ? <span className="settings-meta">{meta}</span> : null}
        </div>
        {list.length > 0 ? (
          <ListToolbar place={place} label={t`Filter tokens`}>
            {search.scope || new Set(list.map((token) => token.scope)).size > 1 ? (
              <SelectField
                size="sm"
                aria-label={t`Scope`}
                value={search.scope ?? ""}
                options={[
                  { value: "", label: t`Every scope` },
                  { value: "read", label: t`Read only` },
                  { value: "write", label: t`Read and write` },
                  { value: "deploy", label: t`Deploy only` },
                ]}
                onChange={(event) =>
                  onSearch({
                    ...search,
                    scope:
                      event.target.value === "read" || event.target.value === "write" || event.target.value === "deploy"
                        ? event.target.value
                        : undefined,
                    page: undefined,
                  })
                }
              />
            ) : null}
          </ListToolbar>
        ) : null}
        {body}
        <ListPager page={page} onPage={place.setPage} />
      </section>
      <CreateToken
        taken={list.map((token) => token.name)}
        onCreated={(m) => {
          setMade(m);
          void tokens.refetch();
        }}
      />
      {editingRepo ? (
        <TokenRepositorySheet
          token={editingRepo}
          onClose={() => setEditingRepo(undefined)}
          onSaved={() => {
            setEditingRepo(undefined);
            void tokens.refetch();
          }}
        />
      ) : null}
    </Page>
  );
}

/**
 * A new token's name to start from: the day it's made, as "Token of 18 Nov", with a
 * number after where one of yours has it already.
 */
function useSuggestedName(taken: readonly string[]): string {
  const { t, i18n } = useLingui();
  const now = useControlPlaneNow();
  const day = formatDay(new Date(now), i18n.locale);
  const base = t`Token of ${day}`;
  if (!taken.includes(base)) return base;
  for (let n = 2; ; n++) {
    const next = `${base} (${n})`;
    if (!taken.includes(next)) return next;
  }
}

/** True for a token that stops working within ms of now. */
export function expiresWithin(token: Token, now: number, ms: number): boolean {
  return token.expireTime !== undefined && timestampDate(token.expireTime).getTime() - now < ms;
}

function shortRepoLabel(repoUrl: string): string {
  try {
    const parsed = new URL(repoUrl);
    const path = parsed.pathname.replace(/^\/+|\/+$/g, "").replace(/\.git$/i, "");
    return path ? `${parsed.host}/${path}` : parsed.host;
  } catch {
    return repoUrl;
  }
}

/**
 * What a token may do, as tags: every token reads, and some write or read logs
 * too; and for one narrowed, where it acts and as what (ADR 0071).
 */
function Scopes({ token }: { token: Token }) {
  const { t } = useLingui();
  // Every row says where it acts, a token for every project too (ADR 0071).
  const where =
    token.projects.length === 0
      ? t`every project you hold`
      : token.projects.map((p) => p.name || t`a project that's gone`).join(", ");
  return (
    <span className="tokens-tags">
      {token.scope === TokenScope.DEPLOY ? (
        <>
          <Tag label={t`Deploy`}>
            {token.deployServiceName ? (
              <Link
                className="cp-link"
                to="/apps/$serviceId/deployments"
                params={{ serviceId: token.deployService }}
                onClick={(event) => event.stopPropagation()}
              >
                {token.deployServiceName}
              </Link>
            ) : (
              t`App unavailable`
            )}
          </Tag>
          <span className="tokens-tag-where" title={token.deployRepository || t`No status repository`}>
            <Tag
              label={t`status`}
              value={token.deployRepository ? shortRepoLabel(token.deployRepository) : t`not set`}
            />
          </span>
        </>
      ) : (
        <Tag label="read" value="*" />
      )}
      {token.scope === TokenScope.READ_WRITE ? <Tag label="write" value="undoable" /> : null}
      {token.grants.includes(TokenGrant.LOGS) ? <Tag label="logs" value="read" /> : null}
      <span className="tokens-tag-where" title={where}>
        <Tag label="in" value={where} />
      </span>
      {token.role === TokenRole.DEVELOPER ? <Tag label="as" value="developer" /> : null}
      {token.role === TokenRole.VIEWER ? <Tag label="as" value="viewer" /> : null}
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
  place,
  now,
  newId,
  onRevoked,
  onEditRepo,
}: {
  tokens: readonly Token[];
  place: ListPlace;
  now: number;
  newId: string | undefined;
  onRevoked: () => void;
  onEditRepo: (token: Token) => void;
}) {
  const { t } = useLingui();
  const columns: TableColumn<Token>[] = [
    { key: "name", header: t`Token`, render: (token) => <TokenName token={token} isNew={token.id === newId} /> },
    // Scopes wrap, so the name takes what's left over (#801's review, L5).
    { key: "scopes", header: t`Scopes`, width: 200, render: (token) => <Scopes token={token} /> },
    { key: "created", header: t`Created`, width: 96, render: (token) => <Created token={token} /> },
    { key: "used", header: t`Last used`, width: 112, render: (token) => <LastUsed token={token} now={now} /> },
    { key: "expires", header: t`Expires`, width: 148, render: (token) => <Expires token={token} now={now} /> },
    {
      key: "act",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 136,
      align: "right",
      render: (token) => <RevokeButton token={token} onRevoked={onRevoked} onEditRepo={onEditRepo} />,
    },
  ];
  return (
    <Table
      className="tokens-table"
      columns={sortableColumns(columns, place, SORT_KEYS)}
      rows={tokens}
      onRowContextMenu={(_token, _at, row) => {
        row.querySelector<HTMLButtonElement>('button[aria-haspopup="menu"][aria-expanded="false"]')?.click();
      }}
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
function RevokeButton({
  token,
  onRevoked,
  onEditRepo,
}: {
  token: Token;
  onRevoked: () => void;
  onEditRepo: (token: Token) => void;
}) {
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
        <ActionMenu
          label={t`Actions for ${token.name}`}
          actions={[
            ...(token.scope === TokenScope.DEPLOY
              ? [
                  {
                    id: "repository",
                    label: token.deployRepository ? t`Change status repository` : t`Set status repository`,
                    icon: "commit" as const,
                    onSelect: () => onEditRepo(token),
                  },
                ]
              : []),
            { id: "revoke", label: t`Revoke`, icon: "trash", danger: true, onSelect: () => setConfirming(true) },
          ]}
        />
      )}
      {problem ? (
        <span className="tokens-problem" role="alert">
          {problem}
        </span>
      ) : null}
    </span>
  );
}

/** Binds a deploy token to the HTTPS repository whose commits it may report status to (ADR 0111). */
function TokenRepositorySheet({ token, onClose, onSaved }: { token: Token; onClose: () => void; onSaved: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const projectId = token.projects[0]?.id ?? "";
  const mayGitHub = may(GitProviderService.method.listGitRepositories, projectId ? { project: projectId } : undefined);
  const mayGitLab = may(GitProviderService.method.listGitLabProjects, projectId ? { project: projectId } : undefined);
  const ghRepos = useQuery(
    GitProviderService.method.listGitRepositories,
    { project: projectId },
    { enabled: Boolean(projectId) && mayGitHub.allowed },
  );
  const glProjects = useQuery(
    GitProviderService.method.listGitLabProjects,
    { project: projectId },
    { enabled: Boolean(projectId) && mayGitLab.allowed },
  );

  const options: { value: string; label: string }[] = [
    ...(ghRepos.data?.repositories ?? []).map((r) => ({
      value: r.url.startsWith("https://") ? r.url : `https://github.com/${r.fullName}`,
      label: `GitHub · ${r.fullName}`,
    })),
    ...(glProjects.data?.projects ?? []).map((p) => ({ value: p.url, label: `GitLab · ${p.pathWithNamespace}` })),
  ];
  const [selected, setSelected] = useState(token.deployRepository);
  const [custom, setCustom] = useState(token.deployRepository);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();

  const hasOptions = options.length > 0;
  const selectOptions =
    token.deployRepository && !options.some((o) => o.value === token.deployRepository)
      ? [{ value: token.deployRepository, label: shortRepoLabel(token.deployRepository) }, ...options]
      : options;

  const save = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(TokenService, transport).setTokenDeployRepository({
        id: token.id,
        deployRepository: hasOptions ? selected : custom.trim(),
      });
      onSaved();
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Status repository`}
      subtitle={token.name}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" loading={busy} onClick={() => void save()}>
            <Trans>Save</Trans>
          </Button>
        </>
      }
    >
      <div className="tokens-form">
        {hasOptions ? (
          <SelectField
            label={t`Status repository`}
            value={selected}
            onChange={(event) => setSelected(event.target.value)}
            hint={t`Dockwell reports commit and PR status for deploys made by this token only to this repository.`}
            options={[...selectOptions, { value: "", label: t`Don't report status` }]}
          />
        ) : (
          <TextField
            label={t`Status repository`}
            value={custom}
            onChange={(event) => setCustom(event.target.value)}
            placeholder="https://github.com/owner/repo"
            hint={t`Enter the full HTTPS repository URL, or leave blank to disable status reporting.`}
          />
        )}
        {problem ? <Callout tone="critical" title={problem} /> : null}
      </div>
    </Sheet>
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
function CreateToken({ onCreated, taken }: { onCreated: (made: Made) => void; taken: readonly string[] }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const formId = useId();
  // A name to start from, which says when it was made, so Create token works as it
  // opens; a person types over it with what will use the token (#801).
  const suggested = useSuggestedName(taken);
  const [typed, setName] = useState<string>();
  const name = typed ?? suggested;
  const [writes, setWrites] = useState(false);
  const [readsLogs, setReadsLogs] = useState(false);
  const [expiry, setExpiry] = useState<Expiry>("90");
  const [projects, setProjects] = useState<readonly string[]>([]);
  const [role, setRole] = useState<TokenRole>(TokenRole.UNSPECIFIED);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();

  const create = async (event: FormEvent) => {
    event.preventDefault();
    // Cleared, it takes the name it started with, rather than stop at an error.
    const trimmed = name.trim() || suggested;
    setBusy(true);
    setProblem(undefined);
    try {
      // Your browser asks for your passkey, to confirm it's you making it, once nothing else stops it.
      const res = await withStepUpIfAsked(transport, StepUpAction.CREATE_TOKEN, "", (proof) =>
        createClient(TokenService, transport).createToken({
          name: trimmed,
          scope: writes ? TokenScope.READ_WRITE : TokenScope.READ_ONLY,
          grants: readsLogs ? [TokenGrant.LOGS] : [],
          projects: [...projects],
          role,
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
      setName(undefined);
      setWrites(false);
      setReadsLogs(false);
      setExpiry("90");
      setProjects([]);
      setRole(TokenRole.UNSPECIFIED);
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
      actions={
        <span className="settings-meta">
          <Trans>
            Each new address it's used from goes in the{" "}
            <Link to="/settings/audit-log" className="cp-link">
              audit log
            </Link>
          </Trans>
        </span>
      }
    >
      <form id={formId} className="tokens-form" onSubmit={(e) => void create(e)} noValidate>
        <div className="tokens-fields">
          <TextField
            id={NEW_TOKEN_NAME}
            className="tokens-name"
            label={t`Name`}
            placeholder={t`What will use this token?`}
            value={name}
            maxLength={MAX_NAME}
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
        <Reach projects={projects} onProjects={setProjects} role={role} onRole={setRole} />
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

/**
 * Where a token acts and as what (ADR 0071): every project you hold, as you, in
 * one line, which Change opens to pick projects from yours and a role below
 * yours. Nothing to type: both are picked.
 */
function Reach({
  projects,
  onProjects,
  role,
  onRole,
}: {
  projects: readonly string[];
  onProjects: (projects: readonly string[]) => void;
  role: TokenRole;
  onRole: (role: TokenRole) => void;
}) {
  const { t } = useLingui();
  const [open, setOpen] = useState(false);
  // The projects you hold: the list is narrowed to them for anyone below an admin (ADR 0049).
  const listed = useQuery(ResourceService.method.listResources, { kind: "project" }, { enabled: open });
  const yours = useSignedIn()?.grants ?? [];
  const highest = highestRole(yours);
  const all = listed.data?.resources ?? [];
  const nameOf = (id: string) => {
    const p = all.find((r) => r.id === id);
    return p ? p.displayName || p.resource?.name || id : id;
  };
  const roles = [
    { value: String(TokenRole.UNSPECIFIED), label: t`Your role` },
    ...(ranksAbove(highest, Role.DEVELOPER) ? [{ value: String(TokenRole.DEVELOPER), label: t`Developer` }] : []),
    ...(ranksAbove(highest, Role.VIEWER) ? [{ value: String(TokenRole.VIEWER), label: t`Viewer` }] : []),
  ];
  const where = projects.length === 0 ? t`every project you hold` : projects.map(nameOf).join(", ");
  const as = role === TokenRole.DEVELOPER ? t`as a developer` : role === TokenRole.VIEWER ? t`as a viewer` : t`as you`;
  return (
    <fieldset className="tokens-scopes tokens-reach">
      <legend className="dw-field-label">
        <Trans>Where it acts</Trans>
      </legend>
      <div className="tokens-reach-summary">
        <span>{t`In ${where}, ${as}`}</span>
        <Button size="sm" variant="ghost" aria-expanded={open} onClick={() => setOpen(!open)}>
          {open ? <Trans>Done</Trans> : <Trans>Change</Trans>}
        </Button>
      </div>
      {open ? (
        <div className="tokens-reach-choices">
          <fieldset className="tokens-reach-projects" aria-label={t`Projects`}>
            {listed.data ? (
              all.map((p) => (
                <ProjectCheck
                  key={p.id}
                  name={p.displayName || p.resource?.name || p.id}
                  checked={projects.includes(p.id)}
                  onChange={(on) => onProjects(on ? [...projects, p.id] : projects.filter((id) => id !== p.id))}
                />
              ))
            ) : (
              <span className="settings-muted" aria-busy={listed.error ? undefined : true}>
                {listed.error ? t`Dockwell can't list your projects` : <Skeleton width={160} />}
              </span>
            )}
          </fieldset>
          {roles.length > 1 ? (
            <SelectField
              className="tokens-expiry"
              label={t`Acts as`}
              value={String(role)}
              options={roles}
              hint={
                role === TokenRole.DEVELOPER ? t`Its changes in a protected environment wait for an admin.` : undefined
              }
              onChange={(e) => onRole(Number(e.target.value) as TokenRole)}
            />
          ) : null}
        </div>
      ) : null}
    </fieldset>
  );
}

/** A project a token may act in, as a checkbox. None checked is every project you hold. */
function ProjectCheck({
  name,
  checked,
  onChange,
}: {
  name: string;
  checked: boolean;
  onChange: (on: boolean) => void;
}) {
  return (
    <label className={cx("tokens-scope tokens-project", checked && "is-on")}>
      <input type="checkbox" className="dw-sr" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      <span className="tokens-box" aria-hidden="true">
        {checked ? <Icon name="check" size={12} /> : null}
      </span>
      <span>{name}</span>
    </label>
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
