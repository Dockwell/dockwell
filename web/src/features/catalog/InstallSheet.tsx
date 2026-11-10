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
  Button,
  Callout,
  cx,
  Icon,
  PHONE_QUERY,
  type PlanChange,
  PlanDiff,
  type PlanResource,
  Sheet,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type FormEvent, useMemo, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { portRefusal, SERVICE_NAME, tidyMessage } from "../apps/app";
import { RunsOnField } from "../apps/NewAppSheet";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { asGoodAsRoot } from "../changes/plan";
import { useFleetView } from "../servers/useFleet";
// The form is New app's, so its fields sit apart the same way, however the sheet is reached.
import "../apps/Apps.css";
import { useMay } from "../auth/may";

/** Whether a person gives an input: Dockwell makes one that generates. */
const asked = (input: Input) => input.generate === 0;

/**
 * Install from the catalog (Catalog-Install): one sheet that asks only what the app
 * needs, starting from what's free, and ends in the plan. Nothing runs until the plan
 * applies, and a password no one types is made and sealed by the control plane, so
 * it's never here. Once it applies, it leads to the app, with where it's reached.
 */
export function InstallSheet({ id, onClose }: { id: string; onClose: () => void }) {
  const { t, i18n } = useLingui();
  // Installing makes a project of its own, which is an admin's (ADR 0049): off, with why, for anyone else.
  const mayInstall = useMay()(CatalogService.method.installCatalogEntry);
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const navigate = useNavigate();
  const { fleet, servers, only } = useFleetView();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const got = useQuery(CatalogService.method.getCatalogEntry, { id });
  const entry = got.data?.entry;
  const inputs = (entry?.inputs ?? []).filter(asked);
  const generated = (entry?.inputs ?? []).filter((input) => !asked(input));

  // Kept apart from what the control plane suggests, so a value typed stays when it reads again.
  const [typed, setTyped] = useState<Readonly<Record<string, string>>>({});
  const [typedName, setTypedName] = useState<string>();
  const [chosenServers, setChosenServers] = useState<readonly string[]>([]);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [nameError, setNameError] = useState<string>();
  const [serversError, setServersError] = useState<string>();
  // The plan under review takes this sheet over, and Cancel brings the form back as it was.
  const [plan, setPlan] = useState<Opening>();

  const name = (typedName ?? got.data?.project ?? "").trim();
  const value = (input: Input) => typed[input.name] ?? got.data?.values[input.name] ?? "";
  const missing = inputs.find((input) => !input.optional && value(input).trim() === "");
  const nameInvalid = name === "" || !SERVICE_NAME.test(name);
  const ready = entry !== undefined && !nameInvalid && !missing;
  const app = entry?.name ?? id;
  const version = entry?.version ?? "";
  const onlyName = only?.name ?? "";
  const webService = entry?.web?.service ?? "";
  const missingLabel = missing?.label ?? "";
  const sealed = generated.filter((input) => !input.logged);
  const logged = generated.filter((input) => input.logged);
  // Labels as they are, as they're the fields' own too, joined as the reader's language
  // does, with a space between a label and a joining word in Han, such as 和.
  const listed = (inputs: readonly Input[]) =>
    new Intl.ListFormat(i18n.locale, { type: "conjunction" })
      .formatToParts(inputs.map((input) => input.label))
      .map((part) =>
        part.type === "literal" && /\p{Script=Han}/u.test(part.value) ? ` ${part.value.trim()} ` : part.value,
      )
      .join("");
  const made = listed(sealed);
  const inLog = listed(logged);
  const license = entry?.appLicense ?? "";

  const review = (change: Change | undefined) => {
    const opens = change?.plan?.resources.find((r) => r.resource?.kind === "service" && r.resource.name === webService);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      ...(opens ? { doneLabel: t`Open ${app}` } : { doneLabel: t`Show the apps` }),
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
        if (opens && after.reverts === "") void navigate({ to: "/apps/$serviceId", params: { serviceId: opens.id } });
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
      const values = Object.fromEntries(inputs.map((input) => [input.name, value(input).trim()]));
      const res = await client.installCatalogEntry({ id, project: name, values, servers: [...chosenServers] });
      review(res.change);
    } catch (err) {
      if (reasonOf(err) === "project_exists") setNameError(sentence(err));
      else setFailure(portRefusal(sentence(err)) ?? sentence(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      title={t`Install ${app}`}
      subtitle={entry ? `${t`Version ${version}`} · ${entry.summary}` : undefined}
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
            {entry.hostAccess ? (
              <Callout tone="warning" title={t`It reaches past its containers`}>
                {entry.hostAccess}
              </Callout>
            ) : null}
            {entry.firstSteps ? (
              <Callout tone="warning" title={t`What to do first`}>
                {entry.firstSteps}
              </Callout>
            ) : null}
            <TextField
              label={t`Name`}
              mono
              value={typedName ?? got.data?.project ?? ""}
              error={
                nameError ??
                (typedName !== undefined && nameInvalid
                  ? t`Use lowercase letters, digits and dashes, starting with a letter, such as shop.`
                  : undefined)
              }
              hint={t`Its project, which keeps it apart from your other apps.`}
              onChange={(event) => {
                setTypedName(event.target.value);
                setNameError(undefined);
              }}
            />
            {inputs.map((input) => (
              <InputField
                key={input.name}
                input={input}
                value={value(input)}
                onChange={(next) => setTyped((was) => ({ ...was, [input.name]: next }))}
              />
            ))}
            {sealed.length > 0 ? (
              <p className="catalog-sheet-note">
                <Icon name="key" size={14} />
                <span>
                  {plural(sealed.length, {
                    one: `Dockwell generates this and keeps it sealed, for you to reveal with your passkey: ${made}`,
                    other: `Dockwell generates these and keeps them sealed, for you to reveal with your passkey: ${made}`,
                  })}
                </span>
              </p>
            ) : null}
            {logged.length > 0 ? (
              <p className="catalog-sheet-note">
                <Icon name="key" size={14} />
                <span>
                  {plural(logged.length, {
                    one: `Dockwell generates this, but ${app} writes it in its log, which takes no passkey to read, so change it after you first sign in: ${inLog}`,
                    other: `Dockwell generates these, but ${app} writes them in its log, which takes no passkey to read, so change them after you first sign in: ${inLog}`,
                  })}
                </span>
              </p>
            ) : null}
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
              <p className="catalog-sheet-note">
                <Icon name="server" size={14} />
                <span>
                  <Trans>Runs on {onlyName}</Trans>
                </span>
              </p>
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

/** A field for one input, as its type asks. A secret never shows as you type it, or stays in a browser's memory. */
function InputField({ input, value, onChange }: { input: Input; value: string; onChange: (next: string) => void }) {
  const secret = input.type === InputType.SECRET;
  const port = input.type === InputType.PORT;
  return (
    <TextField
      label={input.label}
      hint={input.hint || undefined}
      optional={input.optional}
      mono={port || secret}
      type={secret ? "password" : input.type === InputType.EMAIL ? "email" : "text"}
      inputMode={port ? "numeric" : undefined}
      autoComplete="off"
      value={value}
      onChange={(event) => onChange(event.target.value)}
    />
  );
}
