// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeState, DatabaseEngine, DatabaseService, LinkPart, type ServiceInfo } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, SelectField, Sheet, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useMay } from "../auth/may";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { engineAndVersion, firstReading, firstUser, readOf, useDatabases } from "../databases/useDatabases";
import { useAllVariables } from "../variables/VariablesPage";
import { environmentOf, nameOf } from "./app";
import { appNames, candidates, holds, PARTS, passwordGuess, secretPart, serversOf, suggested } from "./connectDatabase";
import { usePlaces } from "./useServices";

import "./Apps.css";

/** The choice of a new database, named for the app. */
const NEW = "";
/** The fields a refusal can name, by its reason's end, as invalid_user. */
const FIELDS = ["id", "database", "user", "password_secret", "sets"] as const;
type Field = (typeof FIELDS)[number];
/** A name the link doesn't set. */
const UNSET = String(LinkPart.UNSPECIFIED);

/**
 * Connects an app from no catalog entry to a PostgreSQL, MySQL or MariaDB that
 * Dockwell runs, as one plan (ADR 0084, Connect a database). One choice a line,
 * defaults first: the server, the one in the app's own environment first; a new
 * database for the app, or one the server has, which keeps its data where it is,
 * signing in as a user there with the password the app holds now; and which of
 * the app's names the link sets, suggested from their names.
 */
