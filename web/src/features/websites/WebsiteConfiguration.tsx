// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { ChangeState, type WebsiteInfo, WebsiteService } from "@dockwell/api";
import { Button, cx, Glyph, Icon, Panel, PHONE_QUERY, SelectField, useMediaQuery } from "@dockwell/ui";
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
import {
  changed,
  type Missing,
  missing,
  refusedField,
  type Serves,
  type Settings,
  settingsOf,
  specOf,
} from "./settings";
import { useMissingWords, useServesWords, WasHint, WebsiteFields } from "./WebsiteFields";
import { useWebsiteValue } from "./WebsiteLayout";
import { changedLines, kindOf } from "./website";

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
 * A website's configuration, as Website-Detail lays it out: what it serves on the
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
  const missingWords = useMissingWords();
  const servesWords = useServesWords();
  const was = useMemo(() => settingsOf(site.spec), [site.spec]);
  const [now, setNow] = useState<Settings>(was);
  const settled = useSettled(now);
  const current = useQuery(WebsiteService.method.previewWebsite, { website: site.spec }, { enabled: !!site.spec });
  const gap = missing(settled);
  const preview = useQuery(
    WebsiteService.method.previewWebsite,
    { website: specOf(settled) },
    { enabled: gap === undefined, retry: false },
  );
  const refusal = gap ? missingWords(gap) : preview.error ? sentence(preview.error) : "";
  const refusedAt: Missing["field"] | "" =
    gap?.field ?? (preview.error ? fieldOf(sentence(preview.error), now.serves) : "");

  const changes = changed(was, now).length;
  const typing = JSON.stringify(now) !== JSON.stringify(settled);
  // What its web server runs differs from what these settings write, as a change made
  // outside Websites edited it: planning these settings puts them back in step.
  const differs = site.configDiffers;
  const ready = (changes > 0 || differs) && refusal === "" && !typing && preview.isSuccess;

  const review = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(WebsiteService, transport).updateWebsite({ id: site.id, website: specOf(now) }),
      retryLabel: t`Edit and try again`,
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED) setNow(settingsOf(site.spec));
      },
    });

  const servesHint: Record<Serves, string> = {
    folder: t`Its web server serves the files as they are, and an edit shows at once`,
    php: t`PHP-FPM runs its code beside a web server that serves its other files`,
    url: t`A small web server of its own sends each request on`,
    port: t`The ingress sends each request straight to the port`,
  };

  return (
    <div className="website-tab">
      <div className="website-columns">
        <div className="website-settings">
          <Panel title={<Trans>What it serves</Trans>}>
            <div className="website-fields">
              <SelectField
                label={t`Serves`}
                value={now.serves}
                options={(["folder", "php", "url", "port"] as const).map((value) => ({
                  value,
                  label: servesWords[value],
                }))}
                hint={
                  now.serves !== was.serves ? (
                    <WasHint>{t`Was ${servesWords[was.serves]}`}</WasHint>
                  ) : (
                    servesHint[now.serves]
                  )
                }
                onChange={(event) => setNow({ ...now, serves: event.target.value as Serves })}
              />
              <WebsiteFields
                settings={now}
                was={was}
                server={site.serverId}
                errors={{ [refusedAt || "none"]: refusal || undefined }}
                onChange={setNow}
              />
            </div>
          </Panel>
          <DomainPanel />
        </div>
        <Panel className={cx("website-config")} flush>
          {now.serves !== "port" ? (
            <ConfigView
              before={site.config || (current.data?.config ?? "")}
              after={preview.data?.config ?? (refusal ? "" : site.config || (current.data?.config ?? ""))}
              showChanges={changes > 0 || differs}
              loading={!site.config && current.isPending && !!site.spec}
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
              ) : differs && changes === 0 ? (
                <>
                  <span className="dw-tone-warning">
                    <Glyph shape="triangle" size={10} />
                  </span>
                  <strong>
                    <Trans>Differs</Trans>
                  </strong>
                  <span className="websites-muted">
                    {" · "}
                    <Trans>
                      its web server runs a config these settings don't write, as a change edited it outside Websites.
                      Review plan puts them back.
                    </Trans>
                  </span>
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

/** The field a refusal of the server's is about: the one it names, or the kind's main one. */
function fieldOf(said: string, serves: Serves): Missing["field"] {
  return refusedField(said, serves) || (serves === "folder" || serves === "php" ? "root" : serves);
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
export function DomainPanel() {
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
        {kind === "folder" ? (
          <Trans>
            Its route and its web server go, and the folder stays on the server as it is. Undo from Activity brings them
            back.
          </Trans>
        ) : kind === "php" ? (
          <Trans>
            Its route, its web server and PHP go, and the folder stays on the server with what PHP wrote there. Undo
            from Activity brings them back.
          </Trans>
        ) : kind === "url" ? (
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
