// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  type CompanionCheck,
  DomainService,
  type Server,
  type WebsiteInfo,
  WebsiteService,
} from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useTunnelSwitchName } from "../apps/DomainNote";
import { DomainRecord, RecordRow, useSettled } from "../apps/DomainRecord";
import { formRefusal } from "../apps/domains";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { domainAdviceOf, forwardToOf } from "../servers/network";
import "../apps/Apps.css";
import "./Websites.css";

/** What CheckDomain says of a domain's companion (ADR 0070): its www name, or the bare domain for www. */
export type CompanionRead = {
  /** The companion's name, or empty for a name that has none. */
  companion: string;
  check: CompanionCheck | undefined;
  again: () => void;
  checking: boolean;
};

/**
 * The companion of a domain being typed, read in the same call as the domain's own DNS,
 * which a form beside it shares. A project's ID lets a shared wildcard that serves it count.
 */
export function useCompanion(server: string, domain: string, project = ""): CompanionRead {
  const asked = useSettled(domain);
  const allowed = useMay()(DomainService.method.checkDomain).allowed;
  const read = useQuery(
    DomainService.method.checkDomain,
    { serverId: server, domain: asked, ...(project ? { projectId: project } : {}) },
    { enabled: server !== "" && asked !== "" && allowed, retry: false },
  );
  const res = asked === "" ? undefined : read.data;
  return {
    companion: res?.companion ?? "",
    check: res?.companionCheck,
    again: () => void read.refetch(),
    checking: read.isFetching,
  };
}

/** Whether a route of env itself serves the companion already. */
function besideHeld(check: CompanionCheck | undefined, env: string): boolean {
  return !!check && check.heldBy !== "" && check.heldByEnvironmentId === env;
}

/** Whether anything serves the companion already, in any environment. */
export function heldAtAll(check: CompanionCheck | undefined): boolean {
  return !!check && check.heldBy !== "";
}

/** Whether another environment than env serves the companion already. */
export function heldElsewhere(check: CompanionCheck | undefined, env = ""): boolean {
  return !!check && check.heldBy !== "" && (env === "" || check.heldByEnvironmentId !== env);
}

/**
 * Whether the companion is on: as a person chose it for that name, or else as DNS says,
 * on when it answers as the domain does. Another name starts again from DNS. A domain
 * Dockwell publishes writes the companion's record too, so it starts on (ADR 0104).
 */
export function useCompanionChoice(
  read: CompanionRead,
  env = "",
  strict = false,
  publishing = false,
): [boolean, (on: boolean) => void] {
  const [choice, setChoice] = useState<{ name: string; on: boolean }>();
  // For an app, anything that serves it already, its own environment's routes too,
  // keeps it: the box never turns a working domain into a redirect (ADR 0070).
  const free = read.companion !== "" && !(strict ? heldAtAll(read.check) : heldElsewhere(read.check, env));
  const on = free && (choice?.name === read.companion ? choice.on : publishing || !!read.check?.answersAlike);
  return [on, (next) => setChoice({ name: read.companion, on: next })];
}

/**
 * Where the companion stands, in a line or a record: who serves it already, with the way
 * there; the wildcard whose certificate covers it; or, while it doesn't answer as the
 * domain does, the record to make, or the tunnel to add it to.
 */
function CompanionState({
  server,
  read,
  tunnel,
  env = "",
  strict = false,
  already = false,
  onLeave,
}: {
  server: Server;
  read: CompanionRead;
  tunnel: boolean;
  env?: string;
  strict?: boolean;
  already?: boolean;
  onLeave: () => void;
}) {
  const c = read.check;
  const name = read.companion;
  if (!c) return null;
  if (strict && already) {
    return (
      <p className="website-companion-said">
        <span>
          <Trans>{name} redirects here already</Trans>
        </span>
      </p>
    );
  }
  // Served beside it in its own environment: what serves it, in words, and the way there.
  if (strict && heldAtAll(c) && !heldElsewhere(c, env)) {
    const what = c.heldFor;
    return (
      <p className="website-companion-said">
        <span>
          {what ? (
            <Trans>
              {name} is served by {what} here
            </Trans>
          ) : (
            <Trans>{name} is served here already</Trans>
          )}
        </span>
        {" · "}
        <Link to="/domains" search={{ q: name }} onClick={onLeave}>
          <Trans>Open</Trans>
        </Link>
      </p>
    );
  }
  if (heldElsewhere(c, env)) {
    const by = c.heldBy;
    return (
      <p className="website-companion-said">
        <span>
          <Trans>
            {name} is served by {by}
          </Trans>
        </span>
        {c.heldByEnvironmentId ? (
          <>
            {" · "}
            <Link to="/domains" search={{ q: name }} onClick={onLeave}>
              <Trans>Open</Trans>
            </Link>
          </>
        ) : null}
      </p>
    );
  }
  const covering = c.wildcard;
  const wildcard = covering ? (
    <p className="website-companion-said">
      <Trans>The certificate for {covering} covers it.</Trans>
    </p>
  ) : null;
  if (c.answersAlike) return wildcard;
  if (tunnel) {
    return (
      <p className="website-companion-said">
        <Trans>Add {name} to your tunnel too.</Trans>
      </p>
    );
  }
  const advice = domainAdviceOf(server.network, forwardToOf(server));
  if (advice.lead !== "record") return wildcard;
  return (
    <div className="website-companion-records">
      {advice.records.map((record) => (
        <RecordRow
          key={record.address}
          domain={name}
          address={record.address}
          read={{ check: c.check, again: read.again, checking: read.checking }}
        />
      ))}
      {wildcard}
    </div>
  );
}

