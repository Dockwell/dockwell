// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type CheckSigninProviderResponse,
  DomainService,
  type SigninProviderInfo,
  SigninProviderState,
} from "@dockwell/api";
import { Advanced, Button, Callout, Sheet, StatusBadge, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAllResources } from "../projects/useProjects";
import { freeProviderName, issuerHost, scopesOf } from "./providers";
import "./Signin.css";

/** How long typing pauses before an issuer is read. */
const CHECK_AFTER_MS = 600;

/**
 * Adds a sign-in provider, or changes one, as a plan (ADR 0087). A person types the
 * issuer, which the control server reads as they type, saying what it found before
 * anything is planned, and the client Dockwell is at the provider. What the panel
 * calls it, its name, its scopes and claim, and which projects may use it start at
 * defaults under Advanced.
 */
export function SigninProviderSheet({
  onClose,
  existing = [],
  editing,
}: {
  onClose: () => void;
  /** The providers there are, so a new one's name isn't taken. */
  existing?: readonly SigninProviderInfo[];
  /** The provider to change, if any. */
  editing?: SigninProviderInfo | undefined;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(DomainService, transport), [transport]);
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayCheck = may(DomainService.method.checkSigninProvider);
  const resources = useAllResources();
  const projects = (resources.data ?? [])
    .filter((r) => r.resource?.kind === "project")
    .map((r) => ({ id: r.id, label: r.displayName || r.resource?.name || r.id }));

  const [issuer, setIssuer] = useState(editing?.issuer ?? "");
  const [clientId, setClientId] = useState(editing?.clientId ?? "");
  const [secret, setSecret] = useState("");
  const [typedName, setTypedName] = useState<string>();
  const [typedDisplay, setTypedDisplay] = useState<string>();
  const [scopes, setScopes] = useState((editing?.scopes ?? []).join(" "));
  const [claim, setClaim] = useState(editing?.groupsClaim ?? "");
  const [anyAccount, setAnyAccount] = useState(editing?.anyAccount ?? false);
  const [some, setSome] = useState((editing?.projects.length ?? 0) > 0);
  const [picked, setPicked] = useState<string[]>(editing?.projects ?? []);
  const [answer, setAnswer] = useState<CheckSigninProviderResponse>();
  const [checking, setChecking] = useState(false);
  const [failure, setFailure] = useState<string>();

  const host = answer?.host || issuerHost(issuer);
  const name = editing?.name ?? typedName ?? freeProviderName(host, existing);
  const display = typedDisplay ?? editing?.displayName ?? host;
  const found = answer?.state === SigninProviderState.FOUND;
  const unchanged = editing !== undefined && issuer.trim() === editing.issuer;
  const open = anyAccount && !answer?.public;
  const ready =
    name !== "" && clientId.trim() !== "" && (found || (unchanged && !checking)) && (!some || picked.length > 0);

  // Each change to the issuer moves this on, so an answer about what was typed before is dropped.
  const asked = useRef(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it reads again whenever the issuer changes
  useEffect(() => {
    const mine = ++asked.current;
    setAnswer(undefined);
    setFailure(undefined);
    if (issuerHost(issuer) === "" || !mayCheck.allowed) {
      setChecking(false);
      return;
    }
    setChecking(true);
    const timer = setTimeout(async () => {
      try {
        const said = await client.checkSigninProvider({ issuer: issuer.trim() });
        if (mine === asked.current) setAnswer(said);
      } catch (err) {
        if (mine === asked.current) setFailure(sentence(err));
      } finally {
        if (mine === asked.current) setChecking(false);
      }
    }, CHECK_AFTER_MS);
    return () => clearTimeout(timer);
  }, [issuer]);

  const spec = () => ({
    displayName: display.trim(),
    issuer: answer?.issuer || editing?.issuer || issuer.trim(),
    clientId: clientId.trim(),
    clientSecret: secret,
    scopes: scopesOf(scopes),
    groupsClaim: claim.trim(),
    anyAccount: open,
    projects: some ? picked : [],
  });
  const review = () =>
    sheet.open({
      kind: "propose",
      summary: editing ? t`Change the sign-in provider ${name}` : t`Sign in to private routes with ${display}`,
      edits: [
        { resource: { kind: "signin.provider", name }, spec: { kind: { case: "signinProvider", value: spec() } } },
      ],
      onClose: (change) => {
        if (change) onClose();
      },
    });
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the sign-in provider ${name}`,
      edits: [{ resource: { kind: "signin.provider", name }, delete: true }],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  const strongest = answer?.algorithms[0] ?? "";
  const answerHost = answer?.host ?? "";
  const shown = display || name;
  const editingName = editing ? editing.displayName || editing.name : "";
  const who = some ? plural(picked.length, { one: "# project", other: "# projects" }) : t`every project`;
  const summary = [t`Named ${name}`, t`Shown as ${shown}`, who].join(" · ");

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`Sign-in provider ${editingName}` : t`Add a sign-in provider`}
      footer={
        <>
          {editing ? (
            <Button variant="ghost" className="signin-remove" onClick={remove} {...mayPlan.off}>
              <Trans>Remove</Trans>
            </Button>
          ) : null}
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!ready} onClick={review} {...mayPlan.off}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="signin-sheet">
        <TextField
          label={t`Issuer`}
          mono
          autoComplete="off"
          spellCheck={false}
          value={issuer}
          placeholder="auth.example.com"
          onChange={(event) => setIssuer(event.target.value)}
        />
        <div className="signin-sheet-check" aria-live="polite">
          {checking ? (
            <StatusBadge status="deploying" label={t`Checking`} labelIsState />
          ) : found ? (
            <StatusBadge status="healthy" label={t`Found ${answerHost} · signs with ${strongest}`} labelIsState />
          ) : answer ? (
            <>
              <StatusBadge
                status={answer.state === SigninProviderState.UNREACHABLE ? "unknown" : "down"}
                label={
                  answer.state === SigninProviderState.UNREACHABLE
                    ? t`Can't read ${answerHost}`
                    : t`Can't sign in with ${answerHost}`
                }
                labelIsState
              />
              {answer.detail ? <p className="signin-sheet-said">{answer.detail}</p> : null}
            </>
          ) : null}
        </div>
        {failure ? (
          <Callout tone="critical" title={t`Not checked`}>
            {failure}
          </Callout>
        ) : null}
        <div className="signin-sheet-row">
          <TextField
            label={t`Client ID`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={clientId}
            onChange={(event) => setClientId(event.target.value)}
          />
          <TextField
            label={t`Client secret`}
            mono
            type="password"
            autoComplete="off"
            value={secret}
            placeholder={editing?.hasSecret ? t`Kept unless you type one` : t`None, for a public client`}
            onChange={(event) => setSecret(event.target.value.trim())}
          />
        </div>

        <Advanced summary={summary}>
          {editing ? null : (
            <TextField
              label={t`Name`}
              mono
              value={name}
              hint={t`What a route names it by, in the CLI.`}
              onChange={(event) => setTypedName(event.target.value)}
            />
          )}
          <TextField
            label={t`Display name`}
            value={display}
            onChange={(event) => setTypedDisplay(event.target.value)}
          />
          <div className="signin-sheet-row">
            <TextField
              label={t`More scopes`}
              mono
              autoComplete="off"
              spellCheck={false}
              value={scopes}
              placeholder="groups"
              onChange={(event) => setScopes(event.target.value)}
            />
            <TextField
              label={t`Groups claim`}
              mono
              autoComplete="off"
              spellCheck={false}
              value={claim}
              placeholder="groups"
              onChange={(event) => setClaim(event.target.value)}
            />
          </div>
          <Switch
            checked={open}
            onChange={setAnyAccount}
            disabled={answer?.public === true}
            label={t`Any account here is your team's`}
            description={
              answer?.public
                ? t`Anyone can have an account at ${answerHost}.`
                : t`A route may then let in anyone who signs in.`
            }
          />
          <Switch
            checked={some}
            onChange={setSome}
            label={t`Only some projects`}
            description={t`Their routes alone may use it.`}
          />
          {some ? (
            <fieldset className="signin-projects">
              <legend className="dw-sr">{t`Projects`}</legend>
              {projects.map((p) => (
                <label key={p.id} className="signin-project">
                  <input
                    type="checkbox"
                    checked={picked.includes(p.id)}
                    onChange={(event) =>
                      setPicked((was) => (event.target.checked ? [...was, p.id] : was.filter((x) => x !== p.id)))
                    }
                  />
                  <span>{p.label}</span>
                </label>
              ))}
            </fieldset>
          ) : null}
        </Advanced>
      </div>
    </Sheet>
  );
}
