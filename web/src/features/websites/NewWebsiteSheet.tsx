// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type Server, WebsiteService } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  PHONE_QUERY,
  SelectField,
  Sheet,
  Switch,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useId, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useTunnelSwitchName } from "../apps/DomainNote";
import { DomainRecord } from "../apps/DomainRecord";
import { formRefusal, proxyList, refusedField } from "../apps/domains";
import { FrontFields, frontOf, type InFront } from "../apps/FrontFields";
import { validPort } from "../apps/form";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { type FleetState, useFleet } from "../servers/useFleet";
import "../apps/Apps.css";
import "./Websites.css";

/** Where a website sends its requests. */
export type SendsTo = "url" | "port";

/** The server a website goes on: the one chosen, or the only one there is. */
function serverOf(fleet: FleetState, id: string): Server | undefined {
  if (fleet.status === "connecting") return undefined;
  return fleet.servers.find((s) => s.id === id) ?? (fleet.servers.length === 1 ? fleet.servers[0] : undefined);
}

/** A URL a website can send to, as far as a form can tell before the server says. */
function looksLikeURL(written: string): boolean {
  return /^https?:\/\/\S+$/i.test(written.trim());
}

/** Which field a refusal of what a website sends to is about, by what the server says. */
function targetField(said: string): "url" | "port" | "" {
  if (/\bURL\b|http:\/\/|https:\/\/|host name|path|query/i.test(said)) return "url";
  if (/\bport\b/i.test(said) && !/\bURL\b/.test(said)) return "port";
  return "";
}

/**
 * New website: its domain, where it sends its requests, and what's in front of the
 * server, then the plan. A URL is reached by a small web server of its own, which needs
 * nothing a token couldn't do; a port on the server needs a person, which the plan says.
 */
