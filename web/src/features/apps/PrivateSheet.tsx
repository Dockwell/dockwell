// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  AuthService,
  ChangeState,
  type Domain,
  type DomainRoute,
  Protocol,
  type ServiceInfo,
  StrangerOutcome,
} from "@dockwell/api";
import { Button, Callout, CopyButton, cx, Icon, PHONE_QUERY, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { onApple } from "../../lib/platform";
import type { Offer } from "../auth/may";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { appOf } from "./app";
import { routeWhere } from "./domains";
import "./Apps.css";
import "./Private.css";

/** The letters a password Dockwell makes is written in: none a person mixes up when reading it out. */
const PASSWORD_LETTERS = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";

/** How long a password Dockwell makes is: far past guessing, with no limit on tries. */
const PASSWORD_LENGTH = 24;

/** A password no one chose, from the browser's own randomness, with no letter likelier than another. */
export function makePassword(): string {
  const out: string[] = [];
  const limit = 256 - (256 % PASSWORD_LETTERS.length);
  while (out.length < PASSWORD_LENGTH) {
    for (const byte of crypto.getRandomValues(new Uint8Array(PASSWORD_LENGTH * 2))) {
      if (byte < limit && out.length < PASSWORD_LENGTH)
        out.push(PASSWORD_LETTERS[byte % PASSWORD_LETTERS.length] ?? "");
    }
  }
  return out.join("");
}

/** An IPv6 address as its eight groups, or undefined for what isn't one. */
function groupsOf(address: string): string[] | undefined {
  const [head = "", tail, more] = address.split("::");
  if (more !== undefined) return undefined;
  const front = head === "" ? [] : head.split(":");
  const back = tail === undefined || tail === "" ? [] : tail.split(":");
  const fill = tail === undefined ? 0 : 8 - front.length - back.length;
  const groups = [...front, ...Array<string>(Math.max(fill, 0)).fill("0"), ...back];
  return groups.length === 8 && groups.every((g) => /^[0-9a-f]{1,4}$/i.test(g)) ? groups : undefined;
}

/**
 * What the list can offer of your own address, as the control plane saw your request:
 * an IPv4 address alone, and for IPv6 its /64 first, as a browser's IPv6 address changes
 * every day or so within it. Loopback, as through the SSH tunnel, is nothing to offer.
 */
export function ownEntries(address: string): string[] {
  const bare = address.replace(/%.*$/, "").replace(/^::ffff:(?=\d+\.)/i, "");
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(bare)) return bare.startsWith("127.") ? [] : [bare];
  const groups = groupsOf(bare);
  if (!groups || groups.slice(0, 7).every((g) => /^0+$/.test(g))) return [];
  return [`${canonical([...groups.slice(0, 4), "0", "0", "0", "0"])}/64`, canonical(groups)];
}

/** Eight IPv6 groups written as RFC 5952 has it, as the server writes a list back: the longest run of zeros, if two or more, as "::". */
function canonical(groups: readonly string[]): string {
  const short = groups.map((g) => g.toLowerCase().replace(/^0+(?=.)/, ""));
  let [at, length] = [-1, 1];
  for (let i = 0; i < short.length; ) {
    let j = i;
    while (j < short.length && short[j] === "0") j++;
    if (j - i > length) [at, length] = [i, j - i];
    i = j === i ? i + 1 : j;
  }
  if (at < 0) return short.join(":");
  return `${short.slice(0, at).join(":")}::${short.slice(at + length).join(":")}`;
}

/** Whether something typed has the shape of an address or a range. The server has the rules, and says what it won't take. */
export function looksLikeEntry(typed: string): boolean {
  const [address = "", bits, more] = typed.split("/");
  if (more !== undefined || (bits !== undefined && !/^\d{1,3}$/.test(bits))) return false;
  return /^\d{1,3}(\.\d{1,3}){3}$/.test(address) || groupsOf(address.replace(/%.*$/, "")) !== undefined;
}

/** Why a route can't keep strangers out by address, in the ways ADR 0087 refuses. */
export function listBlocked(route: DomainRoute): "cloudflare" | "tunnel" | undefined {
  if (route.trustedProxies.includes("cloudflare")) return "cloudflare";
  if (route.tls === "off" && route.trustedProxies.length === 0 && !route.plainForAnyone) return "tunnel";
  return undefined;
}

