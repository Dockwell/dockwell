// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { ChangeState, type Website, type WebsiteInfo, WebsiteService } from "@dockwell/api";
import {
  Button,
  cx,
  Glyph,
  Icon,
  Panel,
  PHONE_QUERY,
  SelectField,
  Switch,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type ReactNode, useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { plainForAnyone } from "../apps/domains";
import { validPort } from "../apps/form";
import { useChangeSheet } from "../changes/ChangeSheet";
import { CopyButton } from "../servers/CopyButton";
import { useCertificateWords } from "./Standing";
import { useWebsiteValue } from "./WebsiteLayout";
import { changedLines, kindOf } from "./website";

/** What the form holds of what a website sends to. */
interface Settings {
  sendsTo: "url" | "port";
  url: string;
  port: string;
  targetHost: boolean;
}

function settingsOf(spec: Website | undefined): Settings {
  const proxy = spec?.serves.case === "proxy" ? spec.serves.value : undefined;
  return {
    sendsTo: proxy?.port ? "port" : "url",
    url: proxy?.url ?? "",
    port: proxy?.port ? String(proxy.port) : "",
    targetHost: proxy?.sendTargetHost ?? false,
  };
}

/** The settings as the API takes them. */
function specOf(s: Settings) {
  return {
    serves: {
      case: "proxy" as const,
      value: s.sendsTo === "port" ? { port: Number(s.port) || 0 } : { url: s.url.trim(), sendTargetHost: s.targetHost },
    },
  };
}

/** How long typing settles before what it comes to is asked for. */
const PREVIEW_AFTER_MS = 300;

function useSettled<T>(value: T): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), PREVIEW_AFTER_MS);
    return () => clearTimeout(timer);
  }, [value]);
  return settled;
}

/**
 * A website's configuration, as Website-Detail lays it out: what it sends to on the
 * left, and on the right the config its web server runs, with the lines an edit
 * changes marked, as the control plane writes it. Nothing changes until the plan is
 * applied. A website that sends to a port on the server has no web server of its own,
 * so the right says how the ingress reaches it instead.
 */
export function WebsiteConfiguration() {
  const { row } = useWebsiteValue();
  // A change applied elsewhere starts the form again from what's stored now.
  return <ConfigurationForm key={JSON.stringify(settingsOf(row.site.spec))} site={row.site} />;
}