export function NewWebsiteSheet({ onClose, onMade }: { onClose: () => void; onMade?: (domain: string) => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(WebsiteService, transport), [transport]);
  const fleet = useFleet();
  const servers = fleet.status === "connecting" ? [] : fleet.servers;
  const [server, setServer] = useState("");
  const [domain, setDomain] = useState("");
  const [sendsTo, setSendsTo] = useState<SendsTo>("url");
  const [url, setURL] = useState("");
  const [port, setPort] = useState("");
  const [targetHost, setTargetHost] = useState(false);
  const [noCertificate, setNoCertificate] = useState(false);
  const [inFront, setInFront] = useState<InFront>("tunnel");
  const [proxies, setProxies] = useState("");
  const [proxiesError, setProxiesError] = useState<string>();
  const [domainError, setDomainError] = useState<string>();
  const [urlError, setURLError] = useState<string>();
  const [portError, setPortError] = useState<string>();
  const [serverError, setServerError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  const form = useRef<HTMLFormElement>(null);
  const choiceName = useId();
  const switchName = useTunnelSwitchName();
  const typed = domain.trim().toLowerCase();
  const several = servers.length > 1;

  /** Takes you to the first field that was refused, ready to change, as New app does. */
  const revealError = () =>
    requestAnimationFrame(() => {
      const refused = form.current?.querySelector<HTMLElement>('[aria-invalid="true"]');
      refused?.focus();
      if (refused instanceof HTMLInputElement) refused.select();
    });

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the form comes back, and revealError is new each time.
  useEffect(() => {
    if (plan || !reveal.current) return;
    reveal.current = false;
    revealError();
  }, [plan]);

  /** Puts what the server refused at the field it's about, or above them all if it's about none of them. */
  const land = (raw: string, wanted: string) => {
    const said = formRefusal(raw, wanted);
    const at = refusedField(said, { domain: wanted, port, onServer: sendsTo === "port" });
    if (at === "domain") return setDomainError(said);
    if (at === "proxies" && noCertificate && inFront === "proxy") return setProxiesError(said);
    const target = targetField(said);
    if (target === "url" && sendsTo === "url") return setURLError(said);
    if ((target === "port" || at === "port") && sendsTo === "port") return setPortError(said);
    setFailure(said);
  };

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const domainOk = typed !== "" && !/\s/.test(typed);
    const urlOk = sendsTo !== "url" || looksLikeURL(url);
    const portOk = sendsTo !== "port" || validPort(port);
    const named = proxyList(proxies);
    const proxiesOk = !noCertificate || inFront !== "proxy" || named.length > 0;
    const serverOk = !several || server !== "";
    setDomainError(domainOk ? undefined : t`Name the domain, such as blog.example.com.`);
    setURLError(
      urlOk
        ? undefined
        : url.trim() === ""
          ? t`Give the URL to send to, such as https://backend.example.com.`
          : t`Start it with http:// or https://, such as https://backend.example.com.`,
    );
    setPortError(portOk ? undefined : t`Use a port from 1 to 65535.`);
    setProxiesError(proxiesOk ? undefined : t`Name what connects to this server, as an address or a range.`);
    setServerError(serverOk ? undefined : t`Choose the server it runs on.`);
    setFailure(undefined);
    if (!domainOk || !urlOk || !portOk || !proxiesOk || !serverOk) {
      revealError();
      return;
    }
    const front = frontOf(noCertificate, inFront, named);
    const website = {
      serves: {
        case: "proxy" as const,
        value: sendsTo === "port" ? { port: Number(port) } : { url: url.trim(), sendTargetHost: targetHost },
      },
    };
    setBusy(true);
    let change: Change | undefined;
    try {
      change = (
        await client.createWebsite({
          domain: typed,
          website,
          ...front,
          ...(server ? { servers: [server] } : {}),
        })
      ).change;
    } catch (err) {
      const said = sentence(err);
      // A server's agent refusing the route is about the route. What else fails isn't.
      if (reasonOf(err) === "invalid" || /^can't plan on /i.test(said)) land(said, typed);
      else setFailure(said);
      setBusy(false);
      revealError();
      return;
    }
    setBusy(false);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          land(after.error, typed);
          reveal.current = true;
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else {
          onMade?.(typed);
          onClose();
        }
      },
    });
  };

  const choices: { value: SendsTo; label: string; hint: string }[] = [
    {
      value: "url",
      label: t`A URL`,
      hint: t`Another site, or an address on your network, such as http://10.0.0.5:8080. A small web server of its own sends each request on.`,
    },
    {
      value: "port",
      label: t`A port on this server`,
      hint: t`Something listening on the server itself, such as port 3000. It sees every visitor as local, so you approve it in the plan.`,
    },
  ];

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`New website`}
      subtitle={
        !noCertificate
          ? t`People reach it at its domain, over HTTPS`
          : inFront === "anyone"
            ? t`People reach it at its domain, over plain HTTP`
            : t`People reach it at its domain, through what's in front of this server`
      }
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
        ref={form}
        className="app-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        {/* Lets Enter in a field do what the button in the sheet's footer does. */}
        <button type="submit" hidden />
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        <TextField
          ref={field}
          label={t`Domain`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder="blog.example.com"
          value={domain}
          error={domainError}
          hint={
            !noCertificate
              ? t`Point its DNS at this server, before or after. Its certificate comes by itself once it does.`
              : inFront === "anyone"
                ? t`Point its DNS at this server.`
                : t`Point its DNS at your tunnel or proxy.`
          }
          onChange={(event) => {
            setDomain(event.target.value);
            setDomainError(undefined);
          }}
        />
        <DomainRecord
          server={serverOf(fleet, server)}
          domain={typed}
          tunnel={noCertificate}
          tunnelName={switchName}
          onLeave={onClose}
        />
        <fieldset className="app-front website-sends">
          <legend className="dw-field-label">
            <Trans>Sends its requests to</Trans>
          </legend>
          {choices.map((choice) => (
            <label key={choice.value} className={cx("app-front-choice", sendsTo === choice.value && "is-on")}>
              <input
                type="radio"
                name={choiceName}
                checked={sendsTo === choice.value}
                onChange={() => {
                  setSendsTo(choice.value);
                  setFailure(undefined);
                }}
              />
              <span className="app-front-text">
                <span className="app-front-name">{choice.label}</span>
                <span className="app-front-hint">{choice.hint}</span>
              </span>
            </label>
          ))}
        </fieldset>
        {sendsTo === "url" ? (
          <>
            <TextField
              label={t`URL`}
              mono
              inputMode="url"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="https://backend.example.com"
              value={url}
              error={urlError}
              hint={t`With a path, such as /app, each request goes under it.`}
              onChange={(event) => {
                setURL(event.target.value);
                setURLError(undefined);
              }}
            />
            <Switch
              label={t`Send the URL's own name as Host`}
              description={t`For a site that serves several names, such as another site over HTTPS. Off, it gets this website's domain.`}
              checked={targetHost}
              onChange={setTargetHost}
            />
          </>
        ) : (
          <TextField
            label={t`Port on the server`}
            mono
            inputMode="numeric"
            placeholder="3000"
            value={port}
            error={portError}
            hint={t`What listens on the server itself, on 127.0.0.1 or every address. Port 80 is the proxy's.`}
            onChange={(event) => {
              setPort(event.target.value);
              setPortError(undefined);
            }}
          />
        )}
        {several ? (
          <SelectField
            label={t`Server`}
            hint={t`A website runs on one server, as its certificate comes to the one its DNS points at.`}
            error={serverError}
            value={server}
            options={[
              { value: "", label: t`Choose a server` },
              ...servers.map((s) => ({ value: s.id, label: s.name })),
            ]}
            onChange={(event) => {
              setServer(event.target.value);
              setServerError(undefined);
            }}
          />
        ) : null}
        <FrontFields
          behind="website"
          noCertificate={noCertificate}
          onNoCertificate={setNoCertificate}
          inFront={inFront}
          onInFront={(value) => {
            setInFront(value);
            setProxiesError(undefined);
          }}
          proxies={proxies}
          onProxies={(value) => {
            setProxies(value);
            setProxiesError(undefined);
          }}
          proxiesError={proxiesError}
        />
      </form>
    </Sheet>
  );
}
