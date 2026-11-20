// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type CloudflareCheck, CloudflareName, RouteWay, type WayOption } from "@dockwell/api";
import { cx, Glyph, SelectField, STATUS, type Status, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode, Ref } from "react";

import { formatClock } from "../../lib/format";
import { nextName, oneLabel, type TakeOverInit, wayOf, type Zone } from "./publish";

import "./DomainStep.css";

/** A state under the field it's about: its shape and colour, a fact, and one action at most. */
export function StateLine({ status, children, action }: { status: Status; children: ReactNode; action?: ReactNode }) {
  return (
    <span className="domain-step-state">
      <span className={cx("domain-step-glyph", `dw-tone-${STATUS[status].tone}`)}>
        <Glyph shape={STATUS[status].shape} size={8} />
      </span>
      <span>
        {children}
        {action ? <> · {action}</> : null}
      </span>
    </span>
  );
}

/** An action in a state line: a link-like button that does it here. */
function Act({ onClick, children }: { onClick: () => void; children: ReactNode }) {
  return (
    <button type="button" className="domain-step-act" onClick={onClick}>
      {children}
    </button>
  );
}

/**
 * The name, as a subdomain and a zone a connected DNS provider writes in (ADR 0104,
 * section 1): `wiki` and `.example.com`, or a whole name typed under Another domain.
 */
export function DomainName({
  sub,
  onSub,
  zone,
  onZone,
  zones,
  error,
  readOnly,
  field,
  children,
}: {
  sub: string;
  onSub: (value: string) => void;
  /** The zone picked, or "" for another domain. */
  zone: string;
  onZone: (zone: string) => void;
  zones: readonly Zone[];
  error?: string;
  readOnly?: boolean;
  field?: Ref<HTMLInputElement>;
  /** The state under the field. */
  children?: ReactNode;
}) {
  const { t } = useLingui();
  return (
    <div className="domain-step-name">
      <span className="dw-field-label" id="domain-step-label">
        <Trans>Domain</Trans>
      </span>
      <div className="domain-step-row">
        <TextField
          ref={field}
          className="domain-step-sub"
          aria-label={zone ? t`Subdomain` : t`Domain`}
          aria-describedby="domain-step-label"
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder={zone ? "" : "shop.example.net"}
          value={sub}
          readOnly={readOnly}
          error={error}
          onChange={(event) => onSub(event.target.value)}
        />
        <SelectField
          className="domain-step-zone"
          aria-label={t`Zone`}
          mono
          value={zone}
          disabled={readOnly}
          options={[
            ...zones.map((z) => ({ value: z.zone, label: `.${z.zone}` })),
            { value: "", label: t({ message: "Another domain", context: "a domain in no connected zone" }) },
          ]}
          onChange={(event) => onZone(event.target.value)}
        />
      </div>
      {children}
    </div>
  );
}

/**
 * What a connected zone says of a name, as a state and its one action (ADR 0104, section
 * 1 and the States mock): free, taken with a free name beside it, past Cloudflare's free
 * certificate with one that's under it, or made in Cloudflare and pointing here.
 */
