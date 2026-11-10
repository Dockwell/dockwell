// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogPart,
  CatalogService,
  type Change,
  ChangeState,
  type HostAccess,
  HostAccessKind,
  type Input,
  InputType,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  PHONE_QUERY,
  type PlanChange,
  PlanDiff,
  type PlanResource,
  type PortCheck,
  PortField,
  Sheet,
  STATUS,
  TextField,
  TONE,
  useMediaQuery,
  useUIStrings,
  validPortNumber,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type FormEvent, type ReactNode, useId, useMemo, useState } from "react";

import { methodCommand } from "../../lib/cli";
import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { portRefusal, SERVICE_NAME, tidyMessage } from "../apps/app";
import { RunsOnField } from "../apps/NewAppSheet";
import { useMay } from "../auth/may";
import { useSignedInUser } from "../auth/useAuthState";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { asGoodAsRoot } from "../changes/plan";
import { type PortHolder, useServerPorts } from "../servers/ports";
import { useFleetView } from "../servers/useFleet";
import { CopyAsCommand } from "../system/CopyCommand";
import { Facts, useEntryFacts } from "./access";
// The form is New app's, so its fields sit apart the same way, however the sheet is reached.
import "../apps/Apps.css";

/** A label as it reads inside a sentence: "Adminer port" is "adminer port", while "API key" keeps its capitals. */
const inSentence = (label: string) =>
  /^[A-Z][a-z]*(\s|$)/.test(label) ? label.charAt(0).toLowerCase() + label.slice(1) : label;

/** Whether a person gives an input: Dockwell makes one that generates. */
const asked = (input: Input) => input.generate === 0;

/**
 * Install from the catalog (Catalog-Install), defaults first: the newest version,
 * a name that's free, the server, a free port for each it publishes and the
 * folders it reads, each a line with Change, and what only a person has as a
 * field. The rest, what it generates, what it installs and the CLI command, is
 * under Advanced. Nothing runs until the plan applies, and a password no one
 * types is made and sealed by the control plane, so it's never here. Once it
 * applies, it leads to where what it made is used: a database's page, with how
 * to connect, or the app's.
 */