export function ConnectDatabaseSheet({
  service,
  preset,
  onClose,
  onAddVariable,
}: {
  service: ServiceInfo;
  /** What's chosen already, as a database's Users tab opens it for one of its users (#798). */
  preset?: { server: string; database?: string; user?: string; password?: string };
  onClose: () => void;
  /** Closes the sheet for the Environment tab's own form, with its key ready to type. */
  onAddVariable: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(DatabaseService, transport), [transport]);
  const may = useMay();
  const places = usePlaces();
  const name = nameOf(service);
  const environment = environmentOf(service);
  const databases = useDatabases();
  const store = useAllVariables(environment);
  // Only a server in a project where the person may make its user: a developer or more.
  const servers = useMemo(
    () =>
      candidates(databases.data?.databases ?? [], service).filter(
        (db) => may(DatabaseService.method.connectDatabase, { environment: db.resource?.scope ?? "" }).allowed,
      ),
    [databases.data, service, may],
  );
  const names = useMemo(() => appNames(service, store.data?.variables), [service, store.data]);
  const [picked, setServer] = useState<string | undefined>(preset?.server);
  const server = servers.find((db) => db.id === picked) ?? servers[0];
  const has = holds(server);
  const clickHouse = server?.engine === DatabaseEngine.CLICKHOUSE;
  const hasAdminPassword = Boolean(
    server?.readings.length &&
      server.readings.every((r) => r.clickhousePasswordVerified && !r.checksNoOne && !r.unread),
  );
  const [wantsAdministrator, setAdministrator] = useState(false);
  const administrator = wantsAdministrator || (clickHouse && !hasAdminPassword);
  const dedicated = clickHouse && !administrator;
  const accounts = dedicated ? (readOf(firstReading(server))?.users ?? []).filter((u) => u.resourceId) : [];
  const availableDatabases = dedicated ? accounts.flatMap((u) => u.databases) : has.databases;
  const [pickedDatabase, setDatabase] = useState(preset?.database ?? NEW);
  const database =
    clickHouse && !availableDatabases.includes(pickedDatabase)
      ? !dedicated && availableDatabases.includes("default")
        ? "default"
        : dedicated
          ? NEW
          : (availableDatabases[0] ?? NEW)
      : pickedDatabase;
  const [user, setUser] = useState<string | undefined>(preset?.user);
  const [password, setPassword] = useState<string | undefined>(preset?.password);
  const [chosen, setParts] = useState<Map<string, LinkPart>>();
  const allowedPart = (part: LinkPart) =>
    (part !== LinkPart.NATIVE_PORT || clickHouse) &&
    (part !== LinkPart.PASSWORD || !clickHouse || dedicated || Boolean(server?.passwordSecret));
  const parts = new Map([...(chosen ?? suggested(names))].filter(([, part]) => allowedPart(part)));
  const existing = database !== NEW;
  const signsInAs =
    user ?? (has.users.includes(has.owners.get(database) ?? "") ? has.owners.get(database) : has.users[0]) ?? "";
  const passwordIn = password ?? passwordGuess(names);
  // Checking takes an admin where the server's environment is protected; otherwise
  // the plan checks it as it applies.
  const mayCheck = may(DatabaseService.method.checkDatabaseSignIn, {
    environment: server?.resource?.scope ?? "",
  }).allowed;
  const maySetPassword = may(DatabaseService.method.changeDatabasePassword, {
    environment: server?.resource?.scope ?? "",
  });
  const signIn = useSignInCheck(client, {
    on: !clickHouse && mayCheck && existing && server !== undefined && signsInAs !== "" && passwordIn !== "",
    id: server?.id ?? "",
    user: signsInAs,
    database,
    appProject: places.environments.find((e) => e.id === environment)?.scope ?? "",
    appEnvironment: environment,
    passwordSecret: passwordIn,
  });
  const refused = existing && (signIn.state === "no" || signIn.state === "admin" || signIn.state === "noRights");
  const secrets = names.filter((n) => n.secret);
  const [busy, setBusy] = useState(false);
  const [errors, setErrors] = useState<Partial<Record<Field, string>>>({});
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLSelectElement>(null);
  const place = (scope: string | undefined) => places.environments.find((e) => e.id === scope)?.label ?? "";
  const partWord: Record<number, string> = {
    [LinkPart.HOST]: t`Host`,
    [LinkPart.PORT]: clickHouse ? t`HTTP port` : t`Port`,
    [LinkPart.NATIVE_PORT]: t`Native port`,
    [LinkPart.ADDRESS]: t`Host and port`,
    [LinkPart.DATABASE]: t({ message: "Database name", context: "what a variable holds" }),
    [LinkPart.USER]: t`User`,
    [LinkPart.PASSWORD]: t`Password`,
    [LinkPart.URL]: t`Connection URL`,
  };
  const setsAny = [...parts.values()].some((p) => p !== LinkPart.UNSPECIFIED);

  const ask = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || !server || !setsAny || (clickHouse && administrator && !database)) return;
    setBusy(true);
    setErrors({});
    setFailure(undefined);
    const sets: Record<string, LinkPart> = {};
    for (const [n, p] of parts) if (p !== LinkPart.UNSPECIFIED) sets[n] = p;
    try {
      const res = await client.connectDatabase({
        id: server.id,
        app: name,
        appProject: places.environments.find((e) => e.id === environment)?.scope ?? "",
        appEnvironment: environment,
        database,
        user: dedicated
          ? (accounts.find((u) => u.databases.includes(database))?.name ?? "")
          : !clickHouse && existing
            ? signsInAs
            : "",
        dedicatedUser: dedicated,
        passwordSecret: !clickHouse && existing ? passwordIn : "",
        sets,
      });
      if (res.change) {
        const change = res.change;
        setPlan({
          kind: "plan",
          make: () => Promise.resolve({ change }),
          retryLabel: t`Choose again`,
          onDone: onClose,
          onClose: (after) => {
            if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.FAILED) setPlan(undefined);
            else onClose();
          },
        });
      }
    } catch (err) {
      const reason = reasonOf(err);
      const at = reason.slice("invalid_".length);
      if (reason.startsWith("invalid_") && FIELDS.includes(at as Field)) setErrors({ [at as Field]: sentence(err) });
      else setFailure(sentence(err));
    }
    setBusy(false);
  };

  const loading = databases.isPending || store.isPending;
  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`Connect ${name} to a database`}
      subtitle={t`Its values come from the link, and follow the database`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            disabled={!server || !setsAny || refused || (clickHouse && administrator && !database)}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void ask()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="app-form"
        aria-label={t`Connect a database`}
        onSubmit={(event) => void ask(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void ask(event);
        }}
      >
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        {!loading && servers.length === 0 ? (
          <Callout
            tone="info"
            title={t`No database on ${serversOf(service).length > 1 ? t`its servers` : (service.deployments[0]?.serverName ?? t`its server`)}`}
            actions={
              <Link to="/databases" className="dw-link">
                <Trans>Open Databases</Trans>
              </Link>
            }
          >
            <Trans>It connects to a PostgreSQL, MySQL, MariaDB or ClickHouse that runs where it does.</Trans>
          </Callout>
        ) : null}
        {server ? (
          <>
            <SelectField
              ref={field}
              label={t`Database server`}
              value={server.id}
              error={errors.id}
              options={servers.map((db) => ({
                value: db.id,
                label: [
                  db.resource?.name ?? "",
                  engineAndVersion(db),
                  db.resource?.scope === environment ? "" : place(db.resource?.scope),
                ]
                  .filter(Boolean)
                  .join(" · "),
              }))}
              onChange={(event) => {
                setServer(event.target.value);
                setDatabase(NEW);
                setUser(undefined);
                setErrors({});
              }}
            />
            {clickHouse ? (
              <SelectField
                label={t`Access`}
                value={administrator ? "administrator" : "app"}
                options={[
                  { value: "app", label: t`User for this app`, disabled: !hasAdminPassword },
                  { value: "administrator", label: t`Database administrator` },
                ]}
                onChange={(event) => {
                  setAdministrator(event.target.value === "administrator");
                  setDatabase(NEW);
                  setErrors({});
                }}
              />
            ) : null}
            {clickHouse && !hasAdminPassword ? (
              <Callout
                tone="info"
                actions={
                  <Button
                    size="sm"
                    {...maySetPassword.off}
                    onClick={() =>
                      setPlan({
                        kind: "plan",
                        make: () => client.changeDatabasePassword({ id: server.id }),
                        onClose: () => setPlan(undefined),
                      })
                    }
                  >
                    <Trans>Set a password</Trans>
                  </Button>
                }
              >
                <Trans>Set an administrator password to give this app its own user.</Trans>
              </Callout>
            ) : null}
            <SelectField
              label={t`Database`}
              value={database}
              error={errors.database}
              hint={existing ? t`Its data stays where it is` : undefined}
              options={[
                ...(!clickHouse || dedicated ? [{ value: NEW, label: t`A new one for ${name}` }] : []),
                ...availableDatabases.map((d) => ({ value: d, label: d })),
              ]}
              onChange={(event) => {
                setDatabase(event.target.value);
                setUser(undefined);
                setErrors({});
              }}
            />
            {clickHouse && administrator ? (
              <p className="settings-meta">
                <Trans>
                  Administrator <code>{firstUser(server, readOf(firstReading(server)))}</code> can read and change every
                  database. Only one app link can use this administrator.
                </Trans>
              </p>
            ) : null}
            {existing && !clickHouse ? (
              <>
                <SelectField
                  label={t`Signs in as`}
                  value={signsInAs}
                  error={
                    errors.user ??
                    (signIn.state === "admin"
                      ? t`${signsInAs} reaches past ${database}, as the server's administrator does, so it reaches every app's data. Give ${name} a new database of its own.`
                      : signIn.state === "noRights"
                        ? t`${signsInAs} has no rights in ${database}, so ${name} couldn't use it. Pick the user that owns it.`
                        : undefined)
                  }
                  hint={t`Never its administrator`}
                  options={has.users.map((u) => ({ value: u, label: u }))}
                  onChange={(event) => setUser(event.target.value)}
                />
                {signIn.state === "admin" ? (
                  <Button
                    size="sm"
                    variant="secondary"
                    className="app-connect-fix"
                    onClick={() => {
                      setDatabase(NEW);
                      setUser(undefined);
                    }}
                  >
                    <Trans>Use a new database</Trans>
                  </Button>
                ) : null}
                <SelectField
                  label={t`Its password is in`}
                  value={passwordIn}
                  error={
                    errors.password_secret ??
                    (signIn.state === "no"
                      ? t`${signsInAs} doesn't sign in with ${passwordIn}, so ${name} couldn't reach its data. Pick the secret that holds its password.`
                      : undefined)
                  }
                  hint={
                    signIn.state === "checking" ? (
                      t`Checking it signs in`
                    ) : signIn.state === "ok" ? (
                      <span className="app-connect-ok">
                        <Icon name="check" size={14} />
                        <Trans>Signs in</Trans>
                      </span>
                    ) : signIn.state === "error" ? (
                      <span title={signIn.said}>
                        <Trans>Couldn't check now. The plan checks it as it applies.</Trans>
                      </span>
                    ) : mayCheck ? undefined : (
                      t`Checked as the plan applies`
                    )
                  }
                  mono
                  options={[
                    ...(passwordIn ? [] : [{ value: "", label: t`Pick a secret` }]),
                    ...secrets.map((s) => ({ value: s.name, label: s.name })),
                  ]}
                  onChange={(event) => setPassword(event.target.value)}
                />
              </>
            ) : null}
            <fieldset
              className="app-connect-sets"
              aria-describedby={errors.sets ? "app-connect-sets-error" : undefined}
            >
              <legend className="dw-text-label">
                <Trans>What {name} reads</Trans>
              </legend>
              {names.map((n) => (
                <SelectField
                  key={n.name}
                  label={n.name}
                  mono
                  size="sm"
                  value={String(parts.get(n.name) ?? LinkPart.UNSPECIFIED)}
                  options={[
                    { value: UNSET, label: t`Not set` },
                    ...PARTS.filter((p) => allowedPart(p) && (n.secret || !secretPart(p))).map((p) => ({
                      value: String(p),
                      label: partWord[p] ?? "",
                    })),
                  ]}
                  onChange={(event) => {
                    const next = new Map(parts);
                    next.set(n.name, Number(event.target.value) as LinkPart);
                    setParts(next);
                  }}
                />
              ))}
              {names.length === 0 && !loading ? (
                <p className="settings-meta">
                  <Trans>{name} reads no variable or secret of its environment.</Trans>{" "}
                  <Button size="sm" variant="secondary" onClick={onAddVariable}>
                    <Trans>Add a variable</Trans>
                  </Button>
                </p>
              ) : null}
              {errors.sets ? (
                <p id="app-connect-sets-error" className="dw-field-error" role="alert">
                  {errors.sets}
                </p>
              ) : null}
            </fieldset>
          </>
        ) : null}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}

