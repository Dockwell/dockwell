// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type Server, WebsiteService } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, SelectField, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useId, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useTunnelSwitchName } from "../apps/DomainNote";
import { DomainRecord } from "../apps/DomainRecord";
import { formRefusal, proxyList, refusedField } from "../apps/domains";
import { FrontFields, frontOf, type InFront } from "../apps/FrontFields";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { type FleetState, useFleet } from "../servers/useFleet";
import {
  BLANK,
  type Missing,
  missing,
  type Serves,
  type Settings,
  refusedField as servesField,
  specOf,
} from "./settings";
import { useMissingWords, useServesWords, WebsiteFields } from "./WebsiteFields";
import "../apps/Apps.css";
import "./Websites.css";

/** The server a website goes on: the one chosen, or the only one there is. */
function serverOf(fleet: FleetState, id: string): Server | undefined {
  if (fleet.status === "connecting") return undefined;
  return fleet.servers.find((s) => s.id === id) ?? (fleet.servers.length === 1 ? fleet.servers[0] : undefined);
}

/**
 * New website: its domain, what it serves, and what's in front of the server, then the
 * plan. A folder and a URL are served by a small web server of its own; a folder and a
 * port on the server need a person, which the plan says. The folder follows the domain,
 * as /var/www/<domain>, until it's typed.
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
  const [settings, setSettings] = useState<Settings>(BLANK);
  const rootTyped = useRef(false);
  const [noCertificate, setNoCertificate] = useState(false);
  const [inFront, setInFront] = useState<InFront>("tunnel");
  const [proxies, setProxies] = useState("");
  const [proxiesError, setProxiesError] = useState<string>();
  const [domainError, setDomainError] = useState<string>();
  const [fieldError, setFieldError] = useState<{ field: Missing["field"]; said: string }>();
  const [serverError, setServerError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  const form = useRef<HTMLFormElement>(null);
  const choiceName = useId();
  const switchName = useTunnelSwitchName();
  const missingWords = useMissingWords();
  const servesWords = useServesWords();
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
    const at = refusedField(said, { domain: wanted, port: settings.port, onServer: settings.serves === "port" });
    if (at === "domain") return setDomainError(said);
    if (at === "proxies" && noCertificate && inFront === "proxy") return setProxiesError(said);
    const target = servesField(said, settings.serves) || (at === "port" && settings.serves === "port" ? "port" : "");
    if (target) return setFieldError({ field: target, said });
    setFailure(said);
  };

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const domainOk = typed !== "" && !/\s/.test(typed);
    const gap = missing(settings);
    const named = proxyList(proxies);
    const proxiesOk = !noCertificate || inFront !== "proxy" || named.length > 0;
    const serverOk = !several || server !== "";
    setDomainError(domainOk ? undefined : t`Name the domain, such as blog.example.com.`);
    setFieldError(gap ? { field: gap.field, said: missingWords(gap) } : undefined);
    setProxiesError(proxiesOk ? undefined : t`Name what connects to this server, as an address or a range.`);
    setServerError(serverOk ? undefined : t`Choose the server it runs on.`);
    setFailure(undefined);
    if (!domainOk || gap || !proxiesOk || !serverOk) {
      revealError();
      return;
    }
    const front = frontOf(noCertificate, inFront, named);
    const website = specOf(settings);
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

  const choices: { value: Serves; hint: string }[] = [
    {
      value: "folder",
      hint: t`A static site: the files in a folder on this server, as they are. You approve the folder in the plan.`,
    },
    {
      value: "url",
      hint: t`Another site, or an address on your network, such as http://10.0.0.5:8080. A small web server of its own sends each request on.`,
    },
    {
      value: "port",
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
            // The folder follows the domain until it's typed, as /var/www/<domain>.
            const name = event.target.value.trim().toLowerCase();
            if (!rootTyped.current) {
              setSettings((was) => ({ ...was, root: name ? `/var/www/${name}` : "" }));
              setFieldError((said) => (said?.field === "root" ? undefined : said));
            }
          }}
        />
        <DomainRecord
          server={serverOf(fleet, server)}
          domain={typed}
          tunnel={noCertificate}
          tunnelName={switchName}
          onLeave={onClose}
        />
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
        <fieldset className="app-front website-sends">
          <legend className="dw-field-label">
            <Trans>What it serves</Trans>
          </legend>
          {choices.map((choice) => (
            <label key={choice.value} className={cx("app-front-choice", settings.serves === choice.value && "is-on")}>
              <input
                type="radio"
                name={choiceName}
                checked={settings.serves === choice.value}
                onChange={() => {
                  setSettings({ ...settings, serves: choice.value });
                  setFieldError(undefined);
                  setFailure(undefined);
                }}
              />
              <span className="app-front-text">
                <span className="app-front-name">{servesWords[choice.value]}</span>
                <span className="app-front-hint">{choice.hint}</span>
              </span>
            </label>
          ))}
        </fieldset>
        <WebsiteFields
          settings={settings}
          server={serverOf(fleet, server)?.id ?? ""}
          errors={fieldError ? { [fieldError.field]: fieldError.said } : {}}
          onChange={(next) => {
            if (next.root !== settings.root) rootTyped.current = true;
            setSettings(next);
            setFieldError(undefined);
          }}
        />
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
