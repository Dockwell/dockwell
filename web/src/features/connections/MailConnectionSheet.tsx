// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  AlertService,
  ChangeService,
  ChangeState,
  type CheckConnectionResponse,
  type ConnectionInfo,
  ConnectionReach,
  ConnectionService,
  LinkKind,
  SmtpSecurity,
} from "@dockwell/api";
import { Advanced, Button, Callout, SelectField, Sheet, StatusBadge, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { channelsOn } from "../alerts/mail";
import { useChannels } from "../alerts/useAlerts";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAllResources } from "../projects/useProjects";
import { useFleetView } from "../servers/useFleet";
import {
  altPorts,
  defaultPort,
  MAIL_HOST,
  type MailProviderId,
  mailPlaceOf,
  mailProvider,
  privateAddress,
  SENDER,
  tlsPortFor,
} from "./mail";
import type { ProviderId } from "./providers";
import { freeConnectionName } from "./providers";
import { ServiceTiles } from "./ServiceTiles";
import { checkServer, SILENT, useCheckWords, useConnections } from "./useConnections";
import "./Connections.css";

/** Examples, which read as examples beside "e.g.", not as values someone typed (#799). */
const EXAMPLE_HOST = "smtp.example.com";
const EXAMPLE_SENDER = "notes@example.com";
const EXAMPLE_NAME = "Notes";
/**
 * Adds a connection to a mail server, or changes one, as a plan (ADR 0099). A person
 * picks the service, which fills in where its server is and who signs in, and pastes
 * its key and the address mail comes from. The server is checked when they leave the
 * password, or ask with Check, never as they type: the check signs in and sends
 * nothing, and a server counts each wrong try. Where the server can't reach the port,
 * the service's own other ports are one step away, and where a port says nothing, its
 * TLS port. Its name, port, security and which projects may use it are under
 * Advanced, at defaults.
 */