function RecordState({
  check,
  checking,
  domain,
  sub,
  companion,
  companionOn = true,
  onCompanion,
  takesOver,
  onTakesOver,
  onSub,
}: {
  check: CloudflareCheck | undefined;
  checking: boolean;
  domain: string;
  sub: string;
  zone: string;
  companion: string;
  /** Whether the name's other half, www or the bare domain, answers beside it. */
  companionOn?: boolean;
  /** Opens the choice to leave the other half out, one step away. */
  onCompanion?: () => void;
  takesOver: readonly TakeOverInit[];
  onTakesOver: (next: TakeOverInit[]) => void;
  onSub: (value: string) => void;
}) {
  const { t, i18n } = useLingui();
  if (!check) {
    return checking && domain !== "" ? (
      <StateLine status="deploying">
        <Trans>Checking Dockwell and Cloudflare</Trans>
      </StateLine>
    ) : null;
  }
  const consented = check.recordId !== "" && takesOver.some((o) => o.recordId === check.recordId);
  const record = check.record;
  switch (check.state) {
    case CloudflareName.FREE:
      return (
        <StateLine
          status="healthy"
          action={companion && onCompanion ? <Act onClick={onCompanion}>{t`Change`}</Act> : undefined}
        >
          {companion
            ? companionOn
              ? t`${domain}, with ${companion} beside it`
              : t`${domain} alone, without ${companion}`
            : t`Free in Dockwell and at Cloudflare`}
        </StateLine>
      );
    case CloudflareName.OURS:
      return (
        <StateLine status="healthy">
          <Trans>Published already</Trans>
        </StateLine>
      );
    case CloudflareName.TAKEN: {
      if (check.recordId === "") {
        const next = nextName(sub || "www");
        return (
          <StateLine status="down" action={<Act onClick={() => onSub(next)}>{t`Use ${next}`}</Act>}>
            <Trans>Taken at Cloudflare</Trans>
          </StateLine>
        );
      }
      return consented ? (
        <StateLine
          status="degraded"
          action={<Act onClick={() => onTakesOver(takesOver.filter((o) => !o.recordId))}>{t`Keep it`}</Act>}
        >
          {t`Replaces ${record}, made in Cloudflare. Undo puts it back`}
        </StateLine>
      ) : (
        <StateLine
          status="degraded"
          action={
            <Act
              onClick={() => onTakesOver([...takesOver.filter((o) => !o.recordId), { recordId: check.recordId }])}
            >{t`Replace it`}</Act>
          }
        >
          {t`A record exists: ${record}, made in Cloudflare`}
        </StateLine>
      );
    }
    case CloudflareName.TAKE_ON:
      if (check.recordId === "") {
        return (
          <StateLine status="degraded">
            <Trans>Made in Cloudflare and pointing here. An admin can take it on</Trans>
          </StateLine>
        );
      }
      return consented ? (
        <StateLine
          status="healthy"
          action={<Act onClick={() => onTakesOver(takesOver.filter((o) => !o.recordId))}>{t`Leave it`}</Act>}
        >
          {t`Takes on ${record}, made in Cloudflare`}
        </StateLine>
      ) : (
        <StateLine
          status="degraded"
          action={
            <Act
              onClick={() => onTakesOver([...takesOver.filter((o) => !o.recordId), { recordId: check.recordId }])}
            >{t`Take it on`}</Act>
          }
        >
          {t`Made in Cloudflare and pointing here: ${record}`}
        </StateLine>
      );
    case CloudflareName.PAST_CERTIFICATE: {
      const under = oneLabel(sub);
      return (
        <StateLine status="down" action={<Act onClick={() => onSub(under)}>{t`Use ${under}`}</Act>}>
          {t`${domain} is past Cloudflare's free certificate`}
        </StateLine>
      );
    }
    case CloudflareName.WAITING: {
      const at = check.checksAgain ? formatClock(timestampDate(check.checksAgain), i18n.locale) : "";
      return (
        <StateLine status="unknown">
          {at ? t`Cloudflare asked Dockwell to wait. It checks again at ${at}` : t`Cloudflare asked Dockwell to wait`}
        </StateLine>
      );
    }
  }
  return null;
}

/**
 * What a connected zone says of a name, as a state and its one action (ADR 0104, section
 * 1 and the States mock): its record, then the tunnel's rule made by hand that serves
 * it, for an admin to take on or move to Dockwell. A check that couldn't be made says
 * so, with Check again, rather than nothing (the review's B-M1).
 */
export function NameState(
  props: Parameters<typeof RecordState>[0] & {
    /** Why the check couldn't be made, or "". */
    failed: string;
    onAgain: () => void;
  },
) {
  const { t } = useLingui();
  const { check, failed, onAgain, takesOver, onTakesOver } = props;
  if (failed) {
    return (
      <StateLine status="degraded" action={<Act onClick={onAgain}>{t`Check again`}</Act>}>
        {failed}
      </StateLine>
    );
  }
  const hand = check?.handRule;
  const ruleConsented =
    hand !== undefined && takesOver.some((o) => o.hostname === hand.hostname && o.tunnel === hand.tunnel);
  const rule = hand
    ? { tunnel: hand.tunnel, hostname: hand.hostname, ...(hand.path ? { path: hand.path } : {}) }
    : undefined;
  const withoutRule = () => takesOver.filter((o) => !o.hostname);
  const tunnelName = hand?.tunnelName ?? "";
  const service = hand?.service ?? "";
  return (
    <>
      <RecordState {...props} />
      {hand && rule ? (
        ruleConsented ? (
          <StateLine status="healthy" action={<Act onClick={() => onTakesOver(withoutRule())}>{t`Leave it`}</Act>}>
            {hand.reachesProxy
              ? t`Takes on the rule made in Cloudflare in ${tunnelName}. Undo puts it back`
              : t`Moves the rule made in Cloudflare in ${tunnelName} to the server's proxy. Undo puts it back`}
          </StateLine>
        ) : (
          <StateLine
            status="degraded"
            action={
              <Act onClick={() => onTakesOver([...withoutRule(), rule])}>
                {hand.reachesProxy ? t`Take it on` : t`Move it to Dockwell`}
              </Act>
            }
          >
            {hand.reachesProxy
              ? t`A rule made in Cloudflare in ${tunnelName} sends it to the server's proxy`
              : t`A rule made in Cloudflare in ${tunnelName} sends it to ${service}`}
          </StateLine>
        )
      ) : null}
    </>
  );
}