function ConfigurationForm({ site }: { site: WebsiteInfo }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const sheet = useChangeSheet();
  const was = useMemo(() => settingsOf(site.spec), [site.spec]);
  const [now, setNow] = useState<Settings>(was);
  const settled = useSettled(now);
  const current = useQuery(WebsiteService.method.previewWebsite, { website: site.spec }, { enabled: !!site.spec });
  const portOk = settled.sendsTo !== "port" || validPort(settled.port);
  const urlTyped = settled.sendsTo !== "url" || settled.url.trim() !== "";
  const preview = useQuery(
    WebsiteService.method.previewWebsite,
    { website: specOf(settled) },
    { enabled: portOk && urlTyped, retry: false },
  );
  const refusal = !portOk
    ? t`Use a port from 1 to 65535.`
    : !urlTyped
      ? t`Give the URL to send to, such as https://backend.example.com.`
      : preview.error
        ? sentence(preview.error)
        : "";

  const changes = [
    now.sendsTo !== was.sendsTo,
    now.sendsTo === "url" && now.url.trim() !== was.url,
    now.sendsTo === "url" && now.targetHost !== was.targetHost,
    now.sendsTo === "port" && now.port !== was.port,
  ].filter(Boolean).length;
  const typing = JSON.stringify(now) !== JSON.stringify(settled);
  const ready = changes > 0 && refusal === "" && !typing && preview.isSuccess;

  const review = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(WebsiteService, transport).updateWebsite({ id: site.id, website: specOf(now) }),
      retryLabel: t`Edit and try again`,
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED) setNow(settingsOf(site.spec));
      },
    });

  const wasHint = (text: string) => (
    <span className="website-was">
      <span className="dw-plan-sym is-change" aria-hidden="true">
        ~
      </span>
      <span>{text}</span>
    </span>
  );
  const fieldRefusal = (field: "url" | "port") => {
    if (!refusal) return undefined;
    if (field === "port" && now.sendsTo === "port") return refusal;
    if (field === "url" && now.sendsTo === "url") return refusal;
    return undefined;
  };

  return (
    <div className="website-tab">
      <div className="website-columns">
        <div className="website-settings">
          <Panel title={<Trans>Where it sends</Trans>}>
            <div className="website-fields">
              <SelectField
                label={t`Sends its requests to`}
                value={now.sendsTo}
                options={[
                  { value: "url", label: t`A URL` },
                  { value: "port", label: t`A port on this server` },
                ]}
                hint={
                  now.sendsTo !== was.sendsTo
                    ? wasHint(was.sendsTo === "url" ? t`Was a URL` : t`Was a port on this server`)
                    : now.sendsTo === "url"
                      ? t`A small web server of its own sends each request on`
                      : t`The ingress sends each request straight to the port`
                }
                onChange={(event) => setNow({ ...now, sendsTo: event.target.value === "port" ? "port" : "url" })}
              />
              {now.sendsTo === "url" ? (
                <>
                  <TextField
                    label={t`URL`}
                    mono
                    inputMode="url"
                    autoCapitalize="none"
                    autoCorrect="off"
                    spellCheck={false}
                    value={now.url}
                    error={fieldRefusal("url")}
                    hint={
                      was.sendsTo === "url" && now.url.trim() !== was.url
                        ? wasHint(t`Was ${was.url}`)
                        : t`With a path, such as /app, each request goes under it.`
                    }
                    onChange={(event) => setNow({ ...now, url: event.target.value })}
                  />
                  <Switch
                    label={t`Send the URL's own name as Host`}
                    description={t`For a site that serves several names, such as another site over HTTPS. Off, it gets this website's domain.`}
                    checked={now.targetHost}
                    onChange={(on) => setNow({ ...now, targetHost: on })}
                  />
                </>
              ) : (
                <TextField
                  label={t`Port on the server`}
                  mono
                  inputMode="numeric"
                  value={now.port}
                  error={fieldRefusal("port")}
                  hint={
                    was.sendsTo === "port" && now.port !== was.port
                      ? wasHint(t`Was ${was.port} · whatever listens there sees every visitor as local`)
                      : t`Whatever listens there sees every visitor as local, so you approve it in the plan.`
                  }
                  onChange={(event) => setNow({ ...now, port: event.target.value })}
                />
              )}
            </div>
          </Panel>
          <DomainPanel />
        </div>
        <Panel className={cx("website-config")} flush>
          {now.sendsTo === "url" ? (
            <ConfigView
              before={current.data?.config ?? ""}
              after={preview.data?.config ?? (refusal ? "" : (current.data?.config ?? ""))}
              showChanges={changes > 0}
              loading={current.isPending && !!site.spec}
            />
          ) : (
            <RouteView port={now.port} />
          )}
          <div className="website-config-foot">
            <span className="website-valid" aria-live="polite">
              {refusal ? (
                <>
                  <span className="dw-tone-critical">
                    <Glyph shape="triangle" size={10} />
                  </span>
                  <strong>
                    <Trans>Not valid</Trans>
                  </strong>
                  <span className="websites-muted">{refusal}</span>
                </>
              ) : (
                <>
                  <span className="dw-tone-healthy">
                    <Glyph shape="circle" size={10} />
                  </span>
                  <strong>
                    <Trans>Valid</Trans>
                  </strong>
                  <span className="websites-muted">
                    {" · "}
                    <Trans>written by Dockwell from these settings</Trans>
                  </span>
                </>
              )}
            </span>
            <span className="website-foot-gap" />
            {changes > 0 ? (
              <span className="website-changes">{plural(changes, { one: "# change", other: "# changes" })}</span>
            ) : null}
            <Button variant="ghost" size={phone ? "lg" : "md"} disabled={changes === 0} onClick={() => setNow(was)}>
              <Trans>Discard</Trans>
            </Button>
            <Button variant="primary" icon="commit" size={phone ? "lg" : "md"} disabled={!ready} onClick={review}>
              <Trans>Review plan</Trans>
            </Button>
          </div>
        </Panel>
      </div>
      <DeletePanel site={site} />
    </div>
  );
}

/** A Caddyfile's line, in the parts the design colours: directive, placeholder, brace, comment. */
function codeParts(line: string): ReactNode[] {
  const out: ReactNode[] = [];
  const indent = /^\s*/.exec(line)?.[0] ?? "";
  if (indent) out.push(indent);
  let rest = line.slice(indent.length);
  if (rest.startsWith("#"))
    return [
      ...out,
      <span key="c" className="is-comment">
        {rest}
      </span>,
    ];
  const word = /^[a-z_]+/.exec(rest)?.[0];
  if (word) {
    out.push(
      <span key="d" className="is-directive">
        {word}
      </span>,
    );
    rest = rest.slice(word.length);
  }
  rest.split(/(\{[a-z_.$]+\}|[{}])/).forEach((part, i) => {
    if (!part) return;
    // Each part is its place in the line, which is all a key needs to say.
    const key = `p${i}`;
    if (part === "{" || part === "}") {
      out.push(
        <span key={key} className="is-brace">
          {part}
        </span>,
      );
    } else if (part.startsWith("{")) {
      out.push(
        <span key={key} className="is-placeholder">
          {part}
        </span>,
      );
    } else out.push(part);
  });
  return out;
}

