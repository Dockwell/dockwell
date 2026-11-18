// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  type CheckConnectionResponse,
  type ConnectionInfo,
  ConnectionInfoSchema,
  ConnectionReach,
  ConnectionService,
  type ConnectionUse,
  LinkKind,
  LinkPart,
  SmtpSecurity,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Panel,
  PHONE_QUERY,
  StatusBadge,
  Switch,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { usePlaces } from "../apps/useServices";
import { type Offer, useMay } from "../auth/may";
import { useInstalls } from "../catalog/useCatalog";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { ConnectionSheet } from "./ConnectionSheet";
import { seesWhere, where } from "./ConnectionsPage";
import { ConnectStorageSheet, useConnectableApps } from "./ConnectStorageSheet";
import { mailCheckOf, useLastCheck } from "./LastCheck";
import { appsOn } from "./storage";
import { checkServer, SILENT, useCheckWords, useConnections, usePartWords, useProjectLabel } from "./useConnections";
import { WantsMail } from "./WantsMail";
import "../apps/Apps.css";
import "./Connections.css";

/**
 * One connection: where its bucket is, which apps read it and as what, and the
 * ways to check it again from a server, use it in another app, and change it or
 * replace its keys (Connection-Detail, ADR 0099). Its keys are never shown: they
 * go to a server only sealed, with the values of an app that reads them.
 */