export function MailConnectionSheet({
  onClose,
  onSaved,
  existing = [],
  editing,
  project,
  start,
  forInstall,
  mailPrefill,
  rotateForAlerts,
  only,
  onPickStore,
  onCustom,
}: {
  onClose: () => void;
  onSaved?: (name: string) => void;
  existing?: readonly ConnectionInfo[];
  editing?: ConnectionInfo | undefined;
  project?: string | undefined;
  start: MailProviderId;
  forInstall?: string | undefined;
  /** Host, port, and security copied from an existing mail connection, with user and password empty (#833). */
  mailPrefill?: { host: string; port: number; security: SmtpSecurity } | undefined;
  /** True when opened to replace a password an app received before alerts may use this connection (#833). */
  rotateForAlerts?: boolean | undefined;
  /** Mail alone, as for an app's mail: storage's tiles don't show. */
  only?: "mail" | undefined;
  /** Picking a storage tile goes back to storage's form. */
  onPickStore: (id: ProviderId) => void;
  onCustom?: (() => void) | undefined;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(ConnectionService, transport), [transport]);
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayCheck = may(ConnectionService.method.checkConnection).allowed;
  const mayListChannels = may(AlertService.method.listAlertChannels).allowed;
  const words = useCheckWords();
  const listed = useConnections();
  const channels = useChannels(mayListChannels).data?.channels ?? [];
  const fleet = useFleetView();
  const services = useServices();
  const resources = useAllResources();
  const projects = (resources.data ?? [])
    .filter((r) => r.resource?.kind === "project")
    .map((r) => ({ id: r.id, label: r.displayName || r.resource?.name || r.id }));

  const was = editing ? mailPlaceOf(editing.host) : mailPrefill ? mailPlaceOf(mailPrefill.host) : undefined;
  const [service, setService] = useState<MailProviderId>(was?.provider ?? start);
  const p = mailProvider(service);
  const [region, setRegion] = useState(was?.region || p.regions[0] || "");
  const [typedHost, setTypedHost] = useState(
    was?.provider === "mail" ? (editing?.host ?? mailPrefill?.host ?? "") : "",
  );
  const [security, setSecurity] = useState<SmtpSecurity>(
    editing?.security || mailPrefill?.security || SmtpSecurity.STARTTLS,
  );
  const [typedPort, setTypedPort] = useState<number | undefined>(editing?.port || mailPrefill?.port || undefined);
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [sender, setSender] = useState(editing?.sender ?? "");
  const [senderName, setSenderName] = useState(editing?.senderName ?? "");
  const [typedName, setTypedName] = useState<string>();
  const [label, setLabel] = useState(editing?.displayName ?? "");
  const installing = !editing && forInstall !== undefined;
  const startReach = editing?.reach ?? (project || installing ? ConnectionReach.PROJECTS : ConnectionReach.EVERY);
  const [reach, setReach] = useState(
    startReach === ConnectionReach.UNSPECIFIED ? ConnectionReach.PROJECTS : startReach,
  );
  const [picked, setPicked] = useState<string[]>(editing?.projects ?? (project ? [project] : []));
  const [server, setServer] = useState("");
  const [answer, setAnswer] = useState<CheckConnectionResponse>();
  const [checking, setChecking] = useState(false);
  const [failure, setFailure] = useState<string>();

  const pick = (id: ProviderId | MailProviderId) => {
    if (id === "r2" || id === "s3" || id === "b2" || id === "gcs" || id === "other") {
      onPickStore(id);
      return;
    }
    const next = mailProvider(id);
    setService(id);
    setRegion(next.regions[0] ?? "");
  };
  const host = service === "mail" ? typedHost.trim() : p.host(region);
  const port = typedPort ?? defaultPort(security);
  // Who signs in, where the service says it: Resend's is always resend, and Postmark's
  // server token is its user and its password both.
  const signsInAs = p.user || (p.userIsKey ? password : user);
  const name =
    editing?.name ??
    typedName ??
    freeConnectionName(
      p.name || "mail",
      existing.map((c) => c.name),
    );
  const servers = fleet.servers ?? [];
  // A saved connection's password goes only to the control server or a server an app
  // linked to it runs on, until it's typed (ADR 0099, #710).
  const passwordStays = editing !== undefined && password === "";
  const linkedOn = useMemo(() => {
    const apps = new Set(editing?.uses.flatMap((u) => u.appIds) ?? []);
    return new Set(
      (services.data ?? []).filter((s) => apps.has(s.id)).flatMap((s) => s.deployments.map((d) => d.serverId)),
    );
  }, [editing, services.data]);
  const checkable = passwordStays ? servers.filter((s) => s.control || linkedOn.has(s.id)) : servers;
  const from = (checkable.some((s) => s.id === server) ? server : "") || checkServer(checkable);
  const hostOk = MAIL_HOST.test(host);
  const senderOk = sender === "" || SENDER.test(sender);
  // A move sends the password somewhere it wasn't saved to go, so it's asked for again.
  const moved =
    editing !== undefined && (host !== editing.host || port !== editing.port || security !== editing.security);
  const signInNeeded = !editing || moved || rotateForAlerts || user !== "" || password !== "";
  const passwordKept = editing !== undefined && !moved && !rotateForAlerts && user === "" && password === "";
  const typedSignIn = password !== "" && (p.user !== "" || p.userIsKey || user !== "");
  // Without TLS, a password goes only to a private address, as the server holds.
  const clearRefused =
    security === SmtpSecurity.NONE &&
    (signsInAs !== "" || (editing !== undefined && !moved)) &&
    hostOk &&
    !privateAddress(host);
  const complete =
    hostOk && port > 0 && port < 65536 && senderOk && !clearRefused && (signInNeeded ? typedSignIn : true);
  const reaching = reach === ConnectionReach.EVERY || picked.length > 0 || installing;
  const passed = answer?.passed === true;
  // Only a server that never took the connection may be one an app reaches from its own
  // network: one that took it and said nothing was reached, as set, and fails.
  const unreached = answer !== undefined && !passed && SILENT.includes(answer.code) && answer.step === "connect";
  // A check signs in, which a server counts: a wrong password a few times can lock the
  // account or ban this server's address. So it never runs while a person types, only
  // when they leave the password, or ask, and once for what's typed (the SMTP review's
  // M2). An answer holds for what it checked alone.
  const what = JSON.stringify([host, port, security, signsInAs, password, from]);
  const [answerFor, setAnswerFor] = useState("");
  const [tried, setTried] = useState("");
  const fresh = answer !== undefined && answerFor === what;
  const ready =
    complete && reaching && name !== "" && (mayCheck && signInNeeded ? fresh && (passed || unreached) : true);
  const asked = useRef(0);
  const running = useRef<AbortController>(undefined);
  useEffect(() => () => running.current?.abort(), []);
  const check = async (key = what, at = { host, port, security }) => {
    if (!complete || !mayCheck) return;
    const mine = ++asked.current;
    running.current?.abort();
    const stop = new AbortController();
    running.current = stop;
    setTried(key);
    setChecking(true);
    setFailure(undefined);
    try {
      const said = await client.checkConnection(
        {
          name: editing?.name ?? "",
          smtp: {
            host: at.host,
            port: at.port,
            security: at.security,
            user: password === "" ? "" : signsInAs,
            password,
          },
          server: from,
        },
        { signal: stop.signal },
      );
      if (mine === asked.current) {
        setAnswer(said);
        setAnswerFor(key);
      }
    } catch (err) {
      if (mine === asked.current) setFailure(sentence(err));
    } finally {
      if (mine === asked.current) setChecking(false);
    }
  };
  // Leaving the password, with what it needs typed, checks it once for what's typed.
  const checkOnLeave = () => {
    if (signInNeeded && typedSignIn && tried !== what) void check();
  };
  // A way round that a person picks, as another port or TLS, checks at once: it's a
  // click, not typing.
  const moveTo = (to: { port?: number; security?: SmtpSecurity }) => {
    const nextSecurity = to.security ?? security;
    // TLS goes to a port that takes it: the service's like, as SES's 2587 to 2465.
    const nextPort =
      to.port ??
      (to.security === SmtpSecurity.TLS ? tlsPortFor(p, security, port) : undefined) ??
      typedPort ??
      defaultPort(nextSecurity);
    const next = { host, port: nextPort, security: nextSecurity };
    if (to.security !== undefined) setSecurity(to.security);
    if (nextPort !== defaultPort(nextSecurity) || typedPort !== undefined) setTypedPort(nextPort);
    const key = JSON.stringify([host, next.port, next.security, signsInAs, password, from]);
    void check(key, next);
  };

  const spec = () => ({
    displayName: label.trim() || (p.name ? `${p.name} · ${sender || host}` : sender || host),
    kind: LinkKind.SMTP,
    smtp: {
      host,
      port,
      security,
      user: password === "" ? "" : signsInAs,
      password,
      sender: sender.trim(),
      senderName: senderName.trim(),
    },
    reach: installing ? ConnectionReach.UNSPECIFIED : reach,
    projects: !installing && reach === ConnectionReach.PROJECTS ? picked : [],
    shared: editing?.shared ?? false,
  });
  const review = () =>
    sheet.open({
      kind: "propose",
      summary: editing ? t`Change the connection ${name}` : t`Send mail through ${host}`,
      edits: [{ resource: { kind: "connection", name }, spec: { kind: { case: "connection", value: spec() } } }],
      onClose: (change) => {
        if (
          !change ||
          change.state === ChangeState.PLANNED ||
          change.state === ChangeState.FAILED ||
          change.state === ChangeState.DISCARDED
        ) {
          return;
        }
        void listed.refetch().then(() => {
          onClose();
          onSaved?.(name);
        });
      },
    });
  const links = editing?.uses.length ?? 0;
  const alertChannels = editing ? channelsOn(editing.name, channels) : [];
  const firstChannel = alertChannels[0]?.name ?? "";
  const inUse =
    links > 0
      ? plural(links, {
          one: "# link names it. Disconnect it under Used by first.",
          other: "# links name it. Disconnect them under Used by first.",
        })
      : alertChannels.length > 0
        ? plural(alertChannels.length, {
            one: `Alert channel ${firstChannel} uses it. Remove it on Alerts first.`,
            other: "# alert channels use it. Remove them on Alerts first.",
          })
        : undefined;
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the connection ${name}`,
      edits: [{ resource: { kind: "connection", name }, delete: true }],
      onClose: (change) => {
        if (change) void listed.refetch().then(onClose);
      },
    });

  const app = forInstall ?? "";
  const who = installing
    ? t`${app}'s project`
    : reach === ConnectionReach.EVERY
      ? t`every project`
      : picked.length === 1
        ? (projects.find((x) => x.id === picked[0])?.label ?? t`1 project`)
        : plural(picked.length, { one: "# project", other: "# projects" });
  const securityWords: Record<number, string> = {
    [SmtpSecurity.STARTTLS]: t`STARTTLS`,
    [SmtpSecurity.TLS]: t`TLS from the start`,
    [SmtpSecurity.NONE]: t`No TLS`,
  };
  const summary = [t`Named ${name}`, t`for ${who}`, t`port ${port}`, securityWords[security] ?? ""].join(" · ");
  const said = fresh && answer ? words(answer, { port, signedIn: password !== "" && signsInAs !== "" }) : undefined;
  // Why Review plan is off, beside it, as the first thing still to do (#799). What a
  // check found, or that it's checking, the line above says already.
  const why = ready
    ? undefined
    : !hostOk
      ? t`Type the host`
      : !editing && !typedSignIn
        ? p.user || p.userIsKey
          ? t`Type the key`
          : t`Type the user and password`
        : !senderOk
          ? t`Fix the address it sends as`
          : !reaching
            ? t`Pick a project`
            : !checking && !fresh && !clearRefused
              ? t`Check it first`
              : undefined;
  // A port the server can't reach may be blocked by the server's provider, as 25 and
  // 587 often are: the service's own other ports are one step away (the review's M1).
  const blocked = fresh && answer?.step === "connect" && SILENT.includes(answer.code);
  const alts = blocked ? altPorts(p, security, port) : [];
  // It took the connection and said nothing: the port may want TLS from the start.
  const wantsTLS =
    fresh &&
    answer?.step === "hello" &&
    answer.code === "timeout" &&
    security !== SmtpSecurity.TLS &&
    tlsPortFor(p, security, port) !== undefined;

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`Connection ${editing.name}` : t`Add a connection`}
      subtitle={
        rotateForAlerts
          ? t`Paste a password made for alerts alone, and revoke the one the app had at your provider`
          : editing
            ? undefined
            : t`A mail server, for apps to send mail through`
      }
      footer={
        <>
          {editing ? (
            <Button variant="ghost" className="connection-remove" onClick={remove} off={inUse} {...mayPlan.off}>
              <Trans>Remove</Trans>
            </Button>
          ) : null}
          {why && mayPlan.allowed ? <span className="connection-sheet-why">{why}</span> : null}
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!ready} onClick={review} {...mayPlan.off}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="connection-sheet">
        <ServiceTiles
          onCustom={onCustom}
          picked={service}
          onPick={pick}
          only={editing ? "mail" : only}
          disabled={editing !== undefined}
        />
        {p.regions.length > 0 ? (
          <SelectField
            label={t`Region`}
            value={region}
            options={p.regions.map((r) => ({ value: r, label: p.id === "mailgun" ? r.toUpperCase() : r }))}
            onChange={(event) => setRegion(event.target.value)}
          />
        ) : null}
        {service === "mail" ? (
          <TextField
            label={t`Host`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={typedHost}
            placeholder={t`e.g. ${EXAMPLE_HOST}`}
            hint={typedHost !== "" && !hostOk ? t`Its name or address alone, with the port apart` : undefined}
            onChange={(event) => setTypedHost(event.target.value.trim())}
          />
        ) : null}
        <div className="connection-sheet-row">
          {p.user || p.userIsKey ? null : (
            <TextField
              label={t`User`}
              mono
              autoComplete="off"
              spellCheck={false}
              value={user}
              placeholder={passwordKept ? t`Kept unless you type one` : undefined}
              onChange={(event) => setUser(event.target.value.trim())}
              onBlur={() => password !== "" && checkOnLeave()}
            />
          )}
          <TextField
            label={p.userIsKey ? t`Server token` : p.user ? t`API key` : t`Password`}
            mono
            type="password"
            autoComplete="off"
            value={password}
            placeholder={passwordKept ? t`Kept unless you type one` : undefined}
            onChange={(event) => setPassword(event.target.value.trim())}
            onBlur={checkOnLeave}
          />
        </div>
        <div className="connection-sheet-row">
          <TextField
            label={t`Sends as`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={sender}
            placeholder={t`e.g. ${EXAMPLE_SENDER}`}
            hint={!senderOk ? t`An email address alone, such as notes@example.com` : undefined}
            onChange={(event) => setSender(event.target.value.trim())}
          />
          <TextField
            label={t`Sender's name`}
            value={senderName}
            placeholder={t`e.g. ${EXAMPLE_NAME}`}
            onChange={(event) => setSenderName(event.target.value)}
          />
        </div>
        <div className="connection-sheet-check" aria-live="polite">
          {clearRefused ? (
            <>
              <StatusBadge status="down" label={t`Password kept back`} labelIsState />
              <span className="connection-sheet-said">
                {t`Without TLS, give the server's private address, such as 10.0.0.5, or use STARTTLS or TLS.`}
              </span>
            </>
          ) : editing && moved && !typedSignIn ? (
            <StatusBadge status="unknown" label={t`Moved: type its password again`} labelIsState />
          ) : editing && signInNeeded && !typedSignIn ? (
            <StatusBadge status="unknown" label={t`Type the user and password`} labelIsState />
          ) : checking ? (
            <StatusBadge status="deploying" label={t`Checking`} labelIsState />
          ) : said ? (
            <>
              <StatusBadge
                status={said.passed ? "healthy" : unreached ? "unknown" : "down"}
                label={said.state}
                labelIsState
              />
              <span className="connection-sheet-said">{said.line}</span>
              {alts.map((alt) => (
                <Button key={alt} size="sm" variant="secondary" onClick={() => moveTo({ port: alt })}>
                  <Trans>Use port {alt}</Trans>
                </Button>
              ))}
              {wantsTLS ? (
                <Button size="sm" variant="secondary" onClick={() => moveTo({ security: SmtpSecurity.TLS })}>
                  <Trans>Use TLS</Trans>
                </Button>
              ) : null}
            </>
          ) : complete && mayCheck && (signInNeeded || editing) ? (
            <Button size="sm" variant="secondary" icon="refresh" onClick={() => void check()}>
              <Trans>Check</Trans>
            </Button>
          ) : null}
        </div>
        {failure ? (
          <Callout tone="critical" title={t`Not checked`}>
            {failure}
          </Callout>
        ) : null}

        <Advanced summary={summary}>
          {editing ? null : (
            <TextField
              label={t`Name`}
              mono
              value={name}
              hint={t`What an app's link names it by, and the CLI.`}
              onChange={(event) => setTypedName(event.target.value.trim())}
            />
          )}
          <TextField
            label={t`Label`}
            value={label}
            placeholder={p.name ? `${p.name} · ${sender || host || "mail"}` : sender || host || "mail"}
            onChange={(event) => setLabel(event.target.value)}
          />
          <SelectField
            label={t`Security`}
            value={String(security)}
            options={[SmtpSecurity.STARTTLS, SmtpSecurity.TLS, SmtpSecurity.NONE].map((s) => ({
              value: String(s),
              label: securityWords[s] ?? "",
            }))}
            hint={
              clearRefused
                ? t`Without TLS, give the server's private address, such as 10.0.0.5, or use STARTTLS or TLS.`
                : security === SmtpSecurity.NONE
                  ? t`Signs in only to a private address, such as 10.0.0.5.`
                  : undefined
            }
            onChange={(event) => setSecurity(Number(event.target.value) as SmtpSecurity)}
          />
          <TextField
            label={t`Port`}
            mono
            inputMode="numeric"
            value={String(port)}
            onChange={(event) => {
              const n = Number(event.target.value.replace(/\D/g, ""));
              setTypedPort(n > 0 ? n : undefined);
            }}
          />
          {installing ? (
            <div className="connection-reach-fixed">
              <span className="dw-field-label">
                <Trans>Reaches</Trans>
              </span>
              <span>
                <Trans>{app}'s project, once it's installed</Trans>
              </span>
            </div>
          ) : (
            <>
              <Switch
                checked={reach === ConnectionReach.PROJECTS}
                onChange={(on) => setReach(on ? ConnectionReach.PROJECTS : ConnectionReach.EVERY)}
                label={t`Only some projects`}
                description={t`Their apps alone may use it.`}
              />
              {reach === ConnectionReach.PROJECTS ? (
                <fieldset className="connection-projects">
                  <legend className="dw-sr">{t`Projects`}</legend>
                  {projects.map((x) => (
                    <label key={x.id} className="connection-project">
                      <input
                        type="checkbox"
                        checked={picked.includes(x.id)}
                        onChange={(event) =>
                          setPicked((was) => (event.target.checked ? [...was, x.id] : was.filter((y) => y !== x.id)))
                        }
                      />
                      <span>{x.label}</span>
                    </label>
                  ))}
                </fieldset>
              ) : null}
            </>
          )}
          {servers.length > 1 ? (
            <SelectField
              label={t`Checked from`}
              value={from}
              options={checkable.map((s) => ({ value: s.id, label: s.name }))}
              hint={
                checkable.length < servers.length
                  ? t`Its saved password stays on these servers. Type it to pick any.`
                  : undefined
              }
              onChange={(event) => setServer(event.target.value)}
            />
          ) : null}
        </Advanced>
      </div>
    </Sheet>
  );
}
