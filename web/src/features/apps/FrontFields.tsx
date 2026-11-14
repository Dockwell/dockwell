// SPDX-License-Identifier: AGPL-3.0-only

import { Callout, cx, Switch, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useId, useRef } from "react";

import { useTunnelSwitchName } from "./DomainNote";

/** What ends TLS in front of this server, which says who the route answers. */
export type InFront = "tunnel" | "cloudflare" | "proxy" | "anyone";

/** What a route says of what's in front, as a route's spec takes it: nothing, with a certificate. */
export interface FrontSpec {
  tls?: string;
  trustedProxies?: string[];
  plainForAnyone?: boolean;
}

/** A route's settings for what's in front of the server: none, so it gets a certificate, or TLS off and who it answers. */
export function frontOf(noCertificate: boolean, inFront: InFront, proxies: string[]): FrontSpec {
  if (!noCertificate) return {};
  switch (inFront) {
    case "tunnel":
      return { tls: "off", trustedProxies: ["127.0.0.1"] };
    case "cloudflare":
      return { tls: "off", trustedProxies: ["cloudflare"] };
    case "proxy":
      return { tls: "off", trustedProxies: proxies };
    case "anyone":
      // Nothing in front, so nothing is believed on who a visitor is.
      return { tls: "off", trustedProxies: [], plainForAnyone: true };
  }
}

/**
 * Whether a tunnel or a proxy in front of the server handles HTTPS, and if so what it
 * is, as that's who the route answers and who what's behind it believes on who a
 * visitor is. The domain form and New website ask it alike.
 */
export function FrontFields({
  noCertificate,
  onNoCertificate,
  inFront,
  onInFront,
  proxies,
  onProxies,
  proxiesError,
  behind = "app",
}: {
  noCertificate: boolean;
  onNoCertificate: (on: boolean) => void;
  inFront: InFront;
  onInFront: (value: InFront) => void;
  proxies: string;
  onProxies: (value: string) => void;
  proxiesError: string | undefined;
  /** What's behind the route, which reads the visitor's address. */
  behind?: "app" | "website";
}) {
  const { t } = useLingui();
  const switchName = useTunnelSwitchName();
  const choiceName = useId();
  // What a choice warns of is what it's for, so it isn't left below the fold of a phone.
  const said = useRef<HTMLDivElement>(null);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs as the choice changes, once what it says is there.
  useEffect(() => {
    said.current?.scrollIntoView?.({ block: "nearest" });
  }, [inFront]);

  const choices: { value: InFront; label: string; hint: string }[] = [
    {
      value: "tunnel",
      label: t`A tunnel on this server`,
      hint:
        behind === "website"
          ? t`Such as cloudflared on the server's own network.`
          : t`Such as cloudflared on the server's own network.`,
    },
    { value: "cloudflare", label: t`Cloudflare's proxy`, hint: t`Cloudflare connects to this server's port 80.` },
    { value: "proxy", label: t`Another proxy`, hint: t`You name what connects to this server.` },
    {
      value: "anyone",
      label: t`Anyone who can reach this server`,
      hint: t`For a private network with nothing in front.`,
    },
  ];

  return (
    <>
      <Switch
        className="app-front-switch"
        label={switchName}
        description={t`This server takes no certificate, and traffic reaches it unencrypted.`}
        checked={noCertificate}
        onChange={onNoCertificate}
      />
      {noCertificate ? (
        <fieldset className="app-front">
          <legend className="dw-field-label">
            <Trans>What's in front</Trans>
          </legend>
          {choices.map((choice) => (
            <label key={choice.value} className={cx("app-front-choice", inFront === choice.value && "is-on")}>
              <input
                type="radio"
                name={choiceName}
                checked={inFront === choice.value}
                onChange={() => onInFront(choice.value)}
              />
              <span className="app-front-text">
                <span className="app-front-name">{choice.label}</span>
                <span className="app-front-hint">{choice.hint}</span>
              </span>
            </label>
          ))}
          <div ref={said} className="app-front-says">
            {inFront === "proxy" ? (
              <TextField
                label={t`What connects to this server`}
                mono
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                value={proxies}
                error={proxiesError}
                hint={t`An address or a range, such as 10.77.0.0/24 for a tunnel on a bridge. That proxy needs a network of its own, not Docker's default bridge. Separate several with commas.`}
                onChange={(event) => onProxies(event.target.value)}
              />
            ) : null}
            {inFront === "cloudflare" ? (
              <Callout tone="warning">
                <Trans>
                  This path crosses the open internet unencrypted, between Cloudflare and this server. It needs Flexible
                  mode in Cloudflare, and a tunnel is the better way.
                </Trans>
              </Callout>
            ) : null}
            {inFront === "anyone" ? (
              <Callout tone="warning">
                <Trans>
                  It answers anyone who can reach port 80 over plain HTTP, around whatever is in front. You approve this
                  in the plan.
                </Trans>
              </Callout>
            ) : null}
          </div>
        </fieldset>
      ) : null}
    </>
  );
}
