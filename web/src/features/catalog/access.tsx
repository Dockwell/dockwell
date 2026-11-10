// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  ChangeService,
  DomainService,
  type EditSchema,
  type Entry,
  type Input,
  InputType,
  type ServiceInfo,
  ServiceService,
  VariableService,
} from "@dockwell/api";
import { Button, ConnectionCard, type ConnectionItem, CopyButton, Icon, type IconName } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { sentence } from "../../lib/errors";
import { appOf, environmentOf, nameOf, valuesOf } from "../apps/app";
import { type EnvEdit, envEdits } from "../apps/envEdit";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { useSignedIn } from "../auth/useAuthState";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { useReveals } from "../variables/useReveal";
import "./Catalog.css";

/** One thing an install says of itself, as state, with what resolves it where something does. */
export interface Fact {
  key: string;
  icon: IconName;
  text: ReactNode;
  action?: ReactNode;
}

/** The facts as a list, each a line with its icon and, where there is one, its action. */
export function Facts({ facts, className }: { facts: readonly Fact[]; className?: string }) {
  if (facts.length === 0) return null;
  return (
    <ul className={className ?? "catalog-facts-list"}>
      {facts.map((f) => (
        <li key={f.key}>
          <Icon name={f.icon} size={14} />
          <span>{f.text}</span>
          {f.action ? <span className="catalog-fact-action">{f.action}</span> : null}
        </li>
      ))}
    </ul>
  );
}

/**
 * What an entry says of itself before it's installed, as lines of state rather than
 * steps (#426): how to sign in, who owns it first, what follows its domain, and its
 * notes. A store page and the install sheet show these.
 */
export function useEntryFacts(entry: Entry | undefined): Fact[] {
  const { t } = useLingui();
  if (!entry) return [];
  const out: Fact[] = [];
  const signIn = entry.signIn;
  const password = inputNamed(entry, signIn?.passwordInput);
  if (signIn) {
    const user = signIn.user || inputNamed(entry, signIn.userInput)?.label || "";
    const generated = password !== undefined && password.generate > 0;
    out.push({
      key: "sign-in",
      icon: "key",
      text: user
        ? generated
          ? t`Signs in as ${user}, with a password Dockwell generates and seals`
          : t`Signs in as ${user}`
        : generated
          ? t`Signs in with a key Dockwell generates and seals`
          : t`Signs in with a key you give`,
    });
  }
  if (entry.firstVisitorOwns) out.push({ key: "owns", icon: "user", text: t`Its first account is its owner` });
  if (entry.signups)
    out.push({
      key: "signups",
      icon: "users",
      text: t`Anyone who reaches it can make an account, until you close sign-ups`,
    });
  if (entry.addressVariables.length > 0) {
    const names = entry.addressVariables.map((a) => a.variable).join(", ");
    out.push({ key: "address", icon: "globe", text: t`Its links follow the domain you add: ${names}` });
  }
  for (const [i, note] of entry.notes.entries()) out.push({ key: `note-${i}`, icon: "question", text: note });
  if (entry.firstSteps) out.push({ key: "first", icon: "arrow-right", text: entry.firstSteps });
  return out;
}

function inputNamed(entry: Entry, name: string | undefined): Input | undefined {
  return name ? entry.inputs.find((i) => i.name === name) : undefined;
}

/**
 * How to sign in to an installed app (#426): its user and its password, which a
 * passkey reveals in place, as a card to copy from rather than a sentence naming the
 * secret. Where the app keeps its password once set, it says where to change it.
 */
