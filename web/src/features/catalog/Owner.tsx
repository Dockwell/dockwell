// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  CatalogOwnerState,
  CatalogService,
  ChangeService,
  type Entry,
  type ServiceInfo,
  StepUpAction,
  VariableService,
} from "@dockwell/api";
import { Button, Callout, ConnectionCard, type ConnectionItem, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { appOf } from "../apps/app";
import { useAppDomains } from "../apps/domains";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedInUser } from "../auth/useAuthState";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useReveals } from "../variables/useReveal";
import { OpenAppButton } from "./access";

/** How making an install's owner stands, as its page shows it (ADR 0105). */
export type OwnerStatus = "email" | "making" | "failed" | "held" | "made";

/**
 * What an install's page shows of its owner: waiting for an email, being made,
 * failed, made, or held with no owner Dockwell makes, as an app whose first
 * visitor makes its owner. Undefined where there's nothing to say: no owner and
 * no hold, or an install a person opened.
 */
export function ownerStatus(install: CatalogInstall | undefined): OwnerStatus | undefined {
  switch (install?.owner?.state) {
    case CatalogOwnerState.WAITING_EMAIL:
      return "email";
    case CatalogOwnerState.WAITING:
      return "making";
    case CatalogOwnerState.FAILED:
      return "failed";
    case CatalogOwnerState.MADE:
      return "made";
  }
  return install?.held ? "held" : undefined;
}

/**
 * An install's owner state in a word or two, for a card or a row that names the
 * install: undefined where it's made or there's none, as a made owner needs no word.
 */
export function useOwnerWords(): (install: CatalogInstall) => string | undefined {
  const { t } = useLingui();
  return (install) => {
    switch (ownerStatus(install)) {
      case "email":
        return t`Needs its owner's email`;
      case "making":
        return t`Making its owner`;
      case "failed":
        return t`Owner not made`;
      case "held":
        return t`Held`;
      default:
        return undefined;
    }
  };
}

/** The install list, read again every few seconds while an owner is on its way or its install is held. */
export function useInstallsWhileMaking(making: boolean) {
  return useQuery(CatalogService.method.listCatalogInstalls, {}, { refetchInterval: making ? 5_000 : false });
}

/**
 * The one state an install's owner is in, as a callout with its action, on each of
 * its services' pages: waiting for its email, being made, couldn't be made, or held
 * until a person sets it up. A made owner is a card of its own (OwnerCard).
 */
export function OwnerCallout({
  service,
  install,
  entry,
}: {
  service: ServiceInfo;
  install: CatalogInstall;
  entry: Entry | undefined;
}) {
  const status = ownerStatus(install);
  // Read again while it's held, as what lifts a hold is observed after the change that asks.
  useInstallsWhileMaking(status !== undefined && status !== "made");
  const [byHand, setByHand] = useState(false);
  const app = entry?.name || install.name || install.pin?.entry || "";
  switch (status) {
    case "email":
      return <OwnerEmail install={install} app={app} />;
    case "making":
      return <OwnerMaking install={install} app={app} entry={entry} />;
    case "failed":
      // Set up by hand through its setup link, as its done reads, it's theirs to open.
      return byHand || install.owner?.setUp ? (
        <Held service={service} install={install} app={app} />
      ) : (
        <OwnerFailed install={install} app={app} onByHand={() => setByHand(true)} />
      );
    case "held":
      return <Held service={service} install={install} app={app} />;
    default:
      return null;
  }
}

/** Plans opening a held install to everyone, which a person applies with a passkey. */
function useOpen(install: CatalogInstall) {
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay()(CatalogService.method.openCatalogInstall, { project: install.projectId });
  const open = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(CatalogService, transport).openCatalogInstall({ id: install.projectId }),
    });
  return { open, may };
}

/**
 * Held: its domains answer that it isn't open yet, and its ports listen on its server
 * alone, so only people in this project reach it, through the panel, until a person
 * opens it to everyone.
 */