export function ConnectionPage({ name }: { name: string }) {
  const { t } = useLingui();
  const query = useConnections();
  const connections = query.data?.connections ?? [];
  const connection = connections.find((c) => c.name === name);
  const may = useMay();
  const mayChange = may(ChangeService.method.proposeChange, { install: true });
  const mayCheck = may(ConnectionService.method.checkConnection);
  const [editing, setEditing] = useState(false);
  const [using, setUsing] = useState(false);
  // A Check that passes or fails is kept as that server's last, so the list reads it again.
  const check = useCheck(name, () => void query.refetch());
  const last = useLastCheck(connection ?? EMPTY);
  const share = useShare(connection ?? EMPTY, () => void query.refetch());
  const phone = useMediaQuery(PHONE_QUERY);
  // Use in an app is for someone who may connect an app it reaches: not a viewer. A
  // mail server is set up from an app's need card instead.
  const connectable = useConnectableApps(connection).length > 0;
  const mail = connection?.kind === LinkKind.SMTP;
  const canUse = connectable && !mail;

  if (!connection) {
    return (
      <Page title={name} machineTitle breadcrumb={<Crumbs />} backLink={<BackToConnections />}>
        {query.isPending ? null : query.error ? (
          <Callout tone="critical" title={t`Dockwell can't read its connections`}>
            {sentence(query.error)}
          </Callout>
        ) : (
          <EmptyState icon="network" title={t`No connection named ${name}`}>
            <Link to="/connections" className="dw-link">
              <Trans>See every connection</Trans>
            </Link>
          </EmptyState>
        )}
      </Page>
    );
  }
  const admin = seesWhere(connection);
  const lead = where(connection) || connection.displayName;
  // One app per connection unless it's shared, as the server holds: two from before
  // keep working, and sharing it is one step away (the review's M2).
  const apps = appsOn(connection);
  const servesOne = !connection.shared && apps > 0;
  // Where the callout says it, with Share it, Use in an app says nothing more, so a
  // phone, which says an off control's reason at the top, says it once (the
  // re-check's N4).
  const unsharedMany = admin && !connection.shared && apps > 1;
  const useOff = unsharedMany
    ? { disabled: true }
    : servesOne
      ? { off: t`It serves an app already, and isn't shared` }
      : {};
  return (
    <Page
      title={connection.name}
      machineTitle
      breadcrumb={<Crumbs />}
      backLink={<BackToConnections />}
      badge={
        check.answer ? (
          <CheckState answer={check.answer} connection={connection} />
        ) : last ? (
          <StatusBadge status={last.status} label={last.label} meta={last.meta} labelIsState />
        ) : (
          <Unchecked />
        )
      }
      lead={lead}
      phoneSub={lead}
      phoneAction={
        admin && mayChange.allowed ? (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`Change`}
            onClick={() => setEditing(true)}
          >
            <Icon name="edit" size={20} />
          </button>
        ) : undefined
      }
      actions={
        <>
          {mayCheck.allowed ? (
            <Button icon="refresh" loading={check.busy} onClick={() => void check.run()}>
              <Trans>Check</Trans>
            </Button>
          ) : null}
          {admin ? (
            <Button icon="edit" onClick={() => setEditing(true)} {...mayChange.off}>
              <Trans>Change</Trans>
            </Button>
          ) : null}
          {canUse ? (
            <Button variant="primary" icon="plus" onClick={() => setUsing(true)} {...useOff}>
              <Trans>Use in an app</Trans>
            </Button>
          ) : null}
        </>
      }
      className="connection-page"
    >
      {check.failure ? (
        <Callout tone="critical" title={t`Not checked`}>
          {check.failure}
        </Callout>
      ) : null}
      {check.answer ? (
        <CheckLine answer={check.answer} connection={connection} />
      ) : last?.failing ? (
        <CheckLine answer={last.failing} connection={connection} />
      ) : null}
      {unsharedMany ? (
        <Callout
          tone="warning"
          title={plural(apps, {
            one: "# app uses it, but it isn't shared",
            other: "# apps use it, but it isn't shared",
          })}
          actions={
            <Button size="sm" onClick={() => share(true)} {...mayChange.off}>
              <Trans>Share it</Trans>
            </Button>
          }
        >
          <Trans>Share it to link another app beside them.</Trans>
        </Callout>
      ) : null}
      <UsedBy connection={connection} admin={admin} onDisconnected={() => void query.refetch()} />
      {admin ? (
        <Details
          connection={connection}
          onReplace={() => setEditing(true)}
          mayChange={mayChange}
          onSaved={() => void query.refetch()}
        />
      ) : null}
      {phone ? (
        // A phone's page has no head for its actions: they sit under it, as the mock has them.
        <div className="connection-phone-actions">
          {check.answer ? (
            <CheckState answer={check.answer} connection={connection} />
          ) : last ? (
            <StatusBadge status={last.status} label={last.label} meta={last.meta} labelIsState />
          ) : (
            <Unchecked />
          )}
          <div className="connection-phone-buttons">
            {mayCheck.allowed ? (
              <Button size="lg" icon="refresh" loading={check.busy} onClick={() => void check.run()}>
                <Trans>Check</Trans>
              </Button>
            ) : null}
            {canUse ? (
              <Button size="lg" variant="primary" icon="plus" onClick={() => setUsing(true)} {...useOff}>
                <Trans>Use in an app</Trans>
              </Button>
            ) : null}
          </div>
        </div>
      ) : null}
      {editing ? (
        <ConnectionSheet onClose={() => setEditing(false)} existing={connections} editing={connection} />
      ) : null}
      {using ? <ConnectStorageSheet connection={connection} onClose={() => setUsing(false)} /> : null}
    </Page>
  );
}

/** No connection, for the hooks a page calls before it knows there's one. */
const EMPTY = create(ConnectionInfoSchema, {});

/** A check of a saved connection, from the first server that's connected. */
function useCheck(name: string, onChecked: () => void) {
  const transport = useTransport();
  const client = useMemo(() => createClient(ConnectionService, transport), [transport]);
  const fleet = useFleetView();
  const servers = fleet.servers ?? [];
  const from = checkServer(servers);
  const [answer, setAnswer] = useState<CheckConnectionResponse>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const run = async () => {
    setBusy(true);
    setFailure(undefined);
    try {
      setAnswer(await client.checkConnection({ name, server: from }));
      onChecked();
    } catch (err) {
      setAnswer(undefined);
      setFailure(sentence(err));
    }
    setBusy(false);
  };
  return { answer, failure, busy, run };
}

/**
 * Nothing checked it yet: the control plane checks each connection within a minute
 * of its saving or a start, and Check checks it now (#799).
 */
