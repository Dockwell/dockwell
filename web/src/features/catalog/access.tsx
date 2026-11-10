// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  ChangeService,
  DomainService,
  type Entry,
  type Input,
  InputType,
  type ServiceInfo,
  VariableService,
} from "@dockwell/api";
import { Button, ConnectionCard, type ConnectionItem, CopyButton, Icon, type IconName } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { sentence } from "../../lib/errors";
import { appOf, environmentOf, nameOf, valuesOf } from "../apps/app";
import { type EnvEdit, envEdits } from "../apps/envEdit";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
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
    out.push({ key: "address", icon: "globe", text: t`${names} follow the domain you add` });
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
  // Sets each variable a service has of its own, and has it use one of its environment's for one it doesn't.
  const setting = (service: ServiceInfo, values: Readonly<Record<string, string>>): Map<string, EnvEdit> => {
    const own = new Set(valuesOf(service).map((v) => v.name));
    return new Map(
      Object.entries(values).map(([name, value]): [string, EnvEdit] => [
        name,
        own.has(name) ? { op: "change", name, value } : { op: "add", name, value, secret: false },
      ]),
    );
  };
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
      out.push({ key: "address", icon: "globe", text: t`${names} follow the domain you add` });
    } else {
      const byService = new Map<string, Record<string, string>>();
      for (const a of entry.addressVariables) {
        const values = byService.get(a.service) ?? {};
        values[a.variable] = a.value.replaceAll("{domain}", domain);
        byService.set(a.service, values);
      }
      const edits = [...byService].flatMap(([name, values]) => {
        const service = inEnvironment(name);
        return service ? envEdits(service, setting(service, values)) : [];
      });
      out.push({
        key: "address",
        icon: "globe",
        text: t`${names} follow its domain, ${domain}`,
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
  // Until it opens through the panel (ADR 0061), the way in before a domain is a tunnel,
  // which is a command to copy with what only the panel knows filled in, not a paragraph.
  const local = web && domains.length === 0 ? appOf(web)?.ports.find((p) => p.hostIp === "127.0.0.1") : undefined;
  const server = servers?.find((s) => s.id === web?.deployments[0]?.serverId);
  const address = server?.address;
  if (local && address) {
    const port = String(local.hostPort);
    const tunnel = `ssh -N -L ${port}:127.0.0.1:${port} ${address}`;
    out.push({
      key: "local",
      icon: "lock",
      text: t`Answers on this server alone, on port ${port}, until it has a domain`,
      action: (
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