/**
 * Whether a browser may reach a route over plain HTTP, as the server's rule says: no
 * certificate here, and either anyone answered or nothing named in front whose edge
 * serves HTTPS. A password isn't sent there.
 */
export function plainAtTheBrowser(route: DomainRoute): boolean {
  return route.tls === "off" && (route.plainForAnyone || route.trustedProxies.length === 0);
}

/** A user of the route as the sheet has them: one it keeps, with their password as it is, or one with a password made here. */
interface UserRow {
  key: string;
  name: string;
  /** A password made here, shown until the plan applies, and never again. Undefined keeps theirs. */
  password: string | undefined;
}

/** A way into a private route's app around it: a port it publishes to anyone, or a public route to it. */
export interface OpenWay {
  key: string;
  port?: { number: number; udp: boolean };
  route?: { domain: Domain; route: DomainRoute };
}

/**
 * The ways into a route's app that don't go through the route, which keep no stranger
 * out whatever the route says: each port its app publishes on every address of its
 * server, and each other route to it that lets anyone in (#606's review, M2).
 */
export function openWaysOf(
  domain: Domain,
  route: DomainRoute,
  service: ServiceInfo | undefined,
  all: readonly Domain[],
): OpenWay[] {
  if (!isPrivate(route) || route.service === "") return [];
  const ports = (service ? (appOf(service)?.ports ?? []) : [])
    .filter((p) => p.hostIp === "" || p.hostIp === "0.0.0.0" || p.hostIp === "::")
    .map(
      (p): OpenWay => ({
        key: `port ${p.hostPort}/${p.protocol}`,
        port: { number: p.hostPort, udp: p.protocol === Protocol.UDP },
      }),
    );
  const routes = all
    .filter((d) => d.environmentId === domain.environmentId)
    .flatMap((d) =>
      d.routes
        .filter((r) => r.id !== route.id && r.service === route.service && !isPrivate(r))
        .map((r): OpenWay => ({ key: `route ${r.id}`, route: { domain: d, route: r } })),
    );
  return [...ports, ...routes];
}

/** One line of what a stranger gets: how it reads, and what it says. */
export interface StrangerLine {
  tone: "ok" | "warning" | "muted";
  text: string;
}

/** What a stranger gets from a route on each of its servers, as they last asked it. */
export function useStrangerWords(): (domain: Domain, route: DomainRoute) => StrangerLine[] {
  const { t } = useLingui();
  return (domain, route) =>
    domain.servers.flatMap((server): StrangerLine[] => {
      const check = server.status?.strangers.find((s) => s.path === route.path);
      if (!check) return [];
      const on = domain.servers.length > 1 ? server.serverName : "";
      switch (check.outcome) {
        case StrangerOutcome.KEPT_OUT:
          return [
            { tone: "ok", text: on ? t`On ${on}, a stranger gets 403 Forbidden` : t`A stranger gets 403 Forbidden` },
          ];
        case StrangerOutcome.ASKED_FOR_PASSWORD:
          return [
            {
              tone: "ok",
              text: on ? t`On ${on}, a stranger is asked for a password` : t`A stranger is asked for a password`,
            },
          ];
        case StrangerOutcome.LET_IN:
          return [{ tone: "warning", text: on ? t`On ${on}, a stranger gets in` : t`A stranger gets in` }];
        case StrangerOutcome.CANT_TELL:
          return [
            {
              tone: "muted",
              text: on ? t`${on} can't tell from where it asks` : t`Its server can't tell from where it asks`,
            },
          ];
        case StrangerOutcome.OTHER: {
          const said = check.said;
          return [{ tone: "muted", text: on ? t`On ${on}, a stranger gets ${said}` : t`A stranger gets ${said}` }];
        }
      }
      return [];
    });
}

/**
 * Who gets in to a route (ADR 0087): an address list, with yours offered first, and
 * users with a password Dockwell makes, shown once. What a stranger gets is what the
 * route's server last found, asking it as one. Applying it is a plan, as any change.
 */