function Unchecked() {
  const { t } = useLingui();
  return <StatusBadge status="unknown" label={t`Not checked yet`} labelIsState />;
}

function CheckState({ answer, connection }: { answer: CheckConnectionResponse; connection: ConnectionInfo }) {
  const words = useCheckWords()(answer, mailCheckOf(connection));
  const silent = SILENT.includes(answer.code);
  return (
    <StatusBadge status={words.passed ? "healthy" : silent ? "unknown" : "down"} label={words.state} labelIsState />
  );
}

/** What a check found, as a line under the page's head, where it didn't pass. */
function CheckLine({ answer, connection }: { answer: CheckConnectionResponse; connection: ConnectionInfo }) {
  const words = useCheckWords()(answer, mailCheckOf(connection));
  const { t } = useLingui();
  if (words.passed) return null;
  return (
    <Callout tone="warning" title={t`${words.state} from ${answer.server || t`the server`}`}>
      {words.line}
    </Callout>
  );
}

/**
 * Each link to the connection: the app that reads it, where, the names it sets and
 * where its objects go, and Disconnect, which plans letting go of the link with each
 * value kept as it reads now, so the connection can be removed.
 */
function UsedBy({
  connection,
  admin,
  onDisconnected,
}: {
  connection: ConnectionInfo;
  /** An install admin keeps a connection's keys on Disconnect, as secrets; below one, they go. */
  admin: boolean;
  onDisconnected: () => void;
}) {
  const { t } = useLingui();
  const places = usePlaces();
  const partWord = usePartWords();
  // A mail server keeps no files, so no link says where they go.
  const mail = connection.kind === LinkKind.SMTP;
  const phone = useMediaQuery(PHONE_QUERY);
  const may = useMay();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(ConnectionService, transport), [transport]);
  const disconnect = (u: ConnectionUse) =>
    sheet.open({
      kind: "plan",
      make: () => client.disconnectStorage({ project: u.project, environment: u.environment, link: u.link }),
      onClose: (after) => {
        if (after && after.state !== ChangeState.PLANNED && after.state !== ChangeState.FAILED) onDisconnected();
      },
    });
  // A link a catalog install's pin names for a need is the install's: disconnecting it
  // changes what the install is pinned to, which is an admin's or the owner's, so
  // below one it's off with why (the slice 3 review, L4), and off while the installs
  // load, until it's known whose it is (its re-check, N5).
  const listed = useInstalls();
  const installs = listed.data?.installs ?? [];
  const pinned = (u: ConnectionUse) =>
    installs.some((i) => i.projectId === u.project && i.pin?.needs[u.link]?.connection === connection.name);
  const disconnectButton = (u: ConnectionUse) => {
    const allowed =
      !admin && listed.isPending
        ? { allowed: false, off: { disabled: true as const } }
        : pinned(u) && !admin
          ? {
              allowed: false,
              off: { disabled: true as const, off: t`An admin or the owner disconnects an install's storage` },
            }
          : may(ConnectionService.method.disconnectStorage, { environment: u.environment });
    const link = u.link;
    // Below an install admin, a connection's keys aren't theirs to keep: the names
    // that hold one go with the link, and the app runs without them.
    const keysGo =
      !admin && Object.values(u.sets).some((p) => p === LinkPart.ACCESS_KEY_ID || p === LinkPart.SECRET_KEY);
    const button = (
      <Button
        size="sm"
        variant="secondary"
        aria-label={t`Disconnect ${link}`}
        onClick={() => disconnect(u)}
        {...allowed.off}
      >
        <Trans>Disconnect</Trans>
      </Button>
    );
    return keysGo ? (
      <span className="connection-disconnect">
        {button}
        <span className="connections-meta">
          <Trans>Its keys go with it</Trans>
        </span>
      </span>
    ) : (
      button
    );
  };
  if (connection.uses.length === 0) {
    return (
      <Panel title={t`Used by`} flush={mail}>
        {mail ? (
          <WantsMail connection={connection} onUsed={onDisconnected} />
        ) : (
          <div className="connection-unused">
            <span className="connections-meta">
              <Trans>No app reads it yet.</Trans>
            </span>
          </div>
        )}
      </Panel>
    );
  }
  const apps = (u: ConnectionUse) =>
    u.appIds.length > 0 ? (
      <span className="connection-apps">
        {u.appIds.map((id, i) => (
          <Link key={id} to="/apps/$serviceId/environment" params={{ serviceId: id }} className="connections-mono">
            {u.apps[i]}
          </Link>
        ))}
      </span>
    ) : (
      <span className="connections-meta">
        <Trans>No app reads it</Trans>
      </span>
    );
  const sets = (u: ConnectionUse) => (
    <ul className="connection-sets">
      {Object.entries(u.sets)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([n, part]) => (
          <li key={n} className="connection-set">
            <span className="connections-mono">{n}</span>
            <Icon name="arrow-left" size={12} className="connection-sets-arrow" />
            <span className="connection-part">{partWord[part] ?? ""}</span>
          </li>
        ))}
    </ul>
  );
  if (phone) {
    // A phone has no room for four columns: each link reads top to bottom.
    return (
      <Panel title={t`Used by`} flush>
        <ul className="connection-uses">
          {connection.uses.map((u) => (
            <li key={u.linkId} className="connection-use">
              <div className="dw-cell">
                {apps(u)}
                <span className="connections-meta">
                  {mail
                    ? places.label(u.environment)
                    : [places.label(u.environment), t`objects under ${u.prefix || "/"}`].join(" · ")}
                </span>
              </div>
              {sets(u)}
              <div className="connection-use-actions">{disconnectButton(u)}</div>
            </li>
          ))}
        </ul>
      </Panel>
    );
  }
  const columns: TableColumn<ConnectionUse>[] = [
    {
      key: "app",
      header: t`App`,
      render: (u) => (
        <div className="dw-cell">
          {apps(u)}
          <span className="connections-meta">{places.label(u.environment)}</span>
        </div>
      ),
    },
    { key: "sets", header: t`Names it sets`, render: sets },
    ...(mail
      ? []
      : [
          {
            key: "prefix",
            header: t`Objects under`,
            width: 180,
            mono: true,
            render: (u: ConnectionUse) => u.prefix || "/",
          },
        ]),
    { key: "link", header: t`Link`, width: 140, mono: true, muted: true, render: (u) => u.link },
    {
      key: "more",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 150,
      align: "right",
      render: disconnectButton,
    },
  ];
  return (
    <Panel title={t`Used by`} flush>
      <Table columns={columns} rows={connection.uses} rowKey="linkId" caption={t`Used by`} />
    </Panel>
  );
}