export function InstallSheet({ id, onClose }: { id: string; onClose: () => void }) {
  const { t } = useLingui();
  // Installing makes a project of its own, which is an admin's (ADR 0049): off, with why, for anyone else.
  const mayInstall = useMay()(CatalogService.method.installCatalogEntry);
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const navigate = useNavigate();
  const { fleet, servers, only } = useFleetView();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const got = useQuery(CatalogService.method.getCatalogEntry, { id });
  const entry = got.data?.entry;
  const email = useSignedInUser()?.email ?? "";
  const inputs = (entry?.inputs ?? []).filter(asked);
  const generated = (entry?.inputs ?? []).filter((input) => !asked(input));

  // Kept apart from what the control plane suggests, so a value typed stays when it reads again.
  const [typed, setTyped] = useState<Readonly<Record<string, string>>>({});
  const [typedName, setTypedName] = useState<string>();
  const [chosenServers, setChosenServers] = useState<readonly string[]>([]);
  // The defaults someone pressed Change on, which show as their fields from then on.
  const [changing, setChanging] = useState<ReadonlySet<string>>(new Set());
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [nameError, setNameError] = useState<string>();
  const [serversError, setServersError] = useState<string>();
  // The plan under review takes this sheet over, and Cancel brings the form back as it was.
  const [plan, setPlan] = useState<Opening>();
  const change = (key: string) => setChanging((was) => new Set(was).add(key));

  // What's free is read on the servers it will run on: the one there is, or those chosen.
  const onlyId = only?.id;
  const serverIds = useMemo(
    () => (fleet ? [...chosenServers] : onlyId ? [onlyId] : []),
    [fleet, chosenServers, onlyId],
  );
  const ports = useServerPorts(serverIds);
  const holderName = (holder: PortHolder) => (holder.kind === "proxy" ? t`Dockwell's proxy` : holder.name);

  // An email it asks for, such as for an owner's account, starts as yours.
  const suggested = (input: Input) => got.data?.values[input.name] || (input.type === InputType.EMAIL ? email : "");
  const portInputs = inputs.filter((input) => input.type === InputType.PORT);
  // Each port nobody typed takes the first free one from what the control plane
  // suggests, past what the server holds and the ports above it, so no two take one.
  const freePorts: Record<string, string> = {};
  if (ports.ready) {
    const taken = portInputs.flatMap((input) => {
      const v = typed[input.name];
      return v !== undefined && validPortNumber(v) ? [Number(v)] : [];
    });
    for (const input of portInputs) {
      if (typed[input.name] !== undefined) continue;
      const from = Number(suggested(input) || input.defaultValue);
      const free = ports.free(validPortNumber(String(from)) ? from : undefined, false, taken);
      freePorts[input.name] = String(free);
      taken.push(free);
    }
  }
  const value = (input: Input) => typed[input.name] ?? freePorts[input.name] ?? suggested(input);
  // The ports the others take, which this one can't.
  const othersOf = (input: Input) =>
    portInputs
      .filter((other) => other.name !== input.name && validPortNumber(value(other)))
      .map((other) => Number(value(other)));
  const portCheck = (input: Input): PortCheck => ports.check(value(input), false, holderName, othersOf(input));

  // The folders on the server its parts read, by the input that names each, such as for media.
  const folders = new Set((got.data?.parts ?? []).flatMap((part) => Object.values(part.pathInputs)));
  // On top: a default with a line of its own, and what only a person has. Under Advanced: the rest.
  const onTop = (input: Input) =>
    input.type === InputType.PORT ||
    input.type === InputType.EMAIL ||
    folders.has(input.name) ||
    (!input.optional && suggested(input) === "");
  const top = inputs.filter(onTop);
  const more = inputs.filter((input) => !onTop(input));

  const name = (typedName ?? got.data?.project ?? "").trim();
  // What it says of itself, as state, beside its defaults; how it signs in is under Advanced.
  const facts = useEntryFacts(entry).filter((f) => f.key !== "sign-in");
  const nameOpen = changing.has("name") || nameError !== undefined;
  // A default shows as its field once someone changes it, and from the start when there's no default to show.
  const open = (input: Input) => changing.has(input.name) || suggested(input) === "";
  const missing = inputs.find((input) => !input.optional && value(input).trim() === "");
  const nameInvalid = name === "" || !SERVICE_NAME.test(name);
  const ready = entry !== undefined && !nameInvalid && !missing;
  const app = entry?.name ?? id;
  const webService = entry?.web?.service ?? "";
  const missingLabel = missing?.label ?? "";
  const license = entry?.appLicense ?? "";
  const values = Object.fromEntries(inputs.map((input) => [input.name, value(input).trim()]));

  // The same install from the CLI: what differs from what it would pick, with a typed secret from your environment.
  const secrets = new Set(inputs.filter((input) => input.type === InputType.SECRET).map((input) => input.name));
  const command = entry
    ? methodCommand(
        CatalogService.method.installCatalogEntry,
        {
          id,
          project: name !== got.data?.project ? name : "",
          values: Object.fromEntries(
            Object.entries(values).filter(([key, v]) => v !== "" && v !== (got.data?.values[key] ?? "")),
          ),
          servers: fleet ? [...chosenServers] : [],
        },
        (_path, key) => !secrets.has(key),
      )
    : "";
  const summary = [
    ...more.filter((input) => value(input).trim() !== "").map((input) => `${input.label} ${value(input).trim()}`),
    ...(generated.length > 0
      ? [plural(generated.length, { one: "# secret generated", other: "# secrets generated" })]
      : []),
    ...(got.data?.parts ?? []).map((part) => part.image),
  ].join(" · ");

  const review = (planned: Change | undefined) => {
    const service = (name: string) =>
      planned?.plan?.resources.find((r) => r.resource?.kind === "service" && r.resource.name === name);
    // A database lands on its own page, with how to connect, rather than on the tool beside it.
    const database = entry?.category === "databases" ? got.data?.parts.find((p) => p.database)?.service : undefined;
    const lands = database ? service(database) : undefined;
    const opens = service(webService);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change: planned }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      ...(lands || opens ? { doneLabel: t`Open ${app}` } : { doneLabel: t`Show the apps` }),
      onClose: (after) => {
        // Cancelled, or it failed and came back as it was: back to the form, as you left it.
        if (
          !after ||
          after.state === ChangeState.PLANNED ||
          after.state === ChangeState.DISCARDED ||
          after.state === ChangeState.FAILED
        ) {
          if (after?.state === ChangeState.FAILED) setFailure(tidyMessage(after.error));
          setPlan(undefined);
          return;
        }
        // Undone in the same sheet, it has no page to open.
        if (after.reverts !== "") void navigate({ to: "/apps" });
        else if (lands) void navigate({ to: "/databases/$serviceId", params: { serviceId: lands.id } });
        else if (opens) void navigate({ to: "/apps/$serviceId", params: { serviceId: opens.id } });
        else void navigate({ to: "/apps" });
      },
    });
  };

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || !ready) return;
    setFailure(undefined);
    setNameError(undefined);
    if (fleet && chosenServers.length === 0) {
      setServersError(t`Choose where it runs.`);
      return;
    }
    setBusy(true);
    try {
      const res = await client.installCatalogEntry({ id, project: name, values, servers: [...chosenServers] });
      review(res.change);
    } catch (err) {
      if (reasonOf(err) === "project_exists") {
        setNameError(sentence(err));
        change("name");
      } else setFailure(portRefusal(sentence(err)) ?? sentence(err));
    } finally {
      setBusy(false);
    }
  };

  const field = (input: Input) => {
    const port = input.type === InputType.PORT;
    const from = Number(value(input));
    return (
      <InputField
        key={input.name}
        input={input}
        value={value(input)}
        check={port ? portCheck(input) : undefined}
        free={port ? ports.free(validPortNumber(value(input)) ? from : undefined, false, othersOf(input)) : undefined}
        autoFocus={changing.has(input.name)}
        onChange={(next) => setTyped((was) => ({ ...was, [input.name]: next }))}
      />
    );
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      title={t`Install ${app}`}
      subtitle={entry?.summary}
      onClose={onClose}
      className={cx("app-sheet", "catalog-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name={entry?.hostAccess ? "shield" : "commit"} size={14} />
            {missing ? (
              <Trans>Give {missingLabel} a value to review the plan</Trans>
            ) : entry?.hostAccess ? (
              <Trans>You approve what it reaches in the plan</Trans>
            ) : (
              <Trans>Nothing runs until you apply the plan</Trans>
            )}
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            disabled={!ready}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
            {...mayInstall.off}
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
        {/* Lets Enter in a field do what the button in the sheet's footer does. */}
        <button type="submit" hidden />
        {got.error ? (
          <Callout tone="critical" title={t`Couldn't read this app`}>
            {sentence(got.error)}
          </Callout>
        ) : null}
        {failure ? (
          <Callout tone="critical" title={t`Can't make a plan`}>
            {failure}
          </Callout>
        ) : null}
        {entry ? (
          <>
            <div className="catalog-chosen-list">
              <Chosen label={t`Version`} value={entry.version} />
              {nameOpen ? (
                <TextField
                  label={t`Name`}
                  mono
                  autoFocus={changing.has("name") && !nameError}
                  value={typedName ?? got.data?.project ?? ""}
                  error={
                    nameError ??
                    (nameInvalid
                      ? t`Use lowercase letters, digits and dashes, starting with a letter, such as shop.`
                      : undefined)
                  }
                  onChange={(event) => {
                    setTypedName(event.target.value);
                    setNameError(undefined);
                  }}
                />
              ) : (
                <Chosen label={t`Name`} value={name} onChange={() => change("name")} />
              )}
              {fleet ? (
                <RunsOnField
                  value={chosenServers}
                  onChange={(next) => {
                    setChosenServers(next);
                    setServersError(undefined);
                  }}
                  error={serversError}
                  servers={servers ?? []}
                />
              ) : only ? (
                <Chosen label={t`Runs on`} value={only.name} />
              ) : null}
              {top.map((input) =>
                open(input) ? (
                  field(input)
                ) : (
                  <Chosen
                    key={input.name}
                    label={input.label}
                    value={value(input)}
                    state={input.type === InputType.PORT ? <PortState check={portCheck(input)} /> : undefined}
                    onChange={() => change(input.name)}
                  />
                ),
              )}
              {entry.hostAccess ? <Chosen label={t`Reaches`} value={entry.hostAccess} tone="warning" /> : null}
            </div>
            <Facts facts={facts} className="catalog-sheet-facts" />
            <Advanced open={advancedOpen} onOpenChange={setAdvancedOpen} summary={summary}>
              {more.map(field)}
              {generated.length > 0 ? (
                <div className="catalog-chosen-list">
                  {generated.map((input) => (
                    <Chosen
                      key={input.name}
                      label={input.label}
                      value={input.logged ? t`Generated, in ${app}'s log` : t`Generated and sealed`}
                      plain
                    />
                  ))}
                </div>
              ) : null}
              <Parts
                parts={got.data?.parts ?? []}
                typed={(name) => {
                  const input = inputs.find((i) => i.name === name);
                  return input ? value(input).trim() : "";
                }}
              />
              <p className="catalog-sheet-source">
                <Trans>
                  {app} is under the {license} licence.
                </Trans>{" "}
                <a href={entry.homepage} target="_blank" rel="noreferrer" className="catalog-link">
                  <Trans>Its site</Trans>
                </a>
              </p>
              {command ? <CopyAsCommand command={command} /> : null}
            </Advanced>
          </>
        ) : !got.error ? (
          <p className="catalog-note" aria-busy="true">
            <Trans>Reading this app</Trans>
          </p>
        ) : null}
      </form>
    </Sheet>
  );
}

/**
 * A default as a line: what it is, its value, a state when it has one (a port's
 * Free), and Change, which turns it into its field.
 */
function Chosen({
  label,
  value,
  state,
  onChange,
  tone,
  plain,
}: {
  label: string;
  value: string;
  state?: ReactNode;
  onChange?: () => void;
  tone?: "warning";
  /** The value is words, not something to copy, such as "Generated and sealed". */
  plain?: boolean;
}) {
  const { t } = useLingui();
  const id = useId();
  const what = inSentence(label);
  return (
    <fieldset className={cx("catalog-chosen", tone && `is-${tone}`)} aria-labelledby={id}>
      <span id={id} className="catalog-chosen-label">
        {label}
      </span>
      <span className="catalog-chosen-value">
        {tone === "warning" ? <Icon name="shield" size={14} className="catalog-chosen-icon" /> : null}
        {plain || tone ? <span>{value}</span> : <code>{value}</code>}
        {state}
        {onChange ? (
          <button type="button" className="catalog-change" aria-label={t`Change ${what}`} onClick={onChange}>
            <Trans>Change</Trans>
          </button>
        ) : null}
      </span>
    </fieldset>
  );
}

/** Whether a port is free, as the port field says it beside the number. */
function PortState({ check }: { check: PortCheck }) {
  const words = useUIStrings().portField;
  if (check.state === "unknown") return null;
  return (
    <span className={cx("catalog-chosen-state", `is-${check.state}`)}>
      <Glyph
        shape={
          check.state === "checking"
            ? STATUS.deploying.shape
            : check.state === "free"
              ? TONE.healthy.shape
              : TONE.critical.shape
        }
        size={9}
      />
      {check.state === "taken" && check.by ? words.takenBy(check.by) : words[check.state]}
    </span>
  );
}

/**
 * What an install runs, as the plan will list it (Catalog-Install): the app you open
 * first, then what it keeps its data in, each with its image by tag, its volumes, and
 * what it reaches on the server, in red for what is as good as root and in amber for
 * the rest, as the plan marks them. A server path a person gives shows as they typed
 * it. The plan says the rest, such as the project and its secrets, so this counts nothing.
 */
function Parts({ parts, typed }: { parts: readonly CatalogPart[]; typed: (input: string) => string }) {
  const { t } = useLingui();
  if (parts.length === 0) return null;
  const reach = (part: CatalogPart, access: HostAccess, i: number): PlanChange => {
    const input = part.pathInputs[access.path];
    const path = (input ? typed(input) : "") || access.path;
    const shown = access.readOnly ? (
      <Trans>
        <code>{path}</code>, read-only
      </Trans>
    ) : (
      path
    );
    const words: Partial<Record<HostAccessKind, [string, PlanChange["to"]]>> = {
      [HostAccessKind.DOCKER_SOCKET]: [t`Docker's socket`, shown],
      [HostAccessKind.HOST_NETWORK]: [t`server's network`, t`the server's own`],
      [HostAccessKind.SERVER_PATH]: [t`server path`, shown],
      [HostAccessKind.EXTERNAL_NETWORK]: [t`server's network`, access.path],
      [HostAccessKind.CAPABILITY]: [t`capability`, access.path],
      [HostAccessKind.GROUP]: [t`group`, access.path],
    };
    const [label, to] = words[access.kind] ?? [t`reaches`, access.path];
    return { path: `host_access[${i}]`, label, to, tone: asGoodAsRoot(access) ? "critical" : "warning" };
  };
  const resources: PlanResource[] = parts.map((part) => ({
    id: part.service,
    action: "create",
    kind: part.database ? "database" : "app",
    kindLabel: part.database ? t`Database` : part.opens ? t`App you open` : t`App`,
    name: part.service,
    changes: [
      { path: "image", label: t`image`, to: part.image },
      ...part.volumes.map((volume, i) => ({ path: `volumes[${i}]`, label: t`volume`, to: volume })),
      ...part.hostAccess.map((access, i) => reach(part, access, i)),
    ],
  }));
  return (
    <section className="catalog-parts" aria-labelledby="catalog-parts-title">
      <div className="catalog-parts-head">
        <h3 id="catalog-parts-title">
          <Trans>What it installs</Trans>
        </h3>
        <span>
          <Trans>Each image pinned by digest</Trans>
        </span>
      </div>
      <PlanDiff resources={resources} noSummary />
    </section>
  );
}

/**
 * A field for one input, as its type asks: a port with whether it's free, and
 * a free one to take when it isn't. A secret never shows as you type it, or
 * stays in a browser's memory.
 */
function InputField({
  input,
  value,
  check,
  free,
  autoFocus,
  onChange,
}: {
  input: Input;
  value: string;
  check: PortCheck | undefined;
  free: number | undefined;
  autoFocus: boolean;
  onChange: (next: string) => void;
}) {
  if (input.type === InputType.PORT) {
    return (
      <PortField
        label={input.label}
        hint={input.hint || undefined}
        autoFocus={autoFocus}
        value={value}
        check={check}
        suggestion={check?.state === "taken" ? free : undefined}
        onChange={onChange}
      />
    );
  }
  const secret = input.type === InputType.SECRET;
  return (
    <TextField
      label={input.label}
      hint={input.hint || undefined}
      optional={input.optional}
      mono={secret || input.type === InputType.TEXT}
      type={secret ? "password" : input.type === InputType.EMAIL ? "email" : "text"}
      autoComplete="off"
      autoFocus={autoFocus}
      value={value}
      onChange={(event) => onChange(event.target.value)}
    />
  );
}