/**
 * "Also answer on www.example.com" for a site at example.com, or the other way round:
 * checked when the companion answers as the domain does, off with who has it when another
 * environment serves it, and otherwise with the record to make beside it.
 */
export function CompanionChoice({
  server,
  read,
  on,
  onChange,
  tunnel,
  env = "",
  strict = false,
  already = false,
  onLeave,
}: {
  server: Server | undefined;
  read: CompanionRead;
  on: boolean;
  onChange: (on: boolean) => void;
  tunnel: boolean;
  /** The environment it's for, by ID, whose own routes don't count as another's. */
  env?: string;
  /** Anything that serves it already keeps it, its own environment's routes too, as for an app's domain. */
  strict?: boolean;
  /** It redirects to the domain already, so there's nothing to add. */
  already?: boolean;
  onLeave: () => void;
}) {
  if (!server || read.companion === "") return null;
  const name = read.companion;
  return (
    <div className="website-companion">
      <label className="app-check">
        <input
          type="checkbox"
          checked={on}
          disabled={strict ? heldAtAll(read.check) || already : heldElsewhere(read.check, env)}
          onChange={(event) => onChange(event.target.checked)}
        />
        <span>
          <Trans>
            Also answer on <code>{name}</code>
          </Trans>
        </span>
      </label>
      <CompanionState
        server={server}
        read={read}
        tunnel={tunnel}
        env={env}
        strict={strict}
        already={already}
        onLeave={onLeave}
      />
    </div>
  );
}