export function PrivateSheet({
  domain,
  route,
  onClose,
  service,
  all = [],
  onOther,
}: {
  domain: Domain;
  route: DomainRoute;
  onClose: () => void;
  /** The app it sends to, for what it publishes around the route. */
  service?: ServiceInfo;
  /** Every domain there is, for the other routes to the same app. */
  all?: readonly Domain[];
  /** Opens this sheet for another route, by ID. */
  onOther?: (id: string) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const where = routeWhere(domain, route);
  const blocked = listBlocked(route);
  const plainHere = plainAtTheBrowser(route);
  const sessions = useQuery(AuthService.method.listSessions, {});
  const address = sessions.data?.sessions.find((s) => s.current)?.address ?? "";
  const own = ownEntries(address);
  const wasPrivate = route.allow.length + route.users.length > 0;
  const [listOn, setListOn] = useState(route.allow.length > 0 || (!wasPrivate && !blocked));
  const [entries, setEntries] = useState<string[]>(route.allow);
  const [listTouched, setListTouched] = useState(route.allow.length > 0);
  const [typed, setTyped] = useState("");
  const [typedError, setTypedError] = useState<string>();
  const [usersOn, setUsersOn] = useState(
    (route.users.length > 0 || (!wasPrivate && blocked !== undefined)) && !plainAtTheBrowser(route),
  );
  const [users, setUsers] = useState<UserRow[]>(() =>
    route.users.length > 0
      ? route.users.map((name) => ({ key: name, name, password: undefined }))
      : [{ key: "new-0", name: "admin", password: makePassword() }],
  );
  const [kept, setKept] = useState(false);
  const [plan, setPlan] = useState<Opening>();
  const added = useRef(1);
  const field = useRef<HTMLInputElement>(null);
  const strangers = useStrangerWords()(domain, route);
  const listId = useId();

  // Your address comes once the control plane says it, and goes first on a list you haven't touched.
  const first = own[0];
  useEffect(() => {
    if (!first || listTouched || wasPrivate) return;
    setEntries([first]);
    // A network is everyone at that office or on that line, so it comes with a password (#606's review, L3).
    if (first.includes("/") && !plainHere) setUsersOn(true);
  }, [first, listTouched, wasPrivate, plainHere]);
  // With no address of yours to offer, as through the SSH tunnel, a new private route starts with a password.
  const noOwn = sessions.isSuccess && own.length === 0;
  useEffect(() => {
    if (noOwn && !wasPrivate && !listTouched) {
      setListOn(false);
      if (!plainHere) setUsersOn(true);
    }
  }, [noOwn, wasPrivate, listTouched, plainHere]);

  // A password goes nowhere a browser would send it over plain HTTP.
  const asks = usersOn && !plainHere;
  const made = asks ? users.filter((u) => u.password !== undefined) : [];
  const keepsList = listOn && blocked === undefined;
  const names = users.map((u) => u.name.trim());
  const nameMissing = asks && names.some((n) => n === "");
  const listEmpty = keepsList && entries.length === 0;
  // Turning both off on a route that lets in anyone already changes nothing.
  const changes = wasPrivate || keepsList || asks;
  const ready = changes && !nameMissing && (made.length === 0 || kept) && !listEmpty;

  const addEntry = (entry: string) => {
    const value = entry.trim();
    if (value === "") return;
    if (!looksLikeEntry(value)) {
      setTypedError(t`Write an address or a range, such as 203.0.113.7 or 2001:db8::/64.`);
      return;
    }
    setListTouched(true);
    setEntries((was) => (was.includes(value) ? was : [...was, value]));
    setTyped("");
    setTypedError(undefined);
  };

  // Every route of the domain at once, as a domain is served one way or the other.
  const domainName = domain.name;
  const turnOnHTTPS = () =>
    setPlan({
      kind: "propose",
      summary: t`Turn on HTTPS for ${domainName}`,
      edits: domain.routes.map((r) => ({
        resource: { kind: "route", scope: domain.environmentId, name: r.name },
        spec: {
          kind: {
            case: "route" as const,
            value: {
              domain: domain.name,
              path: r.path,
              service: r.service,
              ...(r.hostPort ? { hostPort: r.hostPort } : { port: r.port }),
              trustedProxies: r.trustedProxies,
              held: r.held,
              ...(r.allow.length + r.users.length > 0
                ? { access: { allow: r.allow, users: r.users.map((name) => ({ name })) } }
                : {}),
            },
          },
        },
        ...(domain.servers.length === 1 ? { servers: [domain.servers[0]?.serverId ?? ""] } : {}),
      })),
      retryLabel: t`Edit and try again`,
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED || after?.state === ChangeState.APPLYING) onClose();
        else setPlan(undefined);
      },
    });

  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    if (!ready) return;
    const access = {
      allow: keepsList ? entries : [],
      users: asks ? users.map((u) => ({ name: u.name.trim(), password: u.password ?? "" })) : [],
    };
    const opens = access.allow.length + access.users.length === 0;
    setPlan({
      kind: "propose",
      summary: opens
        ? t`Let anyone in to ${where}`
        : wasPrivate
          ? t`Change who gets in to ${where}`
          : t`Make ${where} private`,
      edits: [
        {
          resource: { kind: "route", scope: domain.environmentId, name: route.name },
          spec: {
            kind: {
              case: "route",
              value: {
                domain: domain.name,
                path: route.path,
                service: route.service,
                ...(route.hostPort ? { hostPort: route.hostPort } : { port: route.port }),
                tls: route.tls,
                trustedProxies: route.trustedProxies,
                plainForAnyone: route.plainForAnyone,
                // An app not open yet stays so (ADR 0084): the sheet sends the whole route.
                held: route.held,
                ...(opens ? {} : { access }),
              },
            },
          },
          // Where it runs, as nothing here moves it: named where the domain's routes run on one server alone.
          ...(domain.servers.length === 1 ? { servers: [domain.servers[0]?.serverId ?? ""] } : {}),
        },
      ],
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      onClose: (after) => {
        // Applied, the form's work is done; anything else comes back to it, with what you chose.
        if (after?.state === ChangeState.APPLIED || after?.state === ChangeState.APPLYING) onClose();
        else setPlan(undefined);
      },
    });
  };

  const expected = keepsList
    ? t`Once applied, a stranger gets 403 Forbidden`
    : asks
      ? t`Once applied, a stranger is asked for a password`
      : t`Once applied, anyone who reaches it gets in`;

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={wasPrivate ? t`Who gets in to ${where}` : t`Make ${where} private`}
      subtitle={t`Keep strangers out of an app with no sign-in of its own`}
      onClose={onClose}
      className={cx("app-sheet", "private-sheet", phone && "is-phone")}
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
            disabled={!ready}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => submit()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="app-form private-form"
        onSubmit={submit}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) submit(event);
        }}
      >
        <section className="private-now" aria-label={t`What a stranger gets`}>
          <Icon name="shield" size={16} />
          <div>
            {strangers.length > 0 ? (
              strangers.map((s) => (
                <p key={s.text} className={cx("private-now-line", `is-${s.tone}`)}>
                  {s.text}
                </p>
              ))
            ) : (
              <p className="private-now-line is-muted">
                {wasPrivate ? (
                  <Trans>Its server hasn't checked yet</Trans>
                ) : (
                  <Trans>Now anyone who reaches it gets in</Trans>
                )}
              </p>
            )}
            <p className="private-now-next">{expected}</p>
          </div>
        </section>
        <OpenWays ways={openWaysOf(domain, route, service, all)} service={service} onOther={onOther} />

        <fieldset className="private-part">
          <label className="app-check private-toggle">
            <input
              type="checkbox"
              checked={keepsList}
              disabled={blocked !== undefined}
              onChange={(event) => setListOn(event.target.checked)}
            />
            <span>
              <Trans>Only these addresses</Trans>
            </span>
          </label>
          {blocked === "cloudflare" ? (
            <p className="private-why">{t`Anyone with a Cloudflare Worker can say who they are here, so ask for a password instead.`}</p>
          ) : blocked === "tunnel" ? (
            <p className="private-why">{t`Every visitor comes from your tunnel's address until the route names it as a proxy.`}</p>
          ) : null}
          {keepsList ? (
            <>
              <ul className="private-entries" id={listId} aria-label={t`Addresses let in`}>
                {entries.map((entry) => (
                  <li key={entry} className="private-entry">
                    <span className="private-entry-what">
                      <code>{entry}</code>
                      {own.includes(entry) ? (
                        <span className="private-entry-note">
                          {entry.includes("/") ? (
                            <Trans context="an address range on a list, that yours is in">your network</Trans>
                          ) : (
                            <Trans context="an address on a list, that is yours">you</Trans>
                          )}
                        </span>
                      ) : null}
                    </span>
                    <Button
                      variant="ghost"
                      size="sm"
                      icon="x"
                      aria-label={t`Take ${entry} off the list`}
                      onClick={() => {
                        setListTouched(true);
                        setEntries((was) => was.filter((e) => e !== entry));
                      }}
                    />
                  </li>
                ))}
              </ul>
              {own
                .filter((entry) => !entries.includes(entry))
                .map((entry) => (
                  <Button key={entry} variant="ghost" size="sm" icon="plus" onClick={() => addEntry(entry)}>
                    {entry.includes("/") ? t`Add your network, ${entry}` : t`Add your address, ${entry}`}
                  </Button>
                ))}
              {noOwn ? (
                <p className="private-why">{t`You reach the panel through this server, so your own address can't be offered.`}</p>
              ) : null}
              {listEmpty ? <p className="private-why">{t`Add an address, or turn the list off.`}</p> : null}
              <div className="private-add">
                <TextField
                  ref={field}
                  label={t`Add an address or a range`}
                  mono
                  autoCapitalize="none"
                  autoCorrect="off"
                  spellCheck={false}
                  value={typed}
                  error={typedError}
                  placeholder="203.0.113.0/24"
                  onChange={(event) => {
                    setTyped(event.target.value);
                    setTypedError(undefined);
                  }}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" && !event.metaKey && !event.ctrlKey) {
                      event.preventDefault();
                      addEntry(typed);
                    }
                  }}
                />
                <Button variant="secondary" onClick={() => addEntry(typed)} disabled={typed.trim() === ""}>
                  <Trans>Add</Trans>
                </Button>
              </div>
            </>
          ) : null}
        </fieldset>

        <fieldset className="private-part">
          <label className="app-check private-toggle">
            <input
              type="checkbox"
              checked={asks}
              disabled={plainHere}
              onChange={(event) => setUsersOn(event.target.checked)}
            />
            <span>
              <Trans>Ask for a password</Trans>
            </span>
          </label>
          {plainHere ? (
            <p className="private-why">
              {t`Visitors reach it over plain HTTP, so a password would cross the network in the clear.`}{" "}
              <button type="button" className="app-panel-link" onClick={turnOnHTTPS}>
                {t`Turn on HTTPS for this domain`}
              </button>
            </p>
          ) : null}
          {asks ? (
            <>
              {users.map((u) => {
                const who = u.name;
                return (
                  <div key={u.key} className="private-user">
                    {u.password === undefined ? (
                      <p className="private-user-kept">
                        <Icon name="key" size={14} />
                        <code>{u.name}</code>
                        <span>
                          <Trans>keeps their password</Trans>
                        </span>
                      </p>
                    ) : (
                      <TextField
                        label={t`User name`}
                        mono
                        autoCapitalize="none"
                        autoCorrect="off"
                        spellCheck={false}
                        value={u.name}
                        readOnly={route.users.includes(u.key)}
                        error={u.name.trim() === "" ? t`Name the user, such as admin.` : undefined}
                        onChange={(event) =>
                          setUsers((was) => was.map((o) => (o.key === u.key ? { ...o, name: event.target.value } : o)))
                        }
                      />
                    )}
                    {u.password !== undefined ? (
                      <div className="private-password">
                        <span className="private-password-label">{t`Password for ${who}`}</span>
                        <code className="private-password-value">{u.password}</code>
                        <CopyButton
                          text={u.password}
                          label={t`Copy the password`}
                          copiedLabel={t`Password copied`}
                          variant="secondary"
                        />
                      </div>
                    ) : null}
                    <div className="private-user-acts">
                      {u.password === undefined ? (
                        <Button
                          variant="ghost"
                          size="sm"
                          icon="refresh"
                          onClick={() => {
                            setKept(false);
                            setUsers((was) =>
                              was.map((o) => (o.key === u.key ? { ...o, password: makePassword() } : o)),
                            );
                          }}
                        >
                          {t`New password for ${who}`}
                        </Button>
                      ) : null}
                      {users.length > 1 ? (
                        <Button
                          variant="ghost"
                          size="sm"
                          icon="trash"
                          onClick={() => setUsers((was) => was.filter((o) => o.key !== u.key))}
                        >
                          {t`Remove ${who}`}
                        </Button>
                      ) : null}
                    </div>
                  </div>
                );
              })}
              <Button
                variant="ghost"
                size="sm"
                icon="plus"
                onClick={() => {
                  const key = `new-${added.current++}`;
                  setKept(false);
                  setUsers((was) => [...was, { key, name: "", password: makePassword() }]);
                }}
              >
                <Trans>Add a user</Trans>
              </Button>
              {made.length > 0 ? (
                <Callout tone="warning" title={t`Keep the password somewhere else`}>
                  <p className="private-saved-line">{t`Dockwell shows it only now, and keeps only its hash.`}</p>
                  <label className="private-saved">
                    <input type="checkbox" checked={kept} onChange={(event) => setKept(event.target.checked)} />
                    {made.length > 1 ? (
                      <Trans>I've kept a copy of each password</Trans>
                    ) : (
                      <Trans>I've kept a copy of the password</Trans>
                    )}
                  </label>
                </Callout>
              ) : null}
            </>
          ) : null}
        </fieldset>
      </form>
    </Sheet>
  );
}