function Held({ service, install, app }: { service: ServiceInfo; install: CatalogInstall; app: string }) {
  const { t } = useLingui();
  const { open, may } = useOpen(install);
  const held = useAppDomains(service).domains.filter((d) => d.routes.some((r) => r.held));
  const domains = held.map((d) => d.name);
  // A setup link needs a domain every held route of which a browser reaches over
  // HTTPS, where its cookie is kept, as the server's rule says (ADR 0105; the review's M6).
  const secureDomain = held.find((d) => d.routes.filter((r) => r.held).every(overHTTPS));
  const secure = secureDomain?.name;
  const web = install.webServiceId === service.id;
  // The port that serves its pages, which the control plane works out (#791).
  const port = service.webPort || appOf(service)?.ports.find((p) => p.held)?.containerPort;
  const owned = install.owner !== undefined;
  const link = useSetupLink(install);
  // When the domain's link ends, as its routes say, so any tab counts it down.
  const routesUntil = Math.max(0, ...(secureDomain?.routes ?? []).map((r) => Number(r.setupUntil)));
  const left = useMinutesLeft(Math.max(routesUntil, link.until));
  if (install.owner?.setUp) {
    return (
      <Callout
        className="catalog-owner-callout"
        tone="healthy"
        title={t`${app} is set up`}
        actions={
          <Button variant="primary" size="sm" onClick={open} {...may.off}>
            <Trans>Open it to everyone</Trans>
          </Button>
        }
      >
        <Trans>Only people in this project reach it until you open it to everyone.</Trans>
      </Callout>
    );
  }
  return (
    <Callout
      className="catalog-owner-callout"
      tone="warning"
      title={owned ? t`Held until its owner account exists` : t`Held until you set it up`}
      actions={
        <>
          {secure ? (
            <Button
              variant="primary"
              size="sm"
              icon="external"
              loading={link.busy}
              onClick={() => void link.make(secure)}
              {...link.may.off}
            >
              {left > 0 ? <Trans>New setup link</Trans> : <Trans>Set it up at {secure}</Trans>}
            </Button>
          ) : web ? (
            <OpenAppButton
              serviceId={service.id}
              environment={install.environmentId}
              app={app}
              {...(port ? { port } : {})}
            />
          ) : null}
          <Button variant="secondary" size="sm" onClick={open} {...may.off}>
            <Trans>Open it to everyone</Trans>
          </Button>
        </>
      }
    >
      {link.problem ? (
        link.problem
      ) : left > 0 && secure ? (
        <Trans>
          Your setup link to {secure} works for {left} min. Everyone else finds it isn't open yet.
        </Trans>
      ) : domains.length > 1 ? (
        <Trans>
          {domains[0]} and {domains.length - 1} more answer that it isn't open yet, so only people in this project reach
          it.
        </Trans>
      ) : domains.length === 1 ? (
        <Trans>{domains[0]} answers that it isn't open yet, so only people in this project reach it.</Trans>
      ) : (
        <Trans>Only people in this project reach it, through the panel.</Trans>
      )}
    </Callout>
  );
}

/**
 * Whether a browser reaches a route over HTTPS, as the server's route.PlainAtTheBrowser
 * says the other way round: a certificate here, or plain HTTP behind a proxy it names
 * and that answers no one else.
 */
export function overHTTPS(r: { tls: string; plainForAnyone: boolean; trustedProxies: readonly string[] }): boolean {
  return !(r.tls === "off" && (r.plainForAnyone || r.trustedProxies.length === 0));
}

/**
 * A held install's setup link: making one, with a passkey, opens it in a tab of its
 * own once the server has it; until says when the one this page made ends, in Unix
 * seconds, until the domain's routes say it.
 */
function useSetupLink(install: CatalogInstall) {
  const transport = useTransport();
  const queries = useQueryClient();
  const explain = useAuthError();
  const may = useMay()(CatalogService.method.setUpCatalogInstall, { project: install.projectId });
  const [until, setUntil] = useState(0);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const make = async (domain: string) => {
    setBusy(true);
    setProblem(undefined);
    // Opened before the passkey, as a browser lets a click open a tab only at once.
    const tab = window.open("", "_blank");
    try {
      const client = createClient(CatalogService, transport);
      const res = await withStepUpIfAsked(transport, StepUpAction.SET_UP_CATALOG_INSTALL, install.projectId, (proof) =>
        client.setUpCatalogInstall({ id: install.projectId, domain, proof }),
      );
      if (tab) {
        tab.opener = null;
        tab.location.href = res.url;
      } else {
        window.open(res.url, "_blank", "noopener,noreferrer");
      }
      setUntil(Number(res.until?.seconds ?? 0n));
      await queries.invalidateQueries();
    } catch (err) {
      tab?.close();
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };
  return { make, busy, problem, may, until };
}

/**
 * Whole minutes left until a time in Unix seconds, read again each minute; 0 once
 * it's past. A link lasts an hour at most, so it never reads more than 60. The clock
 * is read again whenever the end moves, so a page open before the link was made
 * doesn't count from when it opened.
 */
function useMinutesLeft(until: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    setNow(Date.now());
    if (until * 1000 <= Date.now()) return;
    const tick = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(tick);
  }, [until]);
  return Math.min(60, Math.max(0, Math.ceil((until * 1000 - now) / 60_000)));
}

