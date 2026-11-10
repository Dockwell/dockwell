// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type Database, DatabaseService, VariableService } from "@dockwell/api";
import { Advanced, Button, Callout, cx, Icon, PHONE_QUERY, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useRef, useState } from "react";
import { methodCommand } from "../../lib/cli";
import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { CopyAsCommand } from "../system/CopyCommand";
import { engineName } from "./useDatabases";
import "../apps/Apps.css";
import { useMay } from "../auth/may";

/** What a user's or a database's name takes, as the control plane checks it. */
const SQL_NAME = /^[a-z][a-z0-9_]{0,31}$/;

/** What a secret's name takes. */
const SECRET_NAME = /^[A-Za-z_][A-Za-z0-9_]{0,254}$/;

/** The secret a user's password goes in unless someone names another, as the control plane names it. */
export function secretFor(user: string): string {
  return `${user.toUpperCase()}_DATABASE_PASSWORD`;
}

/**
 * Adds a database and the user who owns it, with a password Dockwell generates and keeps
 * as a secret, which nobody types or sees. It asks only for the user's name, checked as
 * it's typed against the users the database has. Their database takes it too, and the
 * secret a name made from it: both under Advanced, which says them in its summary, with
 * the same as a CLI command.
 */
export function AddUserSheet({ db, onClose }: { db: Database; onClose: () => void }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const field = useRef<HTMLInputElement>(null);
  const [user, setUser] = useState("");
  const [database, setDatabase] = useState("");
  const [secret, setSecret] = useState("");
  const [userError, setUserError] = useState<string>();
  const [databaseError, setDatabaseError] = useState<string>();
  const [secretError, setSecretError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [plan, setPlan] = useState<Opening>();
  const name = db.resource?.name ?? "";
  // A user is a plan in the database's project, which is a developer's: off, with why, for a viewer (ADR 0059).
  const mayAdd = useMay()(DatabaseService.method.createDatabaseUser, { environment: db.resource?.scope ?? "" });
  const engine = engineName(db.engine);
  const wanted = user.trim();
  const [advancedOpen, setAdvancedOpen] = useState(false);
  // What's there already, so a name that's taken says so as it's typed, not after Review plan.
  const reading = db.readings[0];
  const users = new Set((reading?.users ?? []).map((u) => u.name));
  const databases = new Set((reading?.databases ?? []).map((d) => d.name));
  const environment = db.resource?.scope ?? "";
  const secrets = new Set(
    (
      useQuery(VariableService.method.listVariables, { environment }, { enabled: environment !== "" }).data
        ?.variables ?? []
    ).map((v) => v.name),
  );
  const theDatabase = database.trim() || wanted;
  const theSecret = secret.trim() || (wanted ? secretFor(wanted) : "");
  const nameRule = t`Use up to 32 lowercase letters, digits and underscores, starting with a letter, such as shop.`;
  const userCheck =
    wanted === ""
      ? undefined
      : !SQL_NAME.test(wanted)
        ? nameRule
        : users.has(wanted)
          ? t`${wanted} signs in to ${name} already.`
          : undefined;
  const databaseCheck =
    theDatabase === "" || (database.trim() !== "" && !SQL_NAME.test(database.trim()))
      ? database.trim() !== ""
        ? nameRule
        : undefined
      : databases.has(theDatabase)
        ? t`${name} has a database called ${theDatabase} already.`
        : undefined;
  const secretCheck =
    theSecret === ""
      ? undefined
      : secret.trim() !== "" && !SECRET_NAME.test(secret.trim())
        ? t`Use letters, digits and underscores, starting with a letter, such as SHOP_DATABASE_PASSWORD.`
        : secrets.has(theSecret)
          ? t`${theSecret} is a secret already.`
          : undefined;
  // A default that collides opens Advanced, and it stays open, so what to change is in sight as it's typed.
  if ((databaseCheck || secretCheck) && !advancedOpen) setAdvancedOpen(true);
  const command = wanted
    ? methodCommand(DatabaseService.method.createDatabaseUser, {
        id: name,
        user: wanted,
        database: database.trim(),
        secret: secret.trim(),
      })
    : "";

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    const badUser = wanted === "" ? nameRule : userCheck;
    setUserError(badUser);
    setDatabaseError(databaseCheck);
    setSecretError(secretCheck);
    // What it refused under Advanced has to be in sight to be fixed.
    if (databaseCheck || secretCheck) setAdvancedOpen(true);
    if (badUser || databaseCheck || secretCheck) return;
    setBusy(true);
    setFailure(undefined);
    let change: Change | undefined;
    try {
      change = (
        await createClient(DatabaseService, transport).createDatabaseUser({
          id: db.id,
          user: wanted,
          database,
          secret,
        })
      ).change;
    } catch (err) {
      // A refusal of one field goes beside it, and anything else above the buttons.
      const said = sentence(err);
      const reason = reasonOf(err);
      if (reason === "invalid_user") setUserError(said);
      else if (reason === "invalid_database") {
        setDatabaseError(said);
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
      title={t`Add a user to ${name}`}
      subtitle={t`With a database of their own, and a password Dockwell makes`}
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
          label={t`User`}
          value={user}
          mono
          autoComplete="off"
          spellCheck={false}
          placeholder="shop"
          error={userError ?? userCheck}
          hint={t`Who signs in to ${engine}.`}
          onChange={(event) => {
            setUser(event.target.value);
            setUserError(undefined);
          }}
        />
        <Advanced
          open={advancedOpen}
          onOpenChange={setAdvancedOpen}
          summary={theDatabase ? t`Database ${theDatabase} · secret ${theSecret}` : undefined}
        >
          <TextField
            label={t`Database`}
            value={database}
            mono
            optional
            autoComplete="off"
            spellCheck={false}
            placeholder={wanted || "shop"}
            error={databaseError ?? databaseCheck}
            hint={t`Theirs, to do anything in.`}
            onChange={(event) => {
              setDatabase(event.target.value);
              setDatabaseError(undefined);
            }}
          />
          <TextField
            label={t`Secret for the password`}
            value={secret}
            mono
            optional
            autoComplete="off"
            spellCheck={false}
            placeholder={secretFor(wanted || "shop")}
            error={secretError ?? secretCheck}
            hint={t`Dockwell generates the password into it; apps use it by name.`}
            onChange={(event) => {
              setSecret(event.target.value);
              setSecretError(undefined);
            }}
          />
          {command ? <CopyAsCommand command={command} /> : null}
        </Advanced>
        {failure ? (
          <Callout tone="critical" title={t`This user can't be planned`}>
            {failure}
          </Callout>
        ) : null}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}