/** Whether a route keeps anyone out. */
export function isPrivate(route: DomainRoute): boolean {
  return route.allow.length + route.users.length > 0;
}

/** The way from a route to who gets in to it, beside the way to open it. */
export function PrivateButton({
  domain,
  route,
  onOpen,
  off,
}: {
  domain: Domain;
  route: DomainRoute;
  onOpen: () => void;
  /** Off, with why, for someone whose role can't plan here (ADR 0059). */
  off?: Offer["off"];
}) {
  const { t } = useLingui();
  const where = routeWhere(domain, route);
  return (
    <Button
      variant="ghost"
      size="sm"
      icon="lock"
      className={cx("private-button", isPrivate(route) && "is-private")}
      aria-label={isPrivate(route) ? t`Who gets in to ${where}` : t`Make ${where} private`}
      title={isPrivate(route) ? t`Who gets in` : t`Make it private`}
      onClick={onOpen}
      {...off}
    />
  );
}

/** The ways in around a private route, each with what closes it. */
function OpenWays({
  ways,
  service,
  onOther,
}: {
  ways: readonly OpenWay[];
  service: ServiceInfo | undefined;
  onOther: ((id: string) => void) | undefined;
}) {
  const { t } = useLingui();
  if (ways.length === 0) return null;
  return (
    <Callout tone="warning" title={t`Other ways in let anyone reach this app`}>
      <ul className="private-ways">
        {ways.map((way) => {
          if (way.port) {
            const port = way.port.number;
            return (
              <li key={way.key}>
                <span>{way.port.udp ? t`UDP port ${port} on its server` : t`Port ${port} on its server`}</span>
                {service ? (
                  <Link
                    to="/apps/$serviceId/settings"
                    params={{ serviceId: service.id }}
                    hash="ports"
                    className="app-panel-link"
                  >
                    {t`Bind it to the server alone`}
                  </Link>
                ) : null}
              </li>
            );
          }
          const at = way.route;
          if (!at) return null;
          const where = routeWhere(at.domain, at.route);
          return (
            <li key={way.key}>
              <code>{where}</code>
              {onOther ? (
                <button type="button" className="app-panel-link" onClick={() => onOther(at.route.id)}>
                  {t`Make ${where} private too`}
                </button>
              ) : null}
            </li>
          );
        })}
      </ul>
    </Callout>
  );
}

/** A private route's line on a list: that it's private, and what a stranger got when its server last asked, or a way in around it. */
export function PrivateFact({
  domain,
  route,
  open = [],
}: {
  domain: Domain;
  route: DomainRoute;
  open?: readonly OpenWay[];
}) {
  const { t } = useLingui();
  const lines = useStrangerWords()(domain, route);
  if (!isPrivate(route)) return null;
  const around = open[0];
  if (around) {
    const port = around.port?.number ?? 0;
    const where = around.route ? routeWhere(around.route.domain, around.route.route) : "";
    return (
      <span className="private-fact is-warning">
        <Icon name="lock" size={12} />
        <span>{around.port ? t`Private here · open on port ${port}` : t`Private here · open at ${where}`}</span>
      </span>
    );
  }
  const warn = lines.some((line) => line.tone === "warning");
  const said = (lines.find((line) => line.tone === "warning") ?? lines[0])?.text;
  return (
    <span className={cx("private-fact", warn && "is-warning")}>
      <Icon name="lock" size={12} />
      <span>{said ? t`Private · ${said}` : t`Private`}</span>
    </span>
  );
}