/** What each way is called, and what it does, in a line each. */
function useWayWords(server: string, address: string) {
  const { t } = useLingui();
  // A record points at the server's address, where it's known (the review's B-L4).
  const at = address || server;
  return {
    name: (w: WayOption | undefined, way: RouteWay): string => {
      const tunnel = w?.tunnelName || w?.tunnel || "";
      switch (way) {
        case RouteWay.SERVER_TUNNEL:
          return tunnel ? t`Through the tunnel ${tunnel} on ${server}` : t`Through the tunnel on ${server}`;
        case RouteWay.OWN_TUNNEL:
          return t`Dockwell's own tunnel on ${server}`;
        case RouteWay.PROXY:
          return t`Through Cloudflare's proxy`;
        case RouteWay.OWN_HTTPS:
          return t`This server's own HTTPS`;
        case RouteWay.FRONT:
          return t`Something else in front`;
      }
      return t`None yet`;
    },
    does: (w: WayOption | undefined, way: RouteWay, flexible: boolean): string => {
      // The server is named once, in the way's own line (the polish round).
      const reaches = w?.reaches ?? "";
      switch (way) {
        case RouteWay.SERVER_TUNNEL:
          return reaches
            ? t`Cloudflare ends HTTPS, and the tunnel reaches the server at ${reaches}`
            : t`Cloudflare ends HTTPS, and the tunnel reaches the server's proxy`;
        case RouteWay.OWN_TUNNEL:
          return t`A tunnel only Dockwell writes, about 20 MB on the server`;
        case RouteWay.PROXY:
          return flexible
            ? t`A proxied record to ${at}, which Cloudflare reaches over HTTP`
            : t`A proxied record to ${at}, and a certificate by DNS`;
        case RouteWay.OWN_HTTPS:
          return t`A DNS-only record to ${at}, which gets its own certificate`;
        case RouteWay.FRONT:
          return t`Dockwell writes nothing at Cloudflare. What's in front ends HTTPS`;
      }
      return "";
    },
  };
}

/** What fixes a way that's off: a page at Cloudflare, or the catalog's tunnel. */
function OffAction({ option, onLeave }: { option: WayOption; onLeave: () => void }) {
  if (option.action === "") return null;
  if (option.actionUrl !== "") {
    return (
      <a className="domain-step-act" href={option.actionUrl} target="_blank" rel="noreferrer">
        {option.action}
      </a>
    );
  }
  if (option.way === RouteWay.OWN_TUNNEL) {
    return (
      <Link className="domain-step-act" to="/catalog/$entryId" params={{ entryId: "cloudflared" }} onClick={onLeave}>
        {option.action}
      </Link>
    );
  }
  return null;
}

/** The ways a person can pick, each once: the server's tunnels, then the rest in the check's order. */
function choices(check: CloudflareCheck): WayOption[] {
  return check.ways.filter((w, i) => check.ways.findIndex((o) => o.way === w.way && o.tunnel === w.tunnel) === i);
}

/**
 * How the name is reached, decided for the person in one line with Change beside it
 * (ADR 0104, section 2): the check's default, why it isn't the first where it isn't,
 * and, under Change, every way as a card, one that can't be taken saying why with what
 * fixes it.
 */