export function SignInCard({ entry, install }: { entry: Entry; install: CatalogInstall }) {
  const { t } = useLingui();
  const signIn = entry.signIn;
  const environment = install.environmentId;
  const secretName = signIn?.passwordInput ?? "";
  const variables = useQuery(VariableService.method.listVariables, { environment }, { enabled: secretName !== "" });
  const secretId = variables.data?.variables.find((v) => v.secret && v.name === secretName)?.id ?? "";
  const reveals = useReveals();
  const shown = secretId ? reveals.get(secretId) : {};
  const mayReveal = useMay()(VariableService.method.revealSecret, { environment });
  if (!signIn) return null;
  const userInput = inputNamed(entry, signIn.userInput);
  const user = signIn.user || install.pin?.values[signIn.userInput] || userInput?.defaultValue || "";
  const password = inputNamed(entry, secretName);
  const app = entry.name;
  const where = signIn.changedIn;
  const items: ConnectionItem[] = [
    ...(user ? [{ label: userInput?.type === InputType.EMAIL ? t`Email` : t`User`, value: user }] : []),
    ...(signIn.server ? [{ label: t`Server`, value: signIn.server }] : []),
  ];
  return (
    <ConnectionCard
      title={t`Sign in`}
      className="catalog-sign-in"
      items={items}
      secret={
        password
          ? {
              label: password.label || t`Password`,
              value: shown.value,
              busy: shown.busy ?? false,
              problem: shown.problem ?? (variables.error ? sentence(variables.error) : undefined),
              hint:
                password.setOnce && where ? t`Set as it first started: change it in ${app}, in ${where}` : undefined,
              revealOff: mayReveal.off.off,
              onReveal: () => {
                if (secretId) reveals.reveal(secretId);
              },
              onHide: () => {
                if (secretId) reveals.hide(secretId);
              },
            }
          : undefined
      }
    />
  );
}

/** Sets each variable a service has of its own, and has it use one of its environment's for one it doesn't. */
function setting(service: ServiceInfo, values: Readonly<Record<string, string>>): Map<string, EnvEdit> {
  const own = new Set(valuesOf(service).map((v) => v.name));
  return new Map(
    Object.entries(values).map(([name, value]): [string, EnvEdit] => [
      name,
      own.has(name) ? { op: "change", name, value } : { op: "add", name, value, secret: false },
    ]),
  );
}

/**
 * The edits that point a catalog app's links at domain: each variable its entry
 * says follows the domain (Entry.address_variables), set on the service of its
 * own name, found by inEnvironment.
 */
export function addressEdits(
  entry: Entry,
  domain: string,
  inEnvironment: (name: string) => ServiceInfo | undefined,
): MessageInitShape<typeof EditSchema>[] {
  const byService = new Map<string, Record<string, string>>();
  for (const a of entry.addressVariables) {
    const values = byService.get(a.service) ?? {};
    values[a.variable] = a.value.replaceAll("{domain}", domain);
    byService.set(a.service, values);
  }
  return [...byService].flatMap(([name, values]) => {
    const service = inEnvironment(name);
    return service ? envEdits(service, setting(service, values)) : [];
  });
}

/**
 * What an installed app says of itself as state, each with the action that resolves it
 * where there is one: closing sign-ups, and setting what follows its domain to the
 * domain it has, each a plan.
 */
