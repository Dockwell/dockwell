// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeState, ImportService, type PanelDomainCheck, type PanelKind, type Server } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { type Opening, PlanView } from "../changes/ChangeSheet";

/*
 * Handing over a panel's domains is evidence and a choice (ADR 0089, "The domain
 * handover: evidence and a choice"). The sheet checks each domain the panel's
 * proxy serves as it opens: a scan of every rule on the server, the proxy's
 * static configuration, and a request through the proxy. Each nothing was found
 * for is chosen already; each other says why it stays, and can't be chosen. The
 * plan names the chosen ones, and the agent checks them again before the proxy
 * stops.
 */

/** The sheet that checks a panel's domains and has a person choose which come over. */
export function HandOverSheet({
  server,
  kind,
  name,
  onClose,
}: {
  server: Server;
  kind: PanelKind;
  name: string;
  onClose: (applied: boolean) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const imports = useMemo(() => createClient(ImportService, transport), [transport]);
  const phone = useMediaQuery(PHONE_QUERY);
  const [checks, setChecks] = useState<PanelDomainCheck[]>();
  const [failure, setFailure] = useState<unknown>();
  const [chosen, setChosen] = useState<Set<string>>(new Set());
  const [plan, setPlan] = useState<Opening>();

  // Checked as it opens: what was found decides what can be chosen.
  useEffect(() => {
    let gone = false;
    imports
      .checkPanelDomains({ serverId: server.id, panel: kind })
      .then((res) => {
        if (gone) return;
        setChecks(res.domains);
        setChosen(new Set(res.domains.filter((d) => d.held === "").map((d) => d.domain)));
      })
      .catch((err: unknown) => {
        if (!gone) setFailure(err);
      });
    return () => {
      gone = true;
    };
  }, [imports, server.id, kind]);

  const pick = (domain: string, on: boolean) =>
    setChosen((was) => {
      const next = new Set(was);
      if (on) next.add(domain);
      else next.delete(domain);
      return next;
    });
  const review = () => {
    const domains = [...chosen].sort();
    setPlan({
      kind: "plan",
      make: async () => ({
        change: (await imports.planPanelDomains({ serverId: server.id, panel: kind, domains })).change,
      }),
      retryLabel: t`Choose again`,
      onDone: () => onClose(true),
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED) onClose(true);
        else setPlan(undefined);
      },
    });
  };
  const scanned = checks?.[0]?.scanned;
  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      title={t`Hand over ${name}'s domains`}
      subtitle={t`Each you choose comes as a route once the proxy stops; the rest stop answering`}
      onClose={() => onClose(false)}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={() => onClose(false)}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" size={phone ? "lg" : "md"} disabled={chosen.size === 0} onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      {failure ? (
        <Callout tone="critical" title={t`Couldn't check ${name}'s domains`}>
          {sentence(failure)}
        </Callout>
      ) : checks === undefined ? (
        <p className="handover-checking" aria-busy="true">
          <Icon name="search" size={14} />
          <Trans>Checking each domain through {name}'s proxy</Trans>
        </p>
      ) : checks.length === 0 ? (
        <Callout tone="info" title={t`Nothing left to hand over`}>
          <Trans>Dockwell routes each of {name}'s domains already.</Trans>
        </Callout>
      ) : (
        <>
          <ul className="handover-domains" aria-label={t`Domains`}>
            {checks.map((c) => (
              <li key={c.domain} className={cx("handover-domain", c.held !== "" && "is-held")}>
                <label>
                  <input
                    type="checkbox"
                    checked={chosen.has(c.domain)}
                    disabled={c.held !== ""}
                    onChange={(e) => pick(c.domain, e.currentTarget.checked)}
                  />
                  <span className="handover-name">{c.domain}</span>
                </label>
                <span className="handover-found">
                  {c.held !== "" ? (
                    <>
                      <Icon name="lock" size={12} />
                      {c.held}
                    </>
                  ) : (
                    <>
                      <Icon name="check" size={12} />
                      <Trans>No login or address limit found</Trans>
                      {c.probes.length > 0 ? (
                        <span className="handover-answers">
                          {c.probes.map((p) => `${p.path} ${p.answer}`).join(", ")}
                        </span>
                      ) : null}
                    </>
                  )}
                </span>
              </li>
            ))}
          </ul>
          {scanned ? (
            <p className="handover-scanned">
              <Trans>Checked {scanned}, and asked the proxy for each.</Trans>
            </p>
          ) : null}
        </>
      )}
    </Sheet>
  );
}
