// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type CheckRegistryLoginResponse,
  Connection,
  ImageService,
  type RegistryLoginInfo,
  RegistryLoginState,
} from "@dockwell/api";
import { Advanced, Button, Callout, SelectField, Sheet, StatusBadge, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAllResources } from "../projects/useProjects";
import { useFleetView } from "../servers/useFleet";
import {
  ecrRegion,
  freeLoginName,
  hostOf,
  loginBase,
  REGISTRY_HOSTS,
  REGISTRY_NAMES,
  REGISTRY_ORDER,
  type RegistryKind,
  registryKind,
} from "./registries";
import "./Registries.css";

/** How long typing pauses before a login is checked. */
const CHECK_AFTER_MS = 600;

const ECR_REGIONS = [
  "us-east-1",
  "us-east-2",
  "us-west-1",
  "us-west-2",
  "ca-central-1",
  "eu-west-1",
  "eu-west-2",
  "eu-west-3",
  "eu-central-1",
  "eu-north-1",
  "ap-south-1",
  "ap-southeast-1",
  "ap-southeast-2",
  "ap-northeast-1",
  "sa-east-1",
];

/**
 * Adds a registry login, or changes one, as a plan (ADR 0086). A person picks the
 * registry, types who signs in and a token, and the login is checked from a server
 * as they type, saying Signed in before anything is planned; the plan saves the token
 * service the check found, the one place beside the registry the login ever goes.
 * Its name, and which projects may pull with it, are under Advanced: every project,
 * unless a person narrows it.
 */
