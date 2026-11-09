// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type Database, DatabaseService } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { engineName } from "./useDatabases";
import "../apps/Apps.css";

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
 * as a secret, which nobody types or sees. It asks only for the user's name: the
 * database takes it too, and the secret a name made from it, unless someone says other.
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
  const engine = engineName(db.engine);
  const wanted = user.trim();

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    const nameRule = t`Use up to 32 lowercase letters, digits and underscores, starting with a letter, such as shop.`;
    const badUser = !SQL_NAME.test(wanted);
    const badDatabase = database !== "" && !SQL_NAME.test(database);
    const badSecret = secret !== "" && !SECRET_NAME.test(secret);
    setUserError(badUser ? nameRule : undefined);
    setDatabaseError(badDatabase ? nameRule : undefined);
    setSecretError(
      badSecret
        ? t`Use letters, digits and underscores, starting with a letter, such as SHOP_DATABASE_PASSWORD.`
        : undefined,
    );
    if (badUser || badDatabase || badSecret) return;
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
      else if (reason === "invalid_database") setDatabaseError(said);
      else if (reason === "invalid_secret") setSecretError(said);
      else setFailure(said);
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
          error={userError}
          hint={t`Who signs in to ${engine}. Their database takes this name too, unless you give it another.`}
          onChange={(event) => setUser(event.target.value)}
        />
        <TextField
          label={t`Database`}
          value={database}
          mono
          optional
          autoComplete="off"
          spellCheck={false}
          placeholder={wanted || "shop"}
          error={databaseError}
          hint={t`It's theirs, to do anything in.`}
          onChange={(event) => setDatabase(event.target.value)}
        />
        <TextField
          label={t`Secret for the password`}
          value={secret}
          mono
          optional
          autoComplete="off"
          spellCheck={false}
          placeholder={secretFor(wanted || "shop")}
          error={secretError}
          hint={t`Dockwell makes a password of 32 characters and keeps it in this secret, which an app uses by name. Nobody sees it unless they reveal it with a passkey.`}
          onChange={(event) => setSecret(event.target.value)}
        />
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