export function WayIn({
  check,
  way,
  tunnel,
  onWay,
  open,
  onOpen,
  server,
  address = "",
  admin,
  onStrict,
  onLeave,
}: {
  check: CloudflareCheck | undefined;
  way: RouteWay;
  tunnel: string;
  onWay: (way: RouteWay, tunnel: string) => void;
  /** Whether the ways show as cards. */
  open: boolean;
  onOpen: (open: boolean) => void;
  /** The server's name. */
  server: string;
  /** The server's public address, where it's known. */
  address?: string;
  /** Whether the caller may set the zone to Full (strict). */
  admin: boolean;
  onStrict: () => void;
  onLeave: () => void;
}) {
  const { t } = useLingui();
  const words = useWayWords(server, address);
  if (!check || check.ways.length === 0) return null;
  const flexible = check.sslMode === "flexible";
  const picked = wayOf(check, way, tunnel);
  const takeable = picked !== undefined && picked.off === "";
  // Why the default isn't the server's tunnel, where it runs one Dockwell can't use.
  const tunnelOff =
    way !== RouteWay.SERVER_TUNNEL
      ? check.ways.find((w) => w.way === RouteWay.SERVER_TUNNEL && w.off !== "")
      : undefined;
  const flexibleLine =
    flexible && way === RouteWay.PROXY ? (
      <StateLine status="degraded" action={admin ? <Act onClick={onStrict}>{t`Use Full (strict)`}</Act> : undefined}>
        {t`${check.zone} is set to Flexible, so Cloudflare reaches ${server} over HTTP`}
      </StateLine>
    ) : null;

  if (open) {
    return (
      <div className="domain-step-ways" role="radiogroup" aria-labelledby="domain-step-way">
        <div className="domain-step-ways-head">
          <span id="domain-step-way" className="dw-field-label">
            <Trans>Way in</Trans>
          </span>
          <span className="domain-step-muted">{t`${check.zone} is on Cloudflare`}</span>
        </div>
        {choices(check).map((w) => {
          const on = w.way === way && (w.tunnel === "" || w.tunnel === tunnel);
          const off = w.off !== "";
          return (
            <label key={`${w.way}-${w.tunnel}`} className={cx("domain-step-card", on && "is-on", off && "is-off")}>
              <input
                type="radio"
                name="domain-step-way"
                checked={on}
                disabled={off}
                onChange={() => {
                  onWay(w.way, w.tunnel);
                  onOpen(false);
                }}
                // The way already picked closes the cards too, which no change event says.
                onClick={() => {
                  if (on) onOpen(false);
                }}
              />
              <span className="domain-step-card-text">
                <span className="domain-step-card-name">{words.name(w, w.way)}</span>
                <span className="domain-step-card-hint">
                  {off ? w.off : words.does(w, w.way, flexible)}
                  {off && w.action ? (
                    <>
                      {" · "}
                      <OffAction option={w} onLeave={onLeave} />
                    </>
                  ) : null}
                </span>
              </span>
            </label>
          );
        })}
      </div>
    );
  }

  return (
    <dl className="domain-step-facts">
      <dt>
        <Trans>Way in</Trans>
      </dt>
      <dd className="domain-step-way">
        <span>{takeable ? words.name(picked, way) : t`None yet`}</span>
        {takeable ? <span className="domain-step-muted">{words.does(picked, way, flexible)}</span> : null}
        {takeable && picked.addsRule ? (
          <StateLine status="unknown">{t`Adds ${picked.addsRule} to ${picked.tunnelName}, once`}</StateLine>
        ) : null}
        {!takeable && picked ? (
          <StateLine status="down" action={picked.action ? <OffAction option={picked} onLeave={onLeave} /> : undefined}>
            {picked.off}
          </StateLine>
        ) : null}
        {takeable && tunnelOff ? (
          // A default that moved off the server's tunnel says why as a warning, with what
          // fixes it, or Change where a rule catches the name first (the review's B-L3).
          <StateLine
            status="degraded"
            action={
              tunnelOff.action ? (
                <OffAction option={tunnelOff} onLeave={onLeave} />
              ) : tunnelOff.caughtBy ? (
                <Act onClick={() => onOpen(true)}>{t`Change`}</Act>
              ) : undefined
            }
          >
            {tunnelOff.off}
          </StateLine>
        ) : null}
        {flexibleLine}
      </dd>
      <dd>
        <button
          type="button"
          className="domain-step-change"
          aria-label={t`Change the way in`}
          onClick={() => onOpen(true)}
        >
          <Trans>Change</Trans>
        </button>
      </dd>
    </dl>
  );
}