export function useInstallFacts(
  entry: Entry | undefined,
  install: CatalogInstall | undefined,
  web: ServiceInfo | undefined,
): Fact[] {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const services = useServices().data ?? [];
  const mayPropose = useMay()(ChangeService.method.proposeChange, { environment: install?.environmentId ?? "" });
  const { servers } = useFleetView();
  const onLocalhost = useSignedIn()?.rpId === "localhost";
  const domains =
    useQuery(
      DomainService.method.listDomains,
      { environment: web ? environmentOf(web) : "", service: web ? nameOf(web) : "" },
      { enabled: web !== undefined },
    ).data?.domains ?? [];
  if (!entry || !install) return [];
  const app = entry.name;
  const inEnvironment = (name: string) =>
    services.find((s) => environmentOf(s) === install.environmentId && nameOf(s) === name);
  const out: Fact[] = [];
  if (entry.firstVisitorOwns) {
    out.push({
      key: "owns",
      icon: "user",
      text:
        domains.length > 0
          ? t`Its first account is its owner`
          : t`Its first account is its owner: make yours before it has a domain`,
    });
  }
  const signups = entry.signups;
  const signupsService = signups ? inEnvironment(signups.service) : undefined;
  if (signups && signupsService) {
    const variable = signups.variable;
    const closed = signups.closed;
    out.push({
      key: "signups",
      icon: "users",
      text: t`Anyone who reaches it can make an account until sign-ups close`,
      action: (
        <Button
          size="sm"
          variant="ghost"
          {...mayPropose.off}
          onClick={() =>
            sheet.open({
              kind: "propose",
              summary: t`Close sign-ups to ${app}`,
              edits: envEdits(signupsService, setting(signupsService, { [variable]: closed })),
            })
          }
        >
          <Trans>Close sign-ups</Trans>
        </Button>
      ),
    });
  }
  if (entry.addressVariables.length > 0) {
    const names = entry.addressVariables.map((a) => a.variable).join(", ");
    const domain = domains[0]?.name ?? "";
    if (!domain) {
      out.push({ key: "address", icon: "globe", text: t`Its links follow the domain you add: ${names}` });
    } else {
      const edits = addressEdits(entry, domain, inEnvironment);
      out.push({
        key: "address",
        icon: "globe",
        text: t`Its links follow its domain, ${domain}: ${names}`,
        action: (
          <Button
            size="sm"
            variant="ghost"
            {...mayPropose.off}
            onClick={() => sheet.open({ kind: "propose", summary: t`Point ${app} at ${domain}`, edits })}
          >
            <Trans>Set to {domain}</Trans>
          </Button>
        ),
      });
    }
  }
  // Before a domain, it opens through the panel on localhost (ADR 0061). On the panel's
  // domain, which doesn't open apps yet, the way in is a tunnel: a command to copy with
  // what only the panel knows filled in, not a paragraph.
  const local = web && domains.length === 0 ? appOf(web)?.ports.find((p) => p.hostIp === "127.0.0.1") : undefined;
  const server = servers?.find((s) => s.id === web?.deployments[0]?.serverId);
  const address = server?.address;
  if (local && web && (onLocalhost || address)) {
    const port = String(local.hostPort);
    const tunnel = `ssh -N -L ${port}:127.0.0.1:${port} ${address ?? ""}`;
    out.push({
      key: "local",
      icon: "lock",
      text: t`Answers on this server alone, on port ${port}, until it has a domain`,
      action: onLocalhost ? (
        <OpenAppButton
          serviceId={web.id}
          environment={install.environmentId}
          port={local.containerPort}
          app={entry.category === "databases" ? nameOf(web) : app}
        />
      ) : (
        <CopyButton
          text={tunnel}
          label={t`Copy the SSH tunnel command`}
          copiedLabel={t`SSH tunnel command copied`}
          variant="ghost"
        />
      ),
    });
  }
  for (const [i, note] of entry.notes.entries()) out.push({ key: `note-${i}`, icon: "question", text: note });
  if (entry.firstSteps) out.push({ key: "first", icon: "arrow-right", text: entry.firstSteps });
  return out;
}

/**
 * Opens a service's web UI through the panel, in a tab of its own (ADR 0061). The tab
 * opens as the button is pressed, so no popup blocker stops it, and goes to the app
 * once the control plane hands over its one-time address.
 */
export function OpenAppButton({
  serviceId,
  environment,
  port,
  app,
}: {
  serviceId: string;
  /** The service's environment, by ID, whose role decides who may open it. */
  environment: string;
  port?: number;
  app: string;
}) {
  const { t } = useLingui();
  // As much as revealing a secret: a developer's, off with why for anyone else (ADR 0059, ADR 0061).
  const mayOpen = useMay()(ServiceService.method.openService, { environment });
  const transport = useTransport();
  const explain = useAuthError();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const open = async () => {
    setProblem(undefined);
    setBusy(true);
    const tab = window.open("about:blank", "_blank");
    try {
      const res = await createClient(ServiceService, transport).openService({ id: serviceId, port: port ?? 0 });
      if (tab) {
        tab.opener = null;
        tab.location.href = res.url;
      } else {
        window.location.assign(res.url);
      }
    } catch (err) {
      tab?.close();
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <span className="catalog-open">
      <Button size="sm" icon="external" loading={busy} onClick={() => void open()} {...mayOpen.off}>
        {t`Open ${app}`}
      </Button>
      {problem ? (
        <span className="catalog-open-problem" role="alert">
          {problem}
        </span>
      ) : null}
    </span>
  );
}
