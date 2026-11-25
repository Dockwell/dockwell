// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeState,
  type Database,
  type DatabaseReading,
  DatabaseService,
  VariableService,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  cx,
  Icon,
  PHONE_QUERY,
  SelectField,
  Sheet,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useRef, useState } from "react";

import { methodCommand } from "../../lib/cli";
import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useShownName } from "../apps/names";
import { useMay } from "../auth/may";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { CopyAsCommand } from "../system/CopyCommand";
import { secretFor } from "./AddUserSheet";
import { engineName } from "./useDatabases";
import "../apps/Apps.css";

/** What a user's or a database's name takes, as the control plane checks it. */
const SQL_NAME = /^[a-z][a-z0-9_]{0,31}$/;
const SECRET_NAME = /^[A-Za-z_][A-Za-z0-9_]{0,254}$/;
const SYSTEM_DBS = new Set([
  "postgres",
  "template0",
  "template1",
  "mysql",
  "information_schema",
  "performance_schema",
  "sys",
]);

/**
 * Plans a new logical database inside a PostgreSQL, MySQL or MariaDB engine:
 * defaults first (database name + a new owner user with a generated password
 * kept as a secret), or pick an existing user on the server.
 */
export function NewDatabaseSheet({ db, read, onClose }: { db: Database; read: DatabaseReading; onClose: () => void }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const field = useRef<HTMLInputElement>(null);
  const [database, setDatabase] = useState("");
  const [owner, setOwner] = useState(""); // "" = create a new owner user (default)
  const [user, setUser] = useState("");
  const [secret, setSecret] = useState("");
  const [databaseError, setDatabaseError] = useState<string>();
  const [userError, setUserError] = useState<string>();
  const [secretError, setSecretError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [plan, setPlan] = useState<Opening>();
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const own = db.resource?.name ?? "";
  const name = useShownName()(db, "database").title;
  const engine = engineName(db.engine);
  const environment = db.resource?.scope ?? "";
  const mayAdd = useMay()(DatabaseService.method.createLogicalDatabase, { environment });

  const existingDatabases = new Set(read.databases.map((d) => d.name));
  const existingUsers = new Set(read.users.map((u) => u.name));
  const signInUsers = [...new Set(read.users.filter((u) => !u.system && u.canSignIn).map((u) => u.name))];
  const secrets = new Set(
    (
      useQuery(VariableService.method.listVariables, { environment }, { enabled: environment !== "" }).data
        ?.variables ?? []
    ).map((v) => v.name),
  );

  const wantedDB = database.trim();
  const creatingUser = owner === "";
  const theUser = creatingUser ? user.trim() || wantedDB : "";
  const theSecret = creatingUser ? secret.trim() || (theUser ? secretFor(theUser) : "") : "";

  const nameRule = t`Use up to 32 lowercase letters, digits and underscores, starting with a letter, such as analytics.`;
  const databaseCheck =
    wantedDB === ""
      ? undefined
      : !SQL_NAME.test(wantedDB)
        ? nameRule
        : SYSTEM_DBS.has(wantedDB)
          ? t`${wantedDB} is kept by ${engine} itself. Choose another name.`
          : existingDatabases.has(wantedDB)
            ? t`${name} already has a database called ${wantedDB}.`
            : undefined;
  const userCheck =
    !creatingUser || theUser === ""
      ? undefined
      : !SQL_NAME.test(theUser)
        ? nameRule
        : existingUsers.has(theUser)
          ? t`${theUser} signs in to ${name} already. Pick them as owner or choose another user name.`
          : undefined;
  const secretCheck =
    !creatingUser || theSecret === ""
      ? undefined
      : secret.trim() !== "" && !SECRET_NAME.test(secret.trim())
        ? t`Use letters, digits and underscores, starting with a letter, such as ANALYTICS_DATABASE_PASSWORD.`
        : secrets.has(theSecret)
          ? t`${theSecret} is a secret already.`
          : undefined;

  if (creatingUser && (userCheck || secretCheck) && !advancedOpen) setAdvancedOpen(true);

  const command = wantedDB
    ? methodCommand(DatabaseService.method.createLogicalDatabase, {
        id: own,
        database: wantedDB,
        owner,
        user: creatingUser ? user.trim() : "",
        secret: creatingUser ? secret.trim() : "",
      })
    : "";

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    const badDB = wantedDB === "" ? nameRule : databaseCheck;
    setDatabaseError(badDB);
    setUserError(userCheck);
    setSecretError(secretCheck);
    if (creatingUser && (userCheck || secretCheck)) setAdvancedOpen(true);
    if (badDB || userCheck || secretCheck) return;
    setBusy(true);
    setFailure(undefined);
    let change: Change | undefined;
    try {
      change = (
        await createClient(DatabaseService, transport).createLogicalDatabase({
          id: db.id,
          database: wantedDB,
          owner,
          user: creatingUser ? user.trim() : "",
          secret: creatingUser ? secret.trim() : "",
        })
      ).change;
    } catch (err) {
      const said = sentence(err);
      const reason = reasonOf(err);
      if (reason === "invalid_database") setDatabaseError(said);
      else if (reason === "invalid_user") {
        setUserError(said);
        setAdvancedOpen(true);
      } else if (reason === "invalid_secret") {
        setSecretError(said);
        setAdvancedOpen(true);
      } else setFailure(said);
      setBusy(false);
      return;
    }
    setBusy(false);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      onClose: (after) => {
        if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.FAILED) setPlan(undefined);
        else onClose();
      },
    });
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`New database in ${name}`}
      subtitle={
        creatingUser
          ? t`Creates a database and an owner user with a generated password`
          : t`Creates a database owned by ${owner}`
      }
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
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
            {...mayAdd.off}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="app-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        <TextField
          ref={field}
          label={t`Database`}
          value={database}
          mono
          autoComplete="off"
          spellCheck={false}
          placeholder="analytics"
          error={databaseError ?? databaseCheck}
          hint={t`The database to create in ${engine}.`}
          onChange={(event) => {
            setDatabase(event.target.value);
            setDatabaseError(undefined);
          }}
        />
        <SelectField
          label={t`Owner`}
          value={owner}
          options={[
            { value: "", label: t`Create a new owner user` },
            ...signInUsers.map((u) => ({ value: u, label: u })),
          ]}
          hint={
            creatingUser
              ? t`Dockwell generates a password for the new user and keeps it as a secret.`
              : t`${owner} keeps its existing password and gets full access to this database.`
          }
          onChange={(event) => {
            setOwner(event.target.value);
            setUserError(undefined);
            setSecretError(undefined);
          }}
        />
        <Advanced
          open={advancedOpen}
          onOpenChange={setAdvancedOpen}
          summary={
            creatingUser
              ? theUser
                ? t`User ${theUser} · secret ${theSecret}`
                : t`A user and a secret named for the database`
              : t`CLI command`
          }
        >
          {creatingUser ? (
            <>
              <TextField
                label={t`User`}
                value={user}
                mono
                optional
                autoComplete="off"
                spellCheck={false}
                placeholder={wantedDB || "analytics"}
                error={userError ?? userCheck}
                hint={t`Who signs in to own this database.`}
                onChange={(event) => {
                  setUser(event.target.value);
                  setUserError(undefined);
                }}
              />
              <TextField
                label={t`Secret for the password`}
                value={secret}
                mono
                optional
                autoComplete="off"
                spellCheck={false}
                placeholder={secretFor(theUser || "analytics")}
                error={secretError ?? secretCheck}
                hint={t`Dockwell generates the password into it; apps use it by name.`}
                onChange={(event) => {
                  setSecret(event.target.value);
                  setSecretError(undefined);
                }}
              />
            </>
          ) : null}
          {command ? <CopyAsCommand command={command} /> : null}
        </Advanced>
        {failure ? (
          <Callout tone="critical" title={t`This database can't be planned`}>
            {failure}
          </Callout>
        ) : null}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}
