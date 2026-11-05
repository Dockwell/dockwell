// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService, ChangeState, DomainService, type ServiceInfo } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useChangeSheet } from "../changes/ChangeSheet";
import { appOf, environmentOf, nameOf, tidyMessage } from "./app";
import { routeName } from "./domains";
import { validPort } from "./form";

/**
 * Sends a domain to this app: a domain and the port it listens on, then the plan,
 * which says what a server's first route does to its ports 80 and 443. How the
 * domain gets its certificate is the server's to work out; a route that takes none,
 * as behind a tunnel (#85), is a choice that belongs below the port.
 */
export function AddDomainSheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const name = nameOf(service);
  const environment = environmentOf(service);
  const known = useQuery(DomainService.method.listDomains, { environment });
  const published = appOf(service)?.ports[0]?.containerPort;
  const [domain, setDomain] = useState("");
  const [port, setPort] = useState(published ? String(published) : "80");
  const [busy, setBusy] = useState(false);
  const [domainError, setDomainError] = useState<string>();
  const [portError, setPortError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [hidden, setHidden] = useState(false);
  const field = useRef<HTMLInputElement>(null);

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  useEffect(() => {
    if (hidden || !reveal.current) return;
    reveal.current = false;
    field.current?.focus();
    field.current?.select();
  }, [hidden]);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const wanted = domain.trim().toLowerCase();
    const domainOk = wanted !== "" && !/\s/.test(wanted);
    const portOk = validPort(port);
    setDomainError(domainOk ? undefined : t`Name the domain, such as app.example.com.`);
    setPortError(portOk ? undefined : t`Use a port from 1 to 65535.`);
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
              spec: { kind: { case: "route", value: { domain: wanted, service: name, port: Number(port) } } },
            },
          ],
        })
      ).change;
    } catch (err) {
      if (reasonOf(err) === "invalid") setDomainError(tidyMessage(sentence(err)));
      else setFailure(tidyMessage(sentence(err)));
      setBusy(false);
      return;
    }
    setBusy(false);
    setHidden(true);
    sheet.open({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          setDomainError(tidyMessage(after.error));
          reveal.current = true;
          setHidden(false);
        } else if (after && after.state === ChangeState.PLANNED) setHidden(false);
        else onClose();
      },
    });
  };

  return (
    <Sheet
      open={!hidden}
      initialFocusRef={field}
      title={t`Add a domain to ${name}`}
      subtitle={t`People reach the app at this name, over HTTPS`}
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
          hint={t`Point its DNS at this server, before or after. Its certificate comes by itself once it does.`}
          onChange={(event) => {
            setDomain(event.target.value);
            setDomainError(undefined);
          }}
        />
        <TextField
          label={t`In the app`}
          mono
          inputMode="numeric"
          value={port}
          error={portError}
          hint={t`What the app listens on, such as 80.`}
          onChange={(event) => {
            setPort(event.target.value);
            setPortError(undefined);
          }}
        />
      </form>
    </Sheet>
  );
}