/** Being made: as whom, once its service runs, and held until then. */
function OwnerMaking({ install, app, entry }: { install: CatalogInstall; app: string; entry: Entry | undefined }) {
  const { t } = useLingui();
  const who = install.owner?.email || install.owner?.user || "";
  const runs = entry?.owner?.service || "";
  return (
    <Callout className="catalog-owner-callout" tone="info" shape="spinner" title={t`Making ${app}'s owner account`}>
      {runs ? (
        <Trans>
          As {who}, once {runs} runs. Until then, only people in this project reach it.
        </Trans>
      ) : (
        <Trans>As {who}. Until then, only people in this project reach it.</Trans>
      )}
    </Callout>
  );
}

/** Couldn't be made: what the app said, scrubbed, and the two ways on. */
function OwnerFailed({ install, app, onByHand }: { install: CatalogInstall; app: string; onByHand: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const queries = useQueryClient();
  const explain = useAuthError();
  const may = useMay()(CatalogService.method.retryCatalogOwner, { project: install.projectId });
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const retry = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(CatalogService, transport).retryCatalogOwner({ id: install.projectId });
      await queries.invalidateQueries();
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };
  // What the app said, without a shell's quoting, and where it's long, its step and
  // its last words, with the whole of it a step away (the review's L5).
  const said = (install.owner?.error ?? "").replaceAll('\\"', '"');
  const step = said.match(/^Step \d+: /)?.[0] ?? "";
  const short = said.length > 120 && said.includes(": ") ? step + said.slice(said.lastIndexOf(": ") + 2) : said;
  const [whole, setWhole] = useState(false);
  return (
    <Callout
      className="catalog-owner-callout"
      tone="critical"
      title={t`Couldn't make ${app}'s owner`}
      actions={
        <>
          <Button variant="primary" size="sm" loading={busy} onClick={() => void retry()} {...may.off}>
            <Trans>Try again</Trans>
          </Button>
          <Button variant="secondary" size="sm" onClick={onByHand}>
            <Trans>Set it up yourself</Trans>
          </Button>
        </>
      }
    >
      {problem ??
        (said ? (
          <>
            <code className="catalog-owner-said">{whole ? said : short}</code>{" "}
            {short !== said ? (
              <Button variant="ghost" size="sm" aria-expanded={whole} onClick={() => setWhole(!whole)}>
                {whole ? <Trans context="a long text, shown whole">Less</Trans> : <Trans>All of it</Trans>}
              </Button>
            ) : null}
          </>
        ) : (
          <Trans>It's held until then.</Trans>
        ))}
    </Callout>
  );
}

/**
 * Waits for its owner's email, as an install by a token without one does: the
 * person's own, with another a step away, and the plan that gives it.
 */
function OwnerEmail({ install, app }: { install: CatalogInstall; app: string }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const mine = useSignedInUser()?.email ?? "";
  const may = useMay()(CatalogService.method.setCatalogOwnerEmail, { project: install.projectId });
  const [other, setOther] = useState(mine === "");
  const [typed, setTyped] = useState("");
  const email = other ? typed.trim() : mine;
  const give = (e?: FormEvent) => {
    e?.preventDefault();
    if (!email) return;
    sheet.open({
      kind: "plan",
      make: () => createClient(CatalogService, transport).setCatalogOwnerEmail({ id: install.projectId, email }),
    });
  };
  return (
    <Callout
      className="catalog-owner-callout"
      tone="warning"
      title={t`${app} waits for its owner's email`}
      actions={
        other ? (
          <Button variant="primary" size="sm" onClick={() => give()} disabled={!email} {...may.off}>
            <Trans>Make the owner</Trans>
          </Button>
        ) : (
          <>
            <Button variant="primary" size="sm" onClick={() => give()} {...may.off}>
              <Trans>Make it as {mine}</Trans>
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setOther(true)}>
              <Trans>Another email</Trans>
            </Button>
          </>
        )
      }
    >
      {other ? (
        <form className="catalog-owner-email" onSubmit={give}>
          <TextField
            label={t`Owner's email`}
            type="email"
            autoComplete="email"
            value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)}
            placeholder="you@example.com"
          />
        </form>
      ) : (
        <Trans>It's held until Dockwell makes its owner account.</Trans>
      )}
    </Callout>
  );
}