/** Where the bucket is and who may use it: an admin's to see. */
function Details({
  connection,
  onReplace,
  mayChange,
  onSaved,
}: {
  connection: ConnectionInfo;
  onReplace: () => void;
  mayChange: Offer;
  onSaved: () => void;
}) {
  const { t } = useLingui();
  const projectName = useProjectLabel();
  // A connection serves one app unless an admin shares it: then each app that keeps to
  // a prefix of the bucket may keep its files there, under its own (ADR 0099). It stays
  // shared while more than one app uses it, which the server refuses too.
  const apps = appsOn(connection);
  const stuck = connection.shared && apps > 1;
  const share = useShare(connection, onSaved);
  const reach =
    connection.reach === ConnectionReach.EVERY
      ? t`Every project`
      : connection.projects.length === 0
        ? t`No project yet`
        : connection.projects.map(projectName).join(", ");
  const mail = connection.kind === LinkKind.SMTP;
  const security =
    connection.security === SmtpSecurity.TLS
      ? t`TLS from the start`
      : connection.security === SmtpSecurity.NONE
        ? t`No TLS`
        : t`STARTTLS`;
  const sender = connection.senderName ? `${connection.senderName} <${connection.sender}>` : connection.sender;
  const rows: { label: string; value: string; mono?: boolean }[] = mail
    ? [
        { label: t`Host`, value: connection.host, mono: true },
        { label: t`Port`, value: String(connection.port), mono: true },
        { label: t`Security`, value: security },
        { label: t`Sends as`, value: sender || t`What each app says`, mono: sender !== "" },
        { label: t`Projects`, value: reach },
      ]
    : [
        { label: t`Endpoint`, value: connection.endpoint, mono: true },
        { label: t`Region`, value: connection.region, mono: true },
        { label: t`Bucket`, value: connection.bucket, mono: true },
        {
          label: t`Address style`,
          value: connection.pathStyle ? t`Bucket in the path` : t`Bucket in the hostname`,
        },
        { label: t`Public address`, value: connection.publicUrl || t`None`, mono: connection.publicUrl !== "" },
        { label: t`Projects`, value: reach },
      ];
  return (
    <Panel title={t`Details`}>
      <dl className="connection-details">
        {rows.map((r) => (
          <div key={r.label}>
            <dt>{r.label}</dt>
            <dd className={r.mono ? "connections-mono" : undefined}>{r.value}</dd>
          </div>
        ))}
        <div>
          <dt>
            <Trans>Apps</Trans>
          </dt>
          <dd>
            <Switch
              checked={connection.shared}
              onChange={share}
              disabled={!mayChange.allowed || stuck}
              label={t`More than one app may use it`}
              description={
                !mayChange.allowed
                  ? mayChange.reason
                  : stuck
                    ? plural(apps, {
                        one: "Stays shared while # app uses it: disconnect all but one first",
                        other: "Stays shared while # apps use it: disconnect all but one first",
                      })
                    : mail
                      ? t`Each sends as any sender the server takes`
                      : t`Each keeps its files under a prefix of its own`
              }
            />
          </dd>
        </div>
        <div>
          <dt>{mail ? <Trans>Sign-in</Trans> : <Trans>Keys</Trans>}</dt>
          <dd className="connection-keys">
            <span className="connections-meta">
              <Icon name="lock" size={14} /> <Trans>Sealed, never shown</Trans>
            </span>
            <Button size="sm" variant="secondary" onClick={onReplace}>
              {mail ? <Trans>Replace password</Trans> : <Trans>Replace keys</Trans>}
            </Button>
          </dd>
        </div>
      </dl>
    </Panel>
  );
}