export function RegistryLoginSheet({
  onClose,
  existing = [],
  editing,
  host: preset,
}: {
  onClose: () => void;
  /** The logins there are, so a new one's name isn't taken. */
  existing?: readonly RegistryLoginInfo[];
  /** The login to change, if any. */
  editing?: RegistryLoginInfo | undefined;
  /** The registry to start from, as where an image is typed names it. */
  host?: string | undefined;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(ImageService, transport), [transport]);
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayCheck = may(ImageService.method.checkRegistryLogin);
  const fleet = useFleetView();
  const resources = useAllResources();
  const projects = (resources.data ?? [])
    .filter((r) => r.resource?.kind === "project")
    .map((r) => ({ id: r.id, label: r.displayName || r.resource?.name || r.id }));

  const startHost = editing?.registry ?? preset ?? "docker.io";
  const [kind, setKind] = useState<RegistryKind>(registryKind(startHost));
  const [typedHost, setTypedHost] = useState(registryKind(startHost) === "other" ? startHost : "");
  const [region, setRegion] = useState(ecrRegion(startHost) || "us-east-1");
  const [username, setUsername] = useState(editing?.username ?? "");
  const [password, setPassword] = useState("");
  const [accessKey, setAccessKey] = useState("");
  const [secretKey, setSecretKey] = useState("");
  const [typedName, setTypedName] = useState<string>();
  const [some, setSome] = useState((editing?.projects.length ?? 0) > 0);
  const [picked, setPicked] = useState<string[]>(editing?.projects ?? []);
  const [server, setServer] = useState("");
  const [answer, setAnswer] = useState<CheckRegistryLoginResponse>();
  const [checking, setChecking] = useState(false);
  const [failure, setFailure] = useState<string>();
  // Another registry's host counts once it's typed whole: as the field is left, or
  // on Enter. Nothing is asked of a host half typed.
  const [committedHost, setCommittedHost] = useState(registryKind(startHost) === "other" ? startHost : "");
  // What the registry said with no login, before a token goes there.
  const [probe, setProbe] = useState<{ host: string; said?: CheckRegistryLoginResponse; failure?: string }>();
  // Where the token was typed for: once the registry changes, it's checked only when asked.
  const [typedFor, setTypedFor] = useState<string>();
  const [asks, setAsks] = useState(0);

  const ecr = kind === "ecr";
  const host = ecr ? (answer?.registry ?? editing?.registry ?? "") : (REGISTRY_HOSTS[kind] ?? committedHost);
  const name = editing?.name ?? typedName ?? freeLoginName(loginBase(kind, host || region), existing);
  const servers = fleet.servers ?? [];
  const from = server || servers.find((s) => s.connection === Connection.CONNECTED)?.id || servers[0]?.id || "";
  // What signs in, as typed now; an edit that types none keeps what's saved.
  const typedSignIn = ecr ? accessKey !== "" && secretKey !== "" : username !== "" && password !== "";
  const changedSignIn = editing
    ? password !== "" || accessKey !== "" || secretKey !== "" || username !== (editing.username ?? "")
    : true;
  const place = `${kind}|${host}|${region}`;
  const typedSecret = password !== "" || accessKey !== "" || secretKey !== "";
  // A token typed for one registry isn't sent to another unasked.
  const moved = typedSecret && typedFor !== undefined && typedFor !== place;
  // ECR's key goes to AWS from the control plane, which names the registry; any other
  // registry is asked first with no login, and is one.
  const probed =
    ecr ||
    (probe?.host === host &&
      (probe.said?.state === RegistryLoginState.ASKS || probe.said?.state === RegistryLoginState.SIGNED_IN));
  const checkable =
    mayCheck.allowed &&
    (editing ? changedSignIn && (ecr || username !== "") : typedSignIn) &&
    (ecr || host !== "") &&
    probed &&
    !moved;
  const signedIn = answer?.state === RegistryLoginState.SIGNED_IN;
  const ready = name !== "" && (editing && !changedSignIn ? true : signedIn) && (!some || picked.length > 0);

  // A host is asked with no login once it's chosen or typed whole.
  // biome-ignore lint/correctness/useExhaustiveDependencies: it asks again when the host or the server changes
  useEffect(() => {
    if (ecr || host === "" || !mayCheck.allowed) {
      setProbe(undefined);
      return;
    }
    let live = true;
    setProbe({ host });
    client
      .checkRegistryLogin({ registry: host, probe: true, server: from })
      .then((said) => live && setProbe({ host, said }))
      .catch((err: unknown) => live && setProbe({ host, failure: sentence(err) }));
    return () => {
      live = false;
    };
  }, [host, from, ecr]);

  // Each change to what signs in moves this on, so an answer to what changed since is dropped.
  const asked = useRef(0);
  const what = JSON.stringify([kind, host, region, username, password, accessKey, secretKey, from, asks]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it checks again whenever what signs in changes, which `what` holds
  useEffect(() => {
    const mine = ++asked.current;
    setAnswer(undefined);
    setFailure(undefined);
    if (!checkable) {
      setChecking(false);
      return;
    }
    setChecking(true);
    const timer = setTimeout(async () => {
      try {
        const said = await client.checkRegistryLogin({
          name: editing?.name ?? "",
          registry: ecr ? "" : host,
          username: ecr ? "" : username,
          password,
          ecr: ecr && (accessKey || secretKey) ? { accessKey, secretKey, region } : undefined,
          server: from,
        });
        if (mine === asked.current) setAnswer(said);
      } catch (err) {
        if (mine === asked.current) setFailure(sentence(err));
      } finally {
        if (mine === asked.current) setChecking(false);
      }
    }, CHECK_AFTER_MS);
    return () => clearTimeout(timer);
  }, [what, checkable]);

  const spec = () => ({
    registry: host,
    username: ecr ? "" : username,
    password,
    ecr: ecr ? { accessKey, secretKey, region } : undefined,
    tokenHost: answer ? answer.tokenHost : (editing?.tokenHost ?? ""),
    projects: some ? picked : [],
  });
  const review = () =>
    sheet.open({
      kind: "propose",
      summary: editing ? t`Change the registry login ${name}` : t`Pull private images from ${host}`,
      edits: [{ resource: { kind: "registry.login", name }, spec: { kind: { case: "registryLogin", value: spec() } } }],
      onClose: (change) => {
        if (change) onClose();
      },
    });
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the registry login ${name}`,
      edits: [{ resource: { kind: "registry.login", name }, delete: true }],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  const tokenHint: Partial<Record<RegistryKind, string>> = {
    hub: t`A personal access token that can read.`,
    ghcr: t`A token with read:packages.`,
    gitlab: t`A deploy token with read_registry.`,
  };
  const commitHost = (typed: string) => setCommittedHost(hostOf(typed));
  const checkWith = host || REGISTRY_NAMES[kind] || t`this registry`;
  const typed = (set: (v: string) => void) => (v: string) => {
    set(v);
    setTypedFor(place);
  };
  const stateWords: Partial<Record<RegistryLoginState, string>> = {
    [RegistryLoginState.SIGNED_IN]: t({ message: "Signed in", context: "a registry login, checked" }),
    [RegistryLoginState.REFUSED]: t`Wrong username or password`,
    [RegistryLoginState.UNREACHABLE]: t`Can't reach the registry`,
    [RegistryLoginState.MOVED]: t`Its token service moved`,
  };
  const who = some ? plural(picked.length, { one: "# project", other: "# projects" }) : t`every project`;
  const summary = [t`Named ${name}`, who].join(" · ");

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`Registry login ${editing.name}` : t`Add a registry login`}
      footer={
        <>
          {editing ? (
            <Button variant="ghost" className="registry-remove" onClick={remove} {...mayPlan.off}>
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
      <div className="registry-sheet">
        <SelectField
          label={t`Registry`}
          value={kind}
          disabled={editing !== undefined}
          options={REGISTRY_ORDER.map((k) => ({ value: k, label: REGISTRY_NAMES[k] ?? t`Another registry` }))}
          onChange={(event) => setKind(event.target.value as RegistryKind)}
        />
        {kind === "other" ? (
          <TextField
            label={t`Host`}
            mono
            autoComplete="off"
            spellCheck={false}
            disabled={editing !== undefined}
            value={editing ? editing.registry : typedHost}
            placeholder="registry.example.com"
            hint={t`Checked once you leave the field.`}
            onChange={(event) => setTypedHost(event.target.value)}
            onBlur={(event) => commitHost(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault();
                commitHost(event.currentTarget.value);
              }
            }}
          />
        ) : null}
        {ecr ? (
          <>
            <SelectField
              label={t`Region`}
              value={region}
              disabled={editing !== undefined}
              options={ECR_REGIONS.map((r) => ({ value: r, label: r }))}
              onChange={(event) => setRegion(event.target.value)}
            />
            <div className="registry-sheet-row">
              <TextField
                label={t`Access key`}
                mono
                autoComplete="off"
                spellCheck={false}
                value={accessKey}
                placeholder={editing ? t`Kept unless you type one` : undefined}
                onChange={(event) => typed(setAccessKey)(event.target.value.trim())}
              />
              <TextField
                label={t`Secret key`}
                mono
                type="password"
                autoComplete="off"
                value={secretKey}
                placeholder={editing ? t`Kept unless you type one` : undefined}
                onChange={(event) => typed(setSecretKey)(event.target.value.trim())}
              />
            </div>
          </>
        ) : (
          <div className="registry-sheet-row">
            <TextField
              label={t`Username`}
              mono
              autoComplete="off"
              spellCheck={false}
              value={username}
              onChange={(event) => setUsername(event.target.value.trim())}
            />
            <TextField
              label={t`Token`}
              mono
              type="password"
              autoComplete="off"
              value={password}
              placeholder={editing ? t`Kept unless you type one` : undefined}
              hint={tokenHint[kind]}
              onChange={(event) => typed(setPassword)(event.target.value.trim())}
            />
          </div>
        )}
        <div className="registry-sheet-check" aria-live="polite">
          {probe?.host === host && !ecr && !probe.said && !probe.failure ? (
            <StatusBadge status="deploying" label={t`Checking`} labelIsState />
          ) : probe?.host === host && !ecr && !probed ? (
            <>
              <StatusBadge status="unknown" label={t`Not a registry, or can't reach it`} labelIsState />
              {probe.failure || probe.said?.detail ? (
                <p className="registry-sheet-said">{probe.failure || probe.said?.detail}</p>
              ) : null}
            </>
          ) : moved && probed && !checking && !answer ? (
            <Button
              size="sm"
              variant="secondary"
              icon="check"
              onClick={() => {
                setTypedFor(place);
                setAsks((n) => n + 1);
              }}
              {...mayCheck.off}
            >
              {t`Check with ${checkWith}`}
            </Button>
          ) : checking ? (
            <StatusBadge status="deploying" label={t`Checking`} labelIsState />
          ) : answer ? (
            <StatusBadge
              status={signedIn ? "healthy" : answer.state === RegistryLoginState.UNREACHABLE ? "unknown" : "down"}
              label={
                signedIn && ecr && answer.registry
                  ? t`Signed in to ${answer.registry}`
                  : (stateWords[answer.state] ?? t`Can't reach the registry`)
              }
              labelIsState
            />
          ) : null}
          {answer && !signedIn && answer.detail ? <p className="registry-sheet-said">{answer.detail}</p> : null}
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
              hint={t`What an app names it by, in Compose and the CLI.`}
              onChange={(event) => setTypedName(event.target.value)}
            />
          )}
          <Switch
            checked={some}
            onChange={setSome}
            label={t`Only some projects`}
            description={t`Their apps alone may pull with it.`}
          />
          {some ? (
            <fieldset className="registry-projects">
              <legend className="dw-sr">{t`Projects`}</legend>
              {projects.map((p) => (
                <label key={p.id} className="registry-project">
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
          {servers.length > 1 ? (
            <SelectField
              label={t`Checked from`}
              value={from}
              options={servers.map((s) => ({ value: s.id, label: s.name }))}
              onChange={(event) => setServer(event.target.value)}
            />
          ) : null}
        </Advanced>
      </div>
    </Sheet>
  );
}
