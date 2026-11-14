// SPDX-License-Identifier: AGPL-3.0-only

import { DomainService, type RouteSignIn } from "@dockwell/api";
import { Button, CopyButton, SelectField, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";

import { useMay } from "../auth/may";
import { mailboxDomain, publicIssuer, useSigninProviders } from "../signin/providers";

/** Someone a route lets sign in: by email, by every address at a domain, or by group. */
export interface SignInPerson {
  kind: "email" | "domain" | "group";
  value: string;
}

/** How a route signs people in, as the sheet holds it. */
export interface SignInChoice {
  on: boolean;
  provider: string;
  people: SignInPerson[];
  /** Whether the user has added or taken anyone off yet, so yours is offered first. */
  touched: boolean;
}

/** A route's sign-in as the sheet starts it. */
export function signInChoiceOf(s: RouteSignIn | undefined): SignInChoice {
  if (!s) return { on: false, provider: "", people: [], touched: false };
  return {
    on: true,
    provider: s.provider,
    people: [
      ...s.emails.map((value) => ({ kind: "email" as const, value })),
      ...s.emailDomains.map((value) => ({ kind: "domain" as const, value })),
      ...s.groups.map((value) => ({ kind: "group" as const, value })),
    ],
    touched: true,
  };
}

/** What the sheet's edit says of sign-in, keeping how long a sign-in lasts and its generation. */
export function signInSpec(choice: SignInChoice, was: RouteSignIn | undefined) {
  const of = (kind: SignInPerson["kind"]) => choice.people.filter((p) => p.kind === kind).map((p) => p.value);
  return {
    provider: choice.provider,
    emails: of("email"),
    emailDomains: of("domain"),
    groups: of("group"),
    sessionHours: was?.sessionHours ?? 0,
    generation: was?.generation ?? 0n,
  };
}

/** Reads what's typed as an email, a domain, or else a group's name, in ASCII, as a route keeps them (ADR 0087). */
export function personOf(typed: string): SignInPerson | undefined {
  const value = typed.trim();
  // biome-ignore lint/suspicious/noControlCharactersInRegex: control characters are what this refuses
  if (value === "" || /[\u0000-\u001f\u007f]/.test(value)) return undefined;
  if (/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(value)) {
    return /^[\x20-\x7e]+$/.test(value) ? { kind: "email", value: value.toLowerCase() } : undefined;
  }
  const domain = value.replace(/^@/, "");
  if (/^[a-z0-9-]+(\.[a-z0-9-]+)+$/i.test(domain)) return { kind: "domain", value: domain.toLowerCase() };
  return { kind: "group", value };
}

/** Where the provider sends someone back to, which an admin registers there. */
export function redirectAddress(domain: string, path: string): string {
  return `https://${domain}${path.replace(/\/$/, "")}/.dockwell/signin/callback`;
}

/**
 * Sign in with a provider, as a part of Make it private (ADR 0087): a picker of the
 * providers there are, who may sign in, with your own address first, and the address
 * to register at the provider, with Copy.
 */
export function PrivateSignIn({
  choice,
  onChange,
  ownEmail,
  domain,
  path,
  plainHere,
}: {
  choice: SignInChoice;
  onChange: (next: SignInChoice) => void;
  ownEmail: string;
  domain: string;
  path: string;
  /** The route is reached over plain HTTP, where a sign-in's cookie can't be kept. */
  plainHere: boolean;
}) {
  const { t } = useLingui();
  const providers = useSigninProviders("");
  const listed = providers.data?.providers ?? [];
  const may = useMay();
  const admin = may(DomainService.method.checkSigninProvider).off === undefined;
  const [typed, setTyped] = useState("");
  // The only provider, or the first, is what a new sign-in starts with, once the list is in.
  const first = listed[0]?.name ?? "";
  useEffect(() => {
    if (choice.on && choice.provider === "" && first !== "") onChange({ ...choice, provider: first });
  }, [choice, first, onChange]);
  const [typedError, setTypedError] = useState<string>();
  // Anyone at your own domain is your team, unless anyone can have an address there, or
  // the provider takes anyone's account, where it would be anyone at all (#598's final check, L3).
  const issuer = listed.find((p) => p.name === choice.provider)?.issuer ?? "";
  const ownDomain =
    ownEmail.includes("@") && !publicIssuer(issuer) && !mailboxDomain(ownEmail.split("@")[1] ?? "")
      ? (ownEmail.split("@")[1] ?? "")
      : "";
  const has = (p: SignInPerson) => choice.people.some((q) => q.kind === p.kind && q.value === p.value);

  const add = (p: SignInPerson | undefined) => {
    if (!p) {
      setTypedError(t`Write an email, a domain or a group, in ASCII, such as mira@example.com or example.com.`);
      return;
    }
    onChange({ ...choice, touched: true, people: has(p) ? choice.people : [...choice.people, p] });
    setTyped("");
    setTypedError(undefined);
  };
  const kindWords = (p: SignInPerson) => {
    switch (p.kind) {
      case "email":
        return p.value === ownEmail.toLowerCase()
          ? t({ message: "you", context: "an email on who may sign in, that is yours" })
          : t({ message: "email", context: "a kind of who may sign in" });
      case "domain":
        return t({ message: "anyone at", context: "a domain on who may sign in" });
    }
    return t({ message: "group", context: "a kind of who may sign in" });
  };

  return (
    <fieldset className="private-part">
      <label className="app-check private-toggle">
        <input
          type="checkbox"
          checked={choice.on && !plainHere}
          disabled={plainHere}
          onChange={(event) => {
            const on = event.target.checked;
            // Your own address first, on a sign-in you haven't touched.
            const people =
              on && !choice.touched && choice.people.length === 0 && ownEmail
                ? [{ kind: "email" as const, value: ownEmail.toLowerCase() }]
                : choice.people;
            onChange({ ...choice, on, people, provider: choice.provider || listed[0]?.name || "" });
          }}
        />
        <span>
          <Trans>Sign in with a provider</Trans>
        </span>
      </label>
      {plainHere ? (
        // The password part beside it offers HTTPS, once for the sheet.
        <p className="private-why">{t`Visitors reach it over plain HTTP, where a sign-in can't be kept.`}</p>
      ) : null}
      {choice.on && !plainHere ? (
        <>
          {providers.isSuccess && listed.length === 0 ? (
            admin ? (
              <p className="private-why">
                <Link to="/settings/integrations" className="app-panel-link">
                  {t`Add a sign-in provider`}
                </Link>
              </p>
            ) : (
              <p className="private-why">{t`No sign-in provider is here yet. An admin adds one in Settings.`}</p>
            )
          ) : (
            <SelectField
              label={t`Sign in with`}
              value={choice.provider}
              options={listed.map((p) => ({ value: p.name, label: p.displayName || p.name }))}
              onChange={(event) => onChange({ ...choice, provider: event.target.value })}
            />
          )}
          <ul className="private-entries" aria-label={t`Who may sign in`}>
            {choice.people.map((p) => {
              const who = p.value;
              return (
                <li key={`${p.kind}:${p.value}`} className="private-entry">
                  <span className="private-entry-what">
                    <code>{p.value}</code>
                    <span className="private-entry-note">{kindWords(p)}</span>
                  </span>
                  <Button
                    variant="ghost"
                    size="sm"
                    icon="x"
                    aria-label={t`Take ${who} off`}
                    onClick={() => onChange({ ...choice, touched: true, people: choice.people.filter((q) => q !== p) })}
                  />
                </li>
              );
            })}
          </ul>
          {ownDomain && !has({ kind: "domain", value: ownDomain }) ? (
            <Button variant="ghost" size="sm" icon="plus" onClick={() => add({ kind: "domain", value: ownDomain })}>
              {t`Add anyone at ${ownDomain}`}
            </Button>
          ) : null}
          {choice.people.length === 0 ? <p className="private-why">{t`Add someone who may sign in.`}</p> : null}
          <div className="private-add">
            <TextField
              label={t`Add an email, a domain or a group`}
              mono
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              value={typed}
              error={typedError}
              placeholder="mira@example.com"
              onChange={(event) => {
                setTyped(event.target.value);
                setTypedError(undefined);
              }}
              onKeyDown={(event) => {
                if (event.key === "Enter" && !event.metaKey && !event.ctrlKey) {
                  event.preventDefault();
                  add(personOf(typed));
                }
              }}
            />
            <Button variant="secondary" onClick={() => add(personOf(typed))} disabled={typed.trim() === ""}>
              <Trans>Add</Trans>
            </Button>
          </div>
          <div className="private-redirect">
            <span className="dw-field-label">{t`Redirect address at the provider`}</span>
            <code>{redirectAddress(domain, path)}</code>
            <CopyButton
              size="sm"
              text={redirectAddress(domain, path)}
              label={t`Copy the redirect address`}
              copiedLabel={t`Redirect address copied`}
            />
          </div>
        </>
      ) : null}
    </fieldset>
  );
}
