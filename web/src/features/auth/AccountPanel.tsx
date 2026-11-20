// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { AuthService, StepUpAction } from "@dockwell/api";
import { Button, Panel, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useEffect, useState } from "react";

import { authStateKey } from "../../app/api";
import { reasonOf } from "../../lib/errors";
import { formatDay, setDisplayTimeZone } from "../../lib/format";
import { useAuthError } from "./messages";
import { StepUpDeclined, withStepUpIfAsked } from "./stepUp";
import { TimeZonePicker } from "./TimeZonePicker";
import { useSignedIn } from "./useAuthState";
import { initials, useRoleName } from "./user";

/** As the control plane takes them: a name of 1 to 100 characters, an email of up to 254. */
const MAX_NAME = 100;
const MAX_EMAIL = 254;

/** The shape the control plane checks, so a slip reads at the field before Save. */
export function looksLikeEmail(email: string): boolean {
  const at = email.lastIndexOf("@");
  return at > 0 && at < email.length - 1 && !/[\s<>"]/.test(email) && email.length <= MAX_EMAIL;
}

/** A name as the control plane keeps it: its words, one space apart. */
function tidyName(name: string): string {
  return name.split(/\s+/).filter(Boolean).join(" ");
}

/**
 * Your account, first on Profile and security, as the Profile mock has it: who you
 * are here, your role and since when, your name and email to change, and the way back
 * in when every other is lost (#801). A new email takes your passkey each time, as it
 * names you for code sign-in and for dockwell recover.
 */
export function AccountPanel() {
  const { t, i18n } = useLingui();
  const auth = useSignedIn();
  const user = auth?.user;
  const role = useRoleName(user?.role ?? 0);
  const transport = useTransport();
  const client = useQueryClient();
  const explain = useAuthError();
  // Their own way back in, as the server says it: with sudo and --data-dir as Dockwell runs
  // there, and --user unless they're the owner (#801's review, M2).
  const recover = auth?.recoverCommand || "dockwell recover";
  const since = user?.createTime ? formatDay(timestampDate(user.createTime), i18n.locale) : "";
  const meta = [role, since ? t`member since ${since}` : ""].filter(Boolean).join(" · ");

  const [name, setName] = useState(user?.name ?? "");
  const [email, setEmail] = useState(user?.email ?? "");
  const [timeZone, setTimeZone] = useState(user?.timeZone ?? "");
  const [left, setLeft] = useState({ name: false, email: false });
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [emailProblem, setEmailProblem] = useState<string>();
  const [nameProblem, setNameProblem] = useState<string>();
  const [timeZoneProblem, setTimeZoneProblem] = useState<string>();
  const [problem, setProblem] = useState<string>();
  // The fields start from what the control plane says, once it has said it.
  useEffect(() => {
    setName(user?.name ?? "");
    setEmail(user?.email ?? "");
    setTimeZone(user?.timeZone ?? "");
  }, [user?.name, user?.email, user?.timeZone]);

  const newName = tidyName(name);
  // As typed: the server writes it as it keeps emails, as a browser's lowercase of İ or Σ
  // isn't Go's (#801's review, L1). Lowercase only tells whether it changed.
  const newEmail = email.trim();
  const newTimeZone = timeZone.trim();
  const nameChanged = user !== undefined && newName !== user.name;
  const emailChanged = user !== undefined && newEmail.toLowerCase() !== user.email;
  const timeZoneChanged = user !== undefined && newTimeZone !== (user.timeZone ?? "");
  const nameError =
    nameProblem ??
    (left.name && newName === ""
      ? t`Your name is empty, so it can't be saved. Type the name people here know you by.`
      : undefined);
  const emailError =
    emailProblem ??
    (left.email && !looksLikeEmail(newEmail)
      ? t`That isn't an email address, so it can't be saved. Type one such as mira@example.com.`
      : undefined);
  const ready = (nameChanged || emailChanged || timeZoneChanged) && newName !== "" && looksLikeEmail(newEmail);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready) {
      setLeft({ name: true, email: true });
      return;
    }
    setSaving(true);
    setSaved(false);
    setProblem(undefined);
    setEmailProblem(undefined);
    setNameProblem(undefined);
    setTimeZoneProblem(undefined);
    const update = (proof: string) =>
      createClient(AuthService, transport).updateProfile({
        name: nameChanged ? newName : undefined,
        email: emailChanged ? newEmail : undefined,
        timeZone: timeZoneChanged ? newTimeZone : undefined,
        proof,
      });
    try {
      const res = emailChanged
        ? await withStepUpIfAsked(transport, StepUpAction.CHANGE_EMAIL, newEmail, update)
        : await update("");
      if (timeZoneChanged) setDisplayTimeZone(res.user?.timeZone ?? newTimeZone);
      await client.invalidateQueries({ queryKey: authStateKey });
      setSaved(true);
    } catch (err) {
      if (err instanceof StepUpDeclined) return;
      switch (reasonOf(err)) {
        case "member_exists":
          setEmailProblem(t`Someone else here signs in with this email, so it can't be yours too. Choose another.`);
          break;
        case "email_lookalike":
          setEmailProblem(
            t`This email reads the same as one someone else here signs in with or is invited as, so it would pass for theirs. Choose another.`,
          );
          break;
        case "name_taken":
          setNameProblem(
            t`Someone else here goes by a name that reads the same, so the two of you would be hard to tell apart. Choose another.`,
          );
          break;
        case "email_invited":
          setEmailProblem(
            t`An open invite is for this email, so it can't be yours yet. Choose another, or ask an admin to revoke the invite in Members.`,
          );
          break;
        case "invalid":
          if (timeZoneChanged && !nameChanged && !emailChanged) {
            setTimeZoneProblem(t`That isn't a recognised time zone. Choose one from the list.`);
          } else {
            setProblem(explain(err));
          }
          break;
        default:
          setProblem(explain(err));
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <Panel title={t`Account`} className="profile-panel">
      <div className="profile-account" aria-busy={user === undefined}>
        <div className="profile-account-who">
          <span className="profile-avatar" aria-hidden="true">
            {initials(user?.name ?? "")}
          </span>
          <span className="dw-cell">
            <span className="profile-account-name">{user?.name}</span>
            {meta ? <span className="settings-meta">{meta}</span> : null}
          </span>
        </div>
        <form className="profile-account-form" onSubmit={(e) => void save(e)} noValidate>
          <TextField
            label={t`Name`}
            value={name}
            maxLength={MAX_NAME}
            autoComplete="name"
            disabled={user === undefined}
            error={nameError}
            onChange={(e) => {
              setName(e.target.value);
              setNameProblem(undefined);
              setSaved(false);
            }}
            onBlur={() => setLeft((l) => ({ ...l, name: true }))}
          />
          <TextField
            label={t`Email`}
            type="email"
            mono
            value={email}
            maxLength={MAX_EMAIL}
            autoComplete="email"
            spellCheck={false}
            disabled={user === undefined}
            hint={emailError ? undefined : t`Your passkey confirms a new one.`}
            error={emailError}
            onChange={(e) => {
              setEmail(e.target.value);
              setEmailProblem(undefined);
              setSaved(false);
            }}
            onBlur={() => setLeft((l) => ({ ...l, email: true }))}
          />
          <TimeZonePicker
            value={timeZone}
            disabled={user === undefined}
            error={timeZoneProblem}
            onChange={(next) => {
              setTimeZone(next);
              setTimeZoneProblem(undefined);
              setSaved(false);
            }}
          />
          {problem ? (
            <p className="profile-error" role="alert">
              {problem}
            </p>
          ) : null}
          <div className="profile-account-save">
            <Button type="submit" size="sm" icon="check" loading={saving} disabled={!ready || saving}>
              {t`Save`}
            </Button>
            <span className="settings-meta" role="status">
              {saved ? t`Saved` : ""}
            </span>
          </div>
        </form>
        <dl className="settings-facts">
          <dt>{t`Last resort`}</dt>
          <dd>
            <Trans>
              <code>{recover}</code> on the control server
            </Trans>
          </dd>
        </dl>
      </div>
    </Panel>
  );
}