/**
 * A new password for a made owner, with a passkey: Dockwell sets it in the app by
 * the entry's reset steps, and keeps the one that signs in (ADR 0105).
 */
function useNewPassword(install: CatalogInstall) {
  const transport = useTransport();
  const queries = useQueryClient();
  const explain = useAuthError();
  const may = useMay()(CatalogService.method.resetCatalogOwnerPassword, { project: install.projectId });
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const make = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      const client = createClient(CatalogService, transport);
      await withStepUpIfAsked(transport, StepUpAction.RESET_CATALOG_OWNER_PASSWORD, install.projectId, (proof) =>
        client.resetCatalogOwnerPassword({ id: install.projectId, proof }),
      );
      await queries.invalidateQueries();
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };
  return { make, busy, problem, may };
}

/**
 * The owner Dockwell made: who, to copy, and the password, which a passkey reveals
 * in place, on the page of what a person opens. Where the entry says how, a new
 * password is a button away; where it doesn't, a person who changed it in the app
 * forgets the copy Dockwell keeps, by a plan (ADR 0105).
 */
export function OwnerCard({ entry, install }: { entry: Entry; install: CatalogInstall }) {
  const { t } = useLingui();
  const environment = install.environmentId;
  const secretName = entry.owner?.passwordInput ?? "";
  const variables = useQuery(VariableService.method.listVariables, { environment }, { enabled: secretName !== "" });
  const secretId = variables.data?.variables.find((v) => v.secret && v.name === secretName)?.id ?? "";
  const reveals = useReveals();
  const shown = secretId ? reveals.get(secretId) : {};
  const mayReveal = useMay()(VariableService.method.revealSecret, { environment });
  const mayPropose = useMay()(ChangeService.method.proposeChange, { environment });
  const sheet = useChangeSheet();
  const fresh = useNewPassword(install);
  const owner = install.owner;
  const rotating = owner?.rotating ?? false;
  // Read again while a new password is being set, as its outcome is observed after.
  useInstallsWhileMaking(rotating);
  const wasRotating = useRef(rotating);
  useEffect(() => {
    // The one that signs in now may be the new one, so what was revealed goes.
    if (wasRotating.current && !rotating && secretId) reveals.hide(secretId);
    wasRotating.current = rotating;
  }, [rotating, secretId, reveals]);
  if (owner?.state !== CatalogOwnerState.MADE) return null;
  const app = entry.name;
  const resettable = (entry.owner?.resetSteps.length ?? 0) > 0;
  const items: ConnectionItem[] = [
    ...(owner.email ? [{ label: t`Email`, value: owner.email }] : []),
    ...(owner.user && owner.user !== owner.email ? [{ label: t`User`, value: owner.user }] : []),
  ];
  const forget = () =>
    sheet.open({
      kind: "propose",
      summary: t`Forget ${app}'s owner password`,
      edits: [{ resource: { kind: "secret", scope: environment, name: secretName }, delete: true }],
    });
  let note: ReactNode;
  if (rotating) {
    note = <Trans>Setting a new password in {app}. The current one works until then.</Trans>;
  } else if (fresh.problem) {
    note = fresh.problem;
  } else if (owner.error) {
    note = t`${app} kept its password. ${owner.error}`;
  } else if (!resettable && secretId) {
    note = (
      <Trans>
        Changed it in {app}?{" "}
        <Button variant="ghost" size="sm" onClick={forget} {...mayPropose.off}>
          Forget it
        </Button>
      </Trans>
    );
  }
  return (
    <ConnectionCard
      title={t`Owner`}
      className="catalog-sign-in"
      items={items}
      actions={
        resettable && secretId ? (
          <Button
            size="sm"
            icon="key"
            loading={fresh.busy || rotating}
            {...fresh.may.off}
            onClick={() => void fresh.make()}
          >
            {rotating ? <Trans>Setting a new password</Trans> : <Trans>Make a new password</Trans>}
          </Button>
        ) : undefined
      }
      note={note}
      secret={
        secretName
          ? {
              label: t`Password`,
              value: shown.value,
              busy: shown.busy ?? false,
              problem: shown.problem ?? (variables.error ? sentence(variables.error) : undefined),
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