/** A website's other domains on its Domain panel: each that redirects here, and the ways to add, swap and move. */
export function OtherDomains({
  site,
  server,
  tunnel,
}: {
  site: WebsiteInfo;
  server: Server | undefined;
  tunnel: boolean;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay()(WebsiteService.method.updateWebsite, { project: site.id });
  const [asking, setAsking] = useState<"another" | "change">();
  const read = useCompanion(server?.id ?? "", site.domain, site.id);
  const list = site.spec?.redirectFrom ?? [];
  const companion = read.companion;
  // A listed domain whose redirect a person pointed elsewhere says where it goes now:
  // the site's next update takes it out of the list, and leaves its redirect be.
  const routes = useQuery(DomainService.method.listDomains, { environment: site.environmentId }).data?.domains;
  const elsewhere = (from: string): string => {
    const to = routes?.find((d) => d.name === from)?.routes.find((r) => r.path === "" && r.redirect)?.redirect?.to;
    if (!to) return "";
    try {
      const u = new URL(to);
      return u.hostname === site.domain ? "" : u.host + u.pathname.replace(/\/$/, "");
    } catch {
      return "";
    }
  };
  const heldFor = read.check?.heldFor ?? "";
  const plan = (redirectFrom: string[], summary: string, domain = "") =>
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(WebsiteService, transport).updateWebsite({
          id: site.id,
          website: { serves: site.spec?.serves, redirectFrom },
          summary,
          ...(domain ? { domain } : {}),
        }),
    });
  if (!site.domain) return null;
  const main = site.domain;
  return (
    <div className="website-others">
      {list.length > 0 ? (
        <ul className="website-others-list" aria-label={t`Domains that redirect here`}>
          {list.map((from) => {
            const away = elsewhere(from);
            return (
              <li key={from}>
                <code className="website-others-domain">{from}</code>
                <span className="website-others-word">
                  {away ? <Trans>Redirects to {away} now</Trans> : <Trans>Redirects here</Trans>}
                </span>
                <Button
                  size="sm"
                  variant="ghost"
                  icon="trash"
                  aria-label={t`Stop sending ${from} here`}
                  {...may.off}
                  onClick={() =>
                    plan(
                      list.filter((d) => d !== from),
                      t`Stop sending ${from} to ${main}`,
                    )
                  }
                />
              </li>
            );
          })}
        </ul>
      ) : null}
      {/* Served beside the site by a route its list didn't write: who has it, and the way there. */}
      {companion !== "" && !list.includes(companion) && besideHeld(read.check, site.environmentId) ? (
        <p className="website-companion-said">
          <span>
            {heldFor ? (
              <Trans>
                {companion} is served by {heldFor} here
              </Trans>
            ) : (
              <Trans>{companion} is served here already</Trans>
            )}
          </span>
          {" · "}
          <Link to="/domains" search={{ q: companion }}>
            <Trans>Open</Trans>
          </Link>
        </p>
      ) : companion !== "" && !list.includes(companion) && server ? (
        <div className="website-companion">
          <Button
            size="sm"
            icon="plus"
            {...may.off}
            disabled={may.off.disabled || heldElsewhere(read.check, site.environmentId)}
            onClick={() => plan([...list, companion], t`Send ${companion} to ${main}`)}
          >
            <Trans>
              Also answer on <code>{companion}</code>
            </Trans>
          </Button>
          <CompanionState server={server} read={read} tunnel={tunnel} env={site.environmentId} onLeave={() => {}} />
        </div>
      ) : null}
      <div className="website-others-actions">
        {companion !== "" && list.includes(companion) ? (
          <Button
            size="sm"
            {...may.off}
            onClick={() =>
              plan(
                [...list.filter((d) => d !== companion), main],
                t`Make ${companion} the main domain of ${main}`,
                companion,
              )
            }
          >
            <Trans>
              Make <code>{companion}</code> the main domain
            </Trans>
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" {...may.off} onClick={() => setAsking("another")}>
          <Trans>Send another domain here</Trans>
        </Button>
        <Button size="sm" variant="ghost" {...may.off} onClick={() => setAsking("change")}>
          <Trans>Change domain</Trans>
        </Button>
      </div>
      {asking ? (
        <DomainSheet site={site} server={server} tunnel={tunnel} mode={asking} onClose={() => setAsking(undefined)} />
      ) : null}
    </div>
  );
}

/**
 * A domain for a website: another that redirects to it, as an old one, or the one it
 * answers at now, which offers to keep the old one sending here. Each shows its DNS
 * record as the record arrives, and goes to a plan.
 */
function DomainSheet({
  site,
  server,
  tunnel,
  mode,
  onClose,
}: {
  site: WebsiteInfo;
  server: Server | undefined;
  tunnel: boolean;
  mode: "another" | "change";
  onClose: () => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const sheet = useChangeSheet();
  const switchName = useTunnelSwitchName();
  const [domain, setDomain] = useState("");
  const [keepOld, setKeepOld] = useState(true);
  const [error, setError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const field = useRef<HTMLInputElement>(null);
  const typed = domain.trim().toLowerCase();
  const main = site.domain;
  const list = site.spec?.redirectFrom ?? [];

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    if (typed === "" || /\s/.test(typed)) {
      setError(t`Name the domain, such as example.org.`);
      field.current?.focus();
      return;
    }
    const request =
      mode === "another"
        ? { redirectFrom: [...list, typed], summary: t`Send ${typed} to ${main}` }
        : {
            redirectFrom: keepOld && !list.includes(main) ? [...list, main] : list,
            summary: t`Move the website ${main} to ${typed}`,
            domain: typed,
          };
    setBusy(true);
    let change: Change | undefined;
    try {
      change = (
        await createClient(WebsiteService, transport).updateWebsite({
          id: site.id,
          website: { serves: site.spec?.serves, redirectFrom: request.redirectFrom },
          summary: request.summary,
          ...("domain" in request ? { domain: request.domain } : {}),
        })
      ).change;
    } catch (err) {
      const said = formRefusal(sentence(err), typed);
      if (reasonOf(err) === "invalid" || reasonOf(err) === "domain_taken") setError(said);
      else setFailure(said);
      setBusy(false);
      return;
    }
    setBusy(false);
    onClose();
    sheet.open({ kind: "plan", make: () => Promise.resolve({ change }) });
  };

  return (
    <Sheet
      open
      initialFocusRef={field}
      title={mode === "another" ? t`Send another domain here` : t`Change domain`}
      subtitle={mode === "another" ? t`It redirects to ${main}, keeping the path` : t`${main} answers there instead`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
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
        <button type="submit" hidden />
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        <TextField
          ref={field}
          label={mode === "another" ? t`Domain` : t`New domain`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder="example.org"
          value={domain}
          error={error}
          onChange={(event) => {
            setDomain(event.target.value);
            setError(undefined);
          }}
        />
        <DomainRecord server={server} domain={typed} tunnel={tunnel} tunnelName={switchName} onLeave={onClose} />
        {mode === "change" && !list.includes(main) ? (
          <label className="app-check">
            <input type="checkbox" checked={keepOld} onChange={(event) => setKeepOld(event.target.checked)} />
            <span>
              <Trans>
                Send <code>{main}</code> here
              </Trans>
            </span>
          </label>
        ) : null}
      </form>
    </Sheet>
  );
}
