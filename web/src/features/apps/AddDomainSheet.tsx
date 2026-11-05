// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService, ChangeState, DomainService, type ServiceInfo } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, Switch, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { appOf, environmentOf, nameOf, tidyMessage } from "./app";
import { proxyList, routeName } from "./domains";
import { validPort } from "./form";

/**
 * Sends a domain to this app: a domain and the port it listens on, then the plan,
 * which says what a server's first route does to its ports 80 and 443. How the
 * domain gets its certificate is the server's to work out, unless a tunnel or a
 * proxy in front handles HTTPS, when there's none to get, and the route answers
 * only what's in front. An app on the server's own network can't be sent to by name,
 * so the route goes to its port on the server. The plan says what each of those means.
 */
export function AddDomainSheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const name = nameOf(service);
  const environment = environmentOf(service);
  const known = useQuery(DomainService.method.listDomains, { environment });
  const app = appOf(service);
  const onServer = app?.hostNetwork ?? false;
  const published = app?.ports[0]?.containerPort;
  const [domain, setDomain] = useState("");
  const [port, setPort] = useState(published ? String(published) : "80");
  const [noCertificate, setNoCertificate] = useState(false);
  const [proxies, setProxies] = useState("");
  const [proxiesError, setProxiesError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [domainError, setDomainError] = useState<string>();
  const [portError, setPortError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  useEffect(() => {
    if (plan || !reveal.current) return;
    reveal.current = false;
    field.current?.focus();
    field.current?.select();
  }, [plan]);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const wanted = domain.trim().toLowerCase();
    const domainOk = wanted !== "" && !/\s/.test(wanted);
    const portOk = validPort(port);
    setDomainError(domainOk ? undefined : t`Name the domain, such as app.example.com.`);
    setPortError(portOk ? undefined : t`Use a port from 1 to 65535.`);
    setProxiesError(undefined);
    setFailure(undefined);
    if (!domainOk || !portOk) return;
    setBusy(true);
    let change: Change | undefined;
    try {
      change = (
        await client.proposeChange({
          summary: t`Send ${wanted} to ${name}`,
          edits: [
            {
              resource: { kind: "route", scope: environment, name: routeName(wanted, known.data?.domains) },
              spec: {
                kind: {
                  case: "route",
                  value: {
                    domain: wanted,
                    ...(onServer ? { hostPort: Number(port) } : { service: name, port: Number(port) }),
                    ...(noCertificate ? { tls: "off", trustedProxies: proxyList(proxies) } : {}),
                  },
                },
              },
            },
          ],
        })
      ).change;
    } catch (err) {
      const said = tidyMessage(sentence(err));
      if (reasonOf(err) !== "invalid") setFailure(said);
      else if (noCertificate && /trusted prox/i.test(said)) setProxiesError(said);
      else setDomainError(said);
      setBusy(false);
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
          setDomainError(tidyMessage(after.error));
          reveal.current = true;
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else onClose();
      },
    });
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`Add a domain to ${name}`}
      subtitle={
        noCertificate
          ? t`People reach the app at this name, through what's in front of this server`
          : t`People reach the app at this name, over HTTPS`
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
          value={domain}
          error={domainError}
          hint={
            noCertificate
              ? t`Point its DNS at your tunnel or proxy.`
              : t`Point its DNS at this server, before or after. Its certificate comes by itself once it does.`
          }
          onChange={(event) => {
            setDomain(event.target.value);
            setDomainError(undefined);
          }}
        />
        <TextField
          label={onServer ? t`On the server` : t`In the app`}
          mono
          inputMode="numeric"
          value={port}
          error={portError}
          hint={
            onServer
              ? t`It's on the server's own network, so what it listens on there, such as 8080.`
              : t`What the app listens on, such as 80.`
          }
          onChange={(event) => {
            setPort(event.target.value);
            setPortError(undefined);
          }}
        />
        <Switch
          label={t`A tunnel or a proxy in front handles HTTPS`}
          description={t`This server takes no certificate, and traffic reaches it unencrypted. That's fine on a tunnel or a private network, and not on the open internet.`}
          checked={noCertificate}
          onChange={setNoCertificate}
        />
        {noCertificate ? (
          <TextField
            label={t`What's in front`}
            mono
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            value={proxies}
            error={proxiesError}
            hint={t`Leave this empty for a tunnel on the server's own network. Otherwise what connects to this server: an address or a range, such as a bridge network's 172.17.0.0/16, or cloudflare for Cloudflare's proxy. Separate several with commas.`}
            onChange={(event) => {
              setProxies(event.target.value);
              setProxiesError(undefined);
            }}
          />
        ) : null}
      </form>
    </Sheet>
  );
}
