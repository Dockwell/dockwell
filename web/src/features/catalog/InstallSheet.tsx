// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogPart,
  CatalogService,
  type Change,
  ChangeState,
  type Entry,
  type HostAccess,
  HostAccessKind,
  type Input,
  InputType,
  RestartPolicy,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  CopyButton,
  cx,
  Glyph,
  Icon,
  PHONE_QUERY,
  type PlanChange,
  PlanDiff,
  type PlanResource,
  type PortCheck,
  PortField,
  SelectField,
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
import { Link, useNavigate } from "@tanstack/react-router";
import { type FormEvent, type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";

import { methodCommand } from "../../lib/cli";
import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { portRefusal, SERVICE_NAME, tidyMessage } from "../apps/app";
import { RunsOnField } from "../apps/NewAppSheet";
import { useProjectPage } from "../apps/projectPage";
import { validCpus, validMemory } from "../apps/specEdit";
import { usePlaces } from "../apps/useServices";
import { useAppWords } from "../apps/words";
import { useMay } from "../auth/may";
import { useSignedInUser } from "../auth/useAuthState";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { asGoodAsRoot } from "../changes/plan";
import { type PortHolder, useServerPorts } from "../servers/ports";
import { useFleetView } from "../servers/useFleet";
import { CopyAsCommand } from "../system/CopyCommand";
import { Facts, useEntryFacts } from "./access";
import { nextFree } from "./freeName";
import { projectLink } from "./media";
import { type OwnFault, ownFault, ownRule, strongValue } from "./ownSecret";
import { useOrigins } from "./useCatalog";
// The form is New app's, so its fields sit apart the same way, however the sheet is reached.
import "../apps/Apps.css";
// Its lines and the engine picker, wherever the sheet opens: on the Databases page too, which loads no catalog page.
import "./Catalog.css";

/** A label as it reads inside a sentence: "Adminer port" is "adminer port", while "API key" keeps its capitals. */
const inSentence = (label: string) =>
  /^[A-Z][a-z]*(\s|$)/.test(label) ? label.charAt(0).toLowerCase() + label.slice(1) : label;

/** How long a typed name rests before the control plane is asked whether it's free. */
const NAME_SETTLES_MS = 300;

/** Whether a name is free, as its field says beside it. Before: a project had it, whose data may come back. */
type NameState = "checking" | "free" | "taken" | "before";

/** Whether a person gives an input: Dockwell makes one that generates. */
const asked = (input: Input) => input.generate === 0;

/** What a person sets of a service's limits and restarts, in place of its entry's. */
interface Limit {
  cpus?: string;
  memory?: string;
  restart?: RestartPolicy;
}

const RESTARTS = [RestartPolicy.UNLESS_STOPPED, RestartPolicy.ALWAYS, RestartPolicy.ON_FAILURE, RestartPolicy.NO];

/** When Docker restarts a part as its entry has it, where unset is the driver's default. */
const entryRestart = (part: CatalogPart) =>
  part.restart === RestartPolicy.UNSPECIFIED ? RestartPolicy.UNLESS_STOPPED : part.restart;

/**
 * Install from the catalog (Catalog-Install), defaults first: the newest version,
 * a name that's free, the server, a free port for each it publishes and the
 * folders it reads, each a line with Change, and what only a person has as a
 * field. The rest, what it generates, what it installs and the CLI command, is
 * under Advanced. Nothing runs until the plan applies, and a password no one
 * types is made and sealed by the control plane, so it's never here. Once it
 * applies, it leads to where what it made is used: a database's page, with how
 * to connect, or the app's. Given engines, it's New database: the engine is the
 * first line, picked from them, and picking another keeps the sheet open.
 */
export function InstallSheet({
  id,
  onClose,
  engines,
}: {
  id: string;
  onClose: () => void;
  /** The databases to pick from, for New database on the Databases page. */
  engines?: { entries: readonly Entry[]; onPick: (id: string) => void };
}) {
  const { t } = useLingui();
  const words = useAppWords();
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
  // A person's own value for a secret Dockwell would generate, by its name: there once they choose to set one.
  const [own, setOwn] = useState<Readonly<Record<string, string>>>({});
  const [chosenServers, setChosenServers] = useState<readonly string[]>([]);
  // The defaults someone pressed Change on, which show as their fields from then on.
  const [changing, setChanging] = useState<ReadonlySet<string>>(new Set());
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  // The entry picked now, so a plan asked for another one, before the pick changed, is dropped.
  const picked = useRef(id);
  picked.current = id;
  const [failure, setFailure] = useState<string>();
  const [nameError, setNameError] = useState<string>();
  const [serversError, setServersError] = useState<string>();
  const [limits, setLimits] = useState<Readonly<Record<string, Limit>>>({});
  // The plan under review takes this sheet over, and Cancel brings the form back as it was.
  const [plan, setPlan] = useState<Opening>();
  const change = (key: string) => setChanging((was) => new Set(was).add(key));
  // Another engine starts from its own defaults.
  const [shown, setShown] = useState(id);
  if (shown !== id) {
    setShown(id);
    setTyped({});
    setTypedName(undefined);
    setOwn({});
    setChanging(new Set());
    setFailure(undefined);
    setNameError(undefined);
  }

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

  // What each part may use and when it restarts: the entry's, unless someone changed it.
  const parts = got.data?.parts ?? [];
  const cpusOf = (part: CatalogPart) => limits[part.service]?.cpus ?? part.cpus;
  const memoryOf = (part: CatalogPart) => limits[part.service]?.memory ?? part.memory;
  const restartOf = (part: CatalogPart) => limits[part.service]?.restart ?? entryRestart(part);
  const limitErrors = (part: CatalogPart) => {
    const cpus = cpusOf(part).trim();
    const memory = memoryOf(part).trim();
    return {
      cpus: cpus === "" || validCpus(cpus) ? undefined : t`Use a number of CPUs above 0, such as 0.5 or 2.`,
      memory: memory === "" || validMemory(memory) ? undefined : t`Use an amount from 6M, such as 512M or 2G.`,
    };
  };
  // The first limit that's wrong, which the footer names, as Advanced may be closed over it.
  const badPart = parts.find((part) => {
    const errors = limitErrors(part);
    return errors.cpus !== undefined || errors.memory !== undefined;
  });
  const limitsInvalid = badPart !== undefined;
  const badService = badPart?.service ?? "";
  const setLimit = (part: CatalogPart, next: Limit) =>
    setLimits((was) => ({ ...was, [part.service]: { ...was[part.service], ...next } }));
  // Only what differs from the entry goes in the request, so the plan and the command say no more.
  const tuned = <T,>(of: (part: CatalogPart) => T, entryOf: (part: CatalogPart) => T, given: (value: T) => boolean) =>
    Object.fromEntries(
      parts.flatMap((part) => {
        const v = of(part);
        return given(v) && v !== entryOf(part) ? [[part.service, v]] : [];
      }),
    );
  const memory: Record<string, string> = tuned(
    (part) => memoryOf(part).trim(),
    (part) => part.memory,
    (v) => v !== "",
  );
  const cpus: Record<string, string> = tuned(
    (part) => cpusOf(part).trim(),
    (part) => part.cpus,
    (v) => v !== "",
  );
  const restart: Record<string, RestartPolicy> = tuned(restartOf, entryRestart, () => true);

  const name = (typedName ?? got.data?.project ?? "").trim();
  const nameIsDefault = name === (got.data?.project ?? "");
  // A name typed is checked as it's typed: against the projects there are at once, and
  // then by the control plane, which knows the names used before and the next that's free.
  const places = usePlaces();
  const [settled, setSettled] = useState(name);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(name), NAME_SETTLES_MS);
    return () => clearTimeout(timer);
  }, [name]);
  const nameValid = name !== "" && SERVICE_NAME.test(name);
  const nameAsked = nameValid && !nameIsDefault && settled === name;
  const nameRead = useQuery(CatalogService.method.getCatalogEntry, { id, project: settled }, { enabled: nameAsked });
  const nameAnswer = nameAsked ? nameRead.data?.project : undefined;
  const nameHere = places.projects.includes(name);
  const nameState: NameState | undefined =
    !nameValid || got.data === undefined
      ? undefined
      : nameIsDefault
        ? "free"
        : nameHere
          ? "taken"
          : nameAnswer === undefined
            ? "checking"
            : nameAnswer === name
              ? "free"
              : "before";
  // The next that's free: the control plane's, or until it answers, the next the projects here leave.
  const nameNext =
    nameState === "taken" || nameState === "before" ? (nameAnswer ?? nextFree(name, places.projects)) : undefined;
  const nameTaken = nameState === "taken" || nameState === "before";
  // What it says of itself, as state, beside its defaults; how it signs in is under Advanced.
  // Before it's installed, the sheet says what it is, not what to do next: the sign-in is
  // its generated lines below, and its first steps wait for its page.
  const facts = useEntryFacts(entry).filter((f) => f.key !== "sign-in" && f.key !== "first");
  const nameOpen = changing.has("name") || nameError !== undefined;
  // A default shows as its field once someone changes it, and from the start when there's no default to show.
  const open = (input: Input) => changing.has(input.name) || suggested(input) === "";
  const missing = inputs.find((input) => !input.optional && value(input).trim() === "");
  const nameInvalid = !nameValid;
  // An own value chosen and left empty is one to give; one the app won't take is one to fix.
  const ownEmpty = generated.find((input) => own[input.name] === "");
  const ownWrong = generated.find(
    (input) =>
      own[input.name] !== undefined &&
      own[input.name] !== "" &&
      ownFault(ownRule(input), own[input.name] ?? "") !== undefined,
  );
  // Where it comes from decides what the sheet says of it, so a plan waits until that's read.
  const origins = useOrigins();
  const source = origins.origins.get(id)?.name;
  const ready =
    entry !== undefined &&
    origins.data !== undefined &&
    !origins.error &&
    !nameInvalid &&
    !nameTaken &&
    !missing &&
    !ownEmpty &&
    !ownWrong &&
    !limitsInvalid;
  const app = entry?.name ?? id;
  const webService = entry?.web?.service ?? "";
  const missingLabel = (missing ?? ownEmpty)?.label ?? "";
  const wrongLabel = ownWrong?.label ?? "";
  const license = entry?.appLicense ?? "";
  const values = {
    ...Object.fromEntries(inputs.map((input) => [input.name, value(input).trim()])),
    ...Object.fromEntries(Object.entries(own).filter(([, v]) => v !== "")),
  };
  const ownCount = generated.filter((input) => own[input.name] !== undefined).length;

  // The same install from the CLI: what differs from what it would pick, with a typed secret from your environment.
  const secrets = new Set([
    ...inputs.filter((input) => input.type === InputType.SECRET).map((input) => input.name),
    ...Object.keys(own),
  ]);
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
          memory,
          cpus,
          restart,
        },
        (_path, key) => !secrets.has(key),
      )
    : "";
  const summary = [
    ...more.filter((input) => value(input).trim() !== "").map((input) => `${input.label} ${value(input).trim()}`),
    ...(generated.length > ownCount
      ? [plural(generated.length - ownCount, { one: "# secret generated", other: "# secrets generated" })]
      : []),
    ...(ownCount > 0 ? [plural(ownCount, { one: "# secret of your own", other: "# secrets of your own" })] : []),
    // The parts someone set a limit or a restart for, as what they set.
    ...parts.flatMap((part) => {
      const set = [
        cpus[part.service] ? t`${cpus[part.service]} vCPU` : "",
        memory[part.service] ?? "",
        restart[part.service] !== undefined ? words.restart(restart[part.service] as RestartPolicy) : "",
      ].filter(Boolean);
      return set.length > 0 ? [`${part.service} ${set.join(", ")}`] : [];
    }),
    ...parts.map((part) => part.image),
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
    const asked = id;
    try {
      const res = await client.installCatalogEntry({
        id,
        project: name,
        values,
        servers: [...chosenServers],
        memory,
        cpus,
        restart,
      });
      if (picked.current !== asked) return;
      review(res.change);
    } catch (err) {
      if (picked.current !== asked) return;
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
      title={engines ? t`New database` : t`Install ${app}`}
      subtitle={engines ? undefined : entry?.summary}
      onClose={onClose}
      className={cx("app-sheet", "catalog-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name={entry?.hostAccess ? "shield" : "commit"} size={14} />
            {missing || ownEmpty ? (
              <Trans>Give {missingLabel} a value to review the plan</Trans>
            ) : ownWrong ? (
              <Trans>Fix {wrongLabel} to review the plan</Trans>
            ) : limitsInvalid ? (
              <Trans>Fix the limits of {badService} to review the plan</Trans>
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
        {origins.error ? (
          <Callout tone="critical" title={t`Couldn't read the sources`}>
            {sentence(origins.error)}{" "}
            <Button variant="ghost" onClick={() => void origins.refetch()}>
              <Trans>Try again</Trans>
            </Button>
          </Callout>
        ) : !origins.data ? (
          <p className="catalog-note" aria-busy="true">
            <Trans>Reading the sources</Trans>
          </p>
        ) : null}
        {failure ? (
          <Callout tone="critical" title={t`Can't make a plan`}>
            {failure}
          </Callout>
        ) : null}
        {entry ? (
          <>
            <div className="catalog-chosen-list">
              {engines ? (
                <EnginePicker entries={engines.entries} value={id} onPick={engines.onPick} disabled={busy} />
              ) : (
                <Chosen label={t`Version`} value={entry.version} />
              )}
              {nameOpen ? (
                <NameField
                  value={typedName ?? got.data?.project ?? ""}
                  name={name}
                  state={nameState}
                  next={nameNext}
                  autoFocus={changing.has("name") && !nameError}
                  error={
                    nameError ??
                    (nameInvalid
                      ? t`Use lowercase letters, digits and dashes, starting with a letter, such as shop.`
                      : undefined)
                  }
                  onChange={(next) => {
                    setTypedName(next);
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
              {source ? (
                <Chosen label={t`Catalog`} value={t`${source} · Not tested by Dockwell`} tone="warning" />
              ) : null}
              {entry.hostAccess ? <Chosen label={t`Reaches`} value={entry.hostAccess} tone="warning" /> : null}
            </div>
            <Facts facts={facts} className="catalog-sheet-facts" />
            <Advanced open={advancedOpen} onOpenChange={setAdvancedOpen} summary={summary}>
              {more.map(field)}
              {generated.length > 0 ? (
                <div className="catalog-chosen-list">
                  {generated.map((input) =>
                    own[input.name] !== undefined ? (
                      <OwnSecretField
                        key={input.name}
                        input={input}
                        value={own[input.name] ?? ""}
                        onChange={(next) => setOwn((was) => ({ ...was, [input.name]: next }))}
                        onUseGenerated={() =>
                          setOwn((was) => Object.fromEntries(Object.entries(was).filter(([k]) => k !== input.name)))
                        }
                      />
                    ) : (
                      <Chosen
                        key={input.name}
                        label={input.label}
                        value={input.logged ? t`Generated, in ${app}'s log` : t`Generated and sealed`}
                        plain
                        changeLabel={t`Set my own`}
                        changeName={t`Set my own ${inSentence(input.label)}`}
                        onChange={() => setOwn((was) => ({ ...was, [input.name]: "" }))}
                      />
                    ),
                  )}
                </div>
              ) : null}
              <Limits
                parts={parts}
                changing={changing}
                cpusOf={cpusOf}
                memoryOf={memoryOf}
                restartOf={restartOf}
                errorsOf={limitErrors}
                onChangeLine={(part) => change(`limits:${part.service}`)}
                onChange={setLimit}
              />
              <Parts
                parts={parts}
                typed={(name) => {
                  const input = inputs.find((i) => i.name === name);
                  return input ? value(input).trim() : "";
                }}
              />
              <p className="catalog-sheet-source">
                <Trans>
                  {app} is under the {license} licence.
                </Trans>{" "}
                {projectLink(entry.homepage) ? (
                  <a href={projectLink(entry.homepage)} target="_blank" rel="noreferrer" className="catalog-link">
                    <Trans>Its site</Trans>
                  </a>
                ) : null}
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
  changeLabel,
  changeName,
}: {
  label: string;
  value: string;
  state?: ReactNode;
  onChange?: () => void;
  tone?: "warning";
  /** The value is words, not something to copy, such as "Generated and sealed". */
  plain?: boolean;
  /** What the action says in place of Change, such as "Set my own", and its name for a screen reader. */
  changeLabel?: string;
  changeName?: string;
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
          <button
            type="button"
            className="catalog-change"
            aria-label={changeName ?? t`Change ${what}`}
            onClick={onChange}
          >
            {changeLabel ?? <Trans>Change</Trans>}
          </button>
        ) : null}
      </span>
    </fieldset>
  );
}

/**
 * What each part may use of the server and when Docker restarts it, as its entry
 * gives them: a line for each, and Change for its fields. An upgrade keeps what's
 * set here, as it keeps what someone changes on the app.
 */
function Limits({
  parts,
  changing,
  cpusOf,
  memoryOf,
  restartOf,
  errorsOf,
  onChangeLine,
  onChange,
}: {
  parts: readonly CatalogPart[];
  changing: ReadonlySet<string>;
  cpusOf: (part: CatalogPart) => string;
  memoryOf: (part: CatalogPart) => string;
  restartOf: (part: CatalogPart) => RestartPolicy;
  errorsOf: (part: CatalogPart) => { cpus?: string; memory?: string };
  onChangeLine: (part: CatalogPart) => void;
  onChange: (part: CatalogPart, next: Limit) => void;
}) {
  const { t } = useLingui();
  const words = useAppWords();
  const titleId = useId();
  if (parts.length === 0) return null;
  // A field left empty keeps what the entry gives.
  const line = (part: CatalogPart) => {
    const cpus = cpusOf(part).trim() || part.cpus;
    const memory = memoryOf(part).trim() || part.memory;
    const uses = [cpus ? t`${cpus} vCPU` : "", memory].filter(Boolean).join(", ");
    return `${uses || t`No limits`} · ${t`Restarts ${inSentence(words.restart(restartOf(part)))}`}`;
  };
  return (
    <section className="catalog-limits" aria-labelledby={titleId}>
      <h3 id={titleId}>
        <Trans>Limits and restarts</Trans>
      </h3>
      <div className="catalog-chosen-list">
        {parts.map((part) => {
          if (!changing.has(`limits:${part.service}`)) {
            return (
              <Chosen
                key={part.service}
                label={part.service}
                value={line(part)}
                plain
                onChange={() => onChangeLine(part)}
              />
            );
          }
          const errors = errorsOf(part);
          return (
            <fieldset key={part.service} className="catalog-limit" aria-label={part.service}>
              <legend className="catalog-chosen-label">{part.service}</legend>
              <div className="app-settings-grid">
                <TextField
                  label={t`CPU limit`}
                  mono
                  inputMode="decimal"
                  autoFocus
                  value={cpusOf(part)}
                  placeholder={part.cpus ? t`Entry's: ${part.cpus}` : t`No limit`}
                  suffix="vCPU"
                  error={errors.cpus}
                  onChange={(event) => onChange(part, { cpus: event.target.value })}
                />
                <TextField
                  label={t`Memory limit`}
                  mono
                  value={memoryOf(part)}
                  placeholder={part.memory ? t`Entry's: ${part.memory}` : t`No limit`}
                  hint={t`Such as 512M or 2G.`}
                  error={errors.memory}
                  onChange={(event) => onChange(part, { memory: event.target.value })}
                />
                <SelectField
                  label={t`Restarts`}
                  value={String(restartOf(part))}
                  onChange={(event) => onChange(part, { restart: Number(event.target.value) as RestartPolicy })}
                  options={RESTARTS.map((p) => ({ value: String(p), label: words.restart(p) }))}
                />
              </div>
            </fieldset>
          );
        })}
      </div>
    </section>
  );
}

/** The engine of a new database, picked from those the catalog has, each with its newest version. */
function EnginePicker({
  entries,
  value,
  onPick,
  disabled,
}: {
  entries: readonly Entry[];
  value: string;
  onPick: (id: string) => void;
  // While its plan is asked for, the pick stays as it is.
  disabled?: boolean;
}) {
  const { t } = useLingui();
  const id = useId();
  return (
    <fieldset className="catalog-engines" aria-labelledby={id} disabled={disabled}>
      <span id={id} className="catalog-chosen-label">
        {t`Engine`}
      </span>
      <span className="catalog-engines-list">
        {entries.map((entry) => (
          <label key={entry.id} className={cx("catalog-engine", entry.id === value && "is-picked")}>
            <input
              type="radio"
              name={id}
              value={entry.id}
              checked={entry.id === value}
              onChange={() => onPick(entry.id)}
            />
            <span className="catalog-engine-name">{entry.name}</span>
            <code className="catalog-engine-version">{entry.version}</code>
          </label>
        ))}
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

/**
 * The name of what an install makes, checked as it's typed (Check as they go): free,
 * or taken with the next free one to use in a click and a way to what has it, so the
 * sheet never lands on a name the plan would refuse.
 */
function NameField({
  value,
  name,
  state,
  next,
  error,
  autoFocus,
  onChange,
}: {
  value: string;
  /** The name as it's checked: what's typed, without spaces at its ends. */
  name: string;
  state: NameState | undefined;
  next: string | undefined;
  error: string | undefined;
  autoFocus: boolean;
  onChange: (next: string) => void;
}) {
  const { t } = useLingui();
  const words = useUIStrings().portField;
  const stateId = useId();
  const page = useProjectPage(state === "taken" ? name : "");
  const use = next ? (
    <button type="button" className="catalog-inline-action" onClick={() => onChange(next)}>
      <Trans>Use {next}</Trans>
    </button>
  ) : null;
  const open = page ? (
    <Link {...page} className="catalog-inline-action">
      <Trans>Open {name}</Trans>
    </Link>
  ) : null;
  const message =
    error ??
    (state === "taken" ? (
      <>
        <Trans>A project named {name} is here already</Trans>
        {use ? <> · {use}</> : null}
        {open ? <> · {open}</> : null}
      </>
    ) : state === "before" ? (
      <>
        <Trans>A project named {name} was here before, and its data may come back</Trans>
        {use ? <> · {use}</> : null}
      </>
    ) : undefined);
  const shown = error ? undefined : state === "before" ? "taken" : state;
  return (
    <TextField
      label={t`Name`}
      mono
      autoFocus={autoFocus}
      value={value}
      error={message}
      aria-describedby={shown ? stateId : undefined}
      suffix={
        shown ? (
          <span id={stateId} className={cx("catalog-chosen-state", `is-${shown}`)} aria-live="polite">
            <Glyph
              shape={
                shown === "checking"
                  ? STATUS.deploying.shape
                  : shown === "free"
                    ? TONE.healthy.shape
                    : TONE.critical.shape
              }
              size={9}
            />
            <span>{words[shown]}</span>
          </span>
        ) : undefined
      }
      onChange={(event) => onChange(event.target.value)}
    />
  );
}

/**
 * A person's own value for a secret Dockwell would generate (Defaults first, power one
 * step away): hidden as it's typed or pasted, with Show, Copy, and Generate, which
 * fills a strong one here to keep; what the app takes says itself under the field and
 * turns to what's wrong as it's typed (Check as they go). It's sealed as a generated one
 * is, and never in a plan.
 */
function OwnSecretField({
  input,
  value,
  onChange,
  onUseGenerated,
}: {
  input: Input;
  value: string;
  onChange: (next: string) => void;
  onUseGenerated: () => void;
}) {
  const { t } = useLingui();
  const [shown, setShown] = useState(false);
  const rule = ownRule(input);
  const fault = value === "" ? undefined : ownFault(rule, value);
  const takes =
    rule.form === "hex"
      ? t`${rule.size} hex digits`
      : rule.form === "base64"
        ? t`${rule.size} bytes in base64`
        : rule.urlSafe
          ? t`At least ${rule.min} characters: letters, digits and - . _ ~`
          : t`At least ${rule.min} characters`;
  const faults: Record<OwnFault, string> = {
    ends: t`Take away the space at its start or end.`,
    form: rule.form === "base64" ? t`Use ${input.generate} bytes in base64.` : t`Use ${input.generate} hex digits.`,
    short: t`Use at least ${rule.form === "text" ? rule.min : 0} characters.`,
    unprintable: t`Use printable characters alone.`,
    url: t`Use letters, digits and - . _ ~ alone, as it goes in a URL.`,
  };
  const label = inSentence(input.label);
  return (
    <div className="catalog-own">
      <TextField
        label={input.label}
        mono
        autoFocus
        type={shown ? "text" : "password"}
        autoComplete="new-password"
        value={value}
        error={fault ? faults[fault] : undefined}
        hint={
          <span className={cx("catalog-own-takes", value !== "" && "is-met")}>
            {value !== "" ? <Glyph shape={TONE.healthy.shape} size={9} /> : null}
            {takes}
          </span>
        }
        onChange={(event) => onChange(event.target.value)}
        action={
          <span className="catalog-own-actions">
            <button
              type="button"
              className="app-reveal"
              aria-pressed={shown}
              aria-label={t`Show ${label}`}
              title={shown ? t`Hide ${label}` : t`Show ${label}`}
              onClick={() => setShown((was) => !was)}
            >
              <Icon name="eye" size={16} />
            </button>
            {value !== "" ? (
              <CopyButton
                variant="ghost"
                iconOnly
                text={value}
                label={t`Copy ${label}`}
                copiedLabel={t`${label} copied`}
              />
            ) : null}
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                onChange(strongValue(rule, input.generate));
                setShown(true);
              }}
            >
              <Trans>Generate</Trans>
            </Button>
          </span>
        }
      />
      <button
        type="button"
        className="catalog-change catalog-own-back"
        aria-label={t`Use a generated ${label}`}
        onClick={onUseGenerated}
      >
        <Trans>Use generated</Trans>
      </button>
    </div>
  );
}