/** The web server's config, with each line an edit changes marked, as Website-Detail draws it. */
function ConfigView({
  before,
  after,
  showChanges,
  loading,
}: {
  before: string;
  after: string;
  showChanges: boolean;
  loading: boolean;
}) {
  const { t } = useLingui();
  const lines = useMemo(() => changedLines(showChanges ? before : "", after), [before, after, showChanges]);
  return (
    <>
      <div className="website-config-head">
        <div className="website-config-title">
          <Icon name="file" size={16} />
          <h2>Caddyfile</h2>
        </div>
        <div className="website-config-tools">
          <span>
            <Trans>Its web server</Trans>
          </span>
          <CopyButton text={after} label={t`Copy the config`} />
        </div>
      </div>
      <section className="website-code" aria-label={t`The web server's config`} aria-busy={loading}>
        {lines.map((line) => (
          <div key={line.n} className={cx("website-code-line", line.changed && "is-changed")}>
            <span className="website-code-gutter">{line.n}</span>
            <span className="website-code-text">{codeParts(line.text)}</span>
          </div>
        ))}
      </section>
    </>
  );
}

/** How the ingress reaches a port on the server, which has no web server of its own to show. */
function RouteView({ port }: { port: string }) {
  const { t } = useLingui();
  const { row } = useWebsiteValue();
  const shown = validPort(port) ? port : "…";
  return (
    <>
      <div className="website-config-head">
        <div className="website-config-title">
          <Icon name="link" size={16} />
          <h2 className="is-words">{t`Route`}</h2>
        </div>
      </div>
      <div className="website-route">
        <dl>
          <dt>
            <Trans>Domain</Trans>
          </dt>
          <dd>
            <code>{row.site.domain}</code>
          </dd>
          <dt>
            <Trans>Sends to</Trans>
          </dt>
          <dd>
            <code>127.0.0.1:{shown}</code>
          </dd>
          {row.site.serverName ? (
            <>
              <dt>
                <Trans>On</Trans>
              </dt>
              <dd>
                <code>{row.site.serverName}</code>
              </dd>
            </>
          ) : null}
        </dl>
        <p>
          <Trans>
            The ingress sends each request straight to the port, with nothing of its own between, so there's no config
            here to show. Whatever listens there sees every visitor as local.
          </Trans>
        </p>
      </div>
    </>
  );
}

/** The website's domain, which the domain flow owns: how it's reached, and where to change that. */
function DomainPanel() {
  const { t } = useLingui();
  const { row, read } = useWebsiteValue();
  const certificate = useCertificateWords();
  const { domain, look, site } = row;
  const reached = !domain
    ? t`No route gives it a domain now`
    : domain.routes[0]?.tls !== "off"
      ? t`Over HTTPS, with its own certificate`
      : plainForAnyone(domain)
        ? t`Over plain HTTP, to anyone who reaches the server`
        : t`Through a tunnel or a proxy in front, which ends TLS`;
  const standing = domain && look ? certificate(domain, look, read) : "";
  return (
    <Panel
      title={<Trans>Domain</Trans>}
      actions={
        site.domain ? (
          <RouterButtonLink to="/domains" search={{ q: site.domain }} size="sm" variant="ghost">
            <Trans>Open in Domains</Trans>
          </RouterButtonLink>
        ) : null
      }
    >
      <dl className="website-facts">
        <dt>
          <Trans>Domain</Trans>
        </dt>
        <dd>
          <code>{site.domain || "—"}</code>
        </dd>
        <dt>
          <Trans>Reached</Trans>
        </dt>
        <dd>{reached}</dd>
        {standing ? (
          <>
            <dt>
              <Trans>Certificate</Trans>
            </dt>
            <dd>{standing.charAt(0).toUpperCase() + standing.slice(1)}</dd>
          </>
        ) : null}
      </dl>
    </Panel>
  );
}

/** Deleting a website, which a plan does, and an undo puts back. */
function DeletePanel({ site }: { site: WebsiteInfo }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const kind = kindOf(site.spec);
  return (
    <Panel title={<Trans>Delete this website</Trans>} className="website-delete">
      <p>
        {kind === "url" ? (
          <Trans>Its route and its web server go. Undo from Activity brings them back.</Trans>
        ) : (
          <Trans>Its route goes, and what listens on the port keeps running. Undo from Activity brings it back.</Trans>
        )}
      </p>
      <Button
        variant="danger"
        icon="trash"
        onClick={() =>
          sheet.open({
            kind: "plan",
            make: () => createClient(WebsiteService, transport).deleteWebsite({ id: site.id }),
            onClose: (after) => {
              if (after?.state === ChangeState.APPLIED || after?.state === ChangeState.APPLYING)
                void navigate({ to: "/websites" });
            },
            doneLabel: t`Go to the websites`,
          })
        }
      >
        <Trans>Delete website</Trans>
      </Button>
    </Panel>
  );
}