function Crumbs() {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/connections">
        <Trans>Connections</Trans>
      </Link>
    </nav>
  );
}

function BackToConnections() {
  const { t } = useLingui();
  return (
    <Link to="/connections" className="mobile-header-button" aria-label={t`Back`}>
      <Icon name="arrow-left" size={20} />
    </Link>
  );
}

/** Plans sharing a connection, or unsharing it, as a change of that alone. */
function useShare(connection: ConnectionInfo, onSaved: () => void) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const label = connection.displayName || connection.name;
  return (shared: boolean) =>
    sheet.open({
      kind: "propose",
      summary: shared ? t`Share the connection ${label}` : t`Stop sharing the connection ${label}`,
      edits: [
        {
          resource: { kind: "connection", name: connection.name },
          spec: { kind: { case: "connection", value: { ...specOf(connection), shared } } },
        },
      ],
      onClose: (change) => {
        if (change) onSaved();
      },
    });
}

/**
 * A connection's spec as ConnectionInfo shows it to an admin, with no keys, which an
 * update keeps where its address stays (ADR 0099): for a change of one setting, such
 * as whether it's shared.
 */
export function specOf(c: ConnectionInfo) {
  const at =
    c.kind === LinkKind.SMTP
      ? {
          smtp: { host: c.host, port: c.port, security: c.security, sender: c.sender, senderName: c.senderName },
        }
      : {
          s3: {
            endpoint: c.endpoint,
            region: c.region,
            bucket: c.bucket,
            pathStyle: c.pathStyle,
            publicUrl: c.publicUrl,
          },
        };
  return {
    displayName: c.displayName,
    kind: c.kind,
    ...at,
    reach: c.reach,
    projects: [...c.projects],
    shared: c.shared,
  };
}