/** How the check of a sign-in stands, as the sheet shows it. */
interface SignInCheck {
  state: "idle" | "checking" | "ok" | "no" | "admin" | "noRights" | "error";
  said: string;
}

/** How long a choice waits for another before it's checked, so changing several checks once. */
const CHECK_AFTER_MS = 400;

/**
 * Checks, as each is chosen, that the user signs in with the password the app's
 * secret holds, from another host, as taking it on does, so no one learns at apply
 * that it's wrong (ADR 0084). Only the latest choice's answer counts.
 */
function useSignInCheck(
  client: ReturnType<typeof createClient<typeof DatabaseService>>,
  ask: {
    on: boolean;
    id: string;
    user: string;
    database: string;
    appProject: string;
    appEnvironment: string;
    passwordSecret: string;
  },
): SignInCheck {
  const [check, setCheck] = useState<SignInCheck>({ state: "idle", said: "" });
  const { on, id, user, database, appProject, appEnvironment, passwordSecret } = ask;
  useEffect(() => {
    if (!on) {
      setCheck({ state: "idle", said: "" });
      return;
    }
    let current = true;
    setCheck({ state: "checking", said: "" });
    const timer = setTimeout(() => {
      client
        .checkDatabaseSignIn({ id, user, database, appProject, appEnvironment, passwordSecret })
        .then((res) => {
          if (!current) return;
          if (res.administrator) setCheck({ state: "admin", said: "" });
          else if (res.signsIn && res.noRights) setCheck({ state: "noRights", said: "" });
          else setCheck({ state: res.signsIn ? "ok" : "no", said: res.said });
        })
        .catch((err: unknown) => {
          if (current) setCheck({ state: "error", said: sentence(err) });
        });
    }, CHECK_AFTER_MS);
    return () => {
      current = false;
      clearTimeout(timer);
    };
  }, [client, on, id, user, database, appProject, appEnvironment, passwordSecret]);
  return check;
}
