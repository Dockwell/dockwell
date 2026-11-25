// SPDX-License-Identifier: AGPL-3.0-only

import { Advanced, Button, Callout, Panel, SelectField, Switch, Tag, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { useServices } from "../apps/useServices";
import type { BasicAuthUserDraft, HeaderDraft, PathRuleDraft, Settings, SubpathProxyDraft } from "./settings";

let draftSeq = 0;
function nextDraftId(prefix: string): string {
  draftSeq += 1;
  return `${prefix}-new-${draftSeq}`;
}

function randomPassword(): string {
  const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";
  const bytes = new Uint8Array(20);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => alphabet[b % alphabet.length]).join("");
}

/**
 * Structured HTTP rules for a website: Basic Auth, CORS, rate limit, redirects/rewrites,
 * sub-path proxies, response headers, and static file caching, plus a collapsed Advanced
 * block for site-scoped custom Caddy directives.
 */
export function RulesEditor({
  settings: s,
  serverId,
  rulesError,
  customCaddyError,
  customCaddyValid,
  onChange,
}: {
  settings: Settings;
  serverId: string;
  rulesError?: string;
  customCaddyError?: string;
  customCaddyValid?: boolean;
  onChange: (next: Settings) => void;
}) {
  const { t } = useLingui();
  const services = useServices();
  const serverServices = (services.data ?? []).filter((svc) => {
    const name = svc.resource?.name ?? "";
    return svc.deployments.some((d) => d.serverId === serverId) && name !== "" && name !== "web" && name !== "php";
  });
  const isPort = s.serves === "port";

  const updateUser = (index: number, patch: Partial<BasicAuthUserDraft>) => {
    const next = s.basicAuthUsers.map((u, i) => (i === index ? { ...u, ...patch } : u));
    onChange({ ...s, basicAuthUsers: next });
  };
  const addUser = () =>
    onChange({
      ...s,
      basicAuthEnabled: true,
      basicAuthUsers: [
        ...s.basicAuthUsers,
        { id: nextDraftId("ba"), username: "", secret: "", password: randomPassword() },
      ],
    });
  const removeUser = (index: number) => {
    const next = s.basicAuthUsers.filter((_, i) => i !== index);
    onChange({ ...s, basicAuthUsers: next, basicAuthEnabled: next.length > 0 });
  };

  const updatePathRule = (index: number, patch: Partial<PathRuleDraft>) =>
    onChange({ ...s, pathRules: s.pathRules.map((r, i) => (i === index ? { ...r, ...patch } : r)) });
  const addPathRule = () =>
    onChange({
      ...s,
      pathRules: [...s.pathRules, { id: nextDraftId("pr"), from: "", to: "", kind: "redirect", permanent: true }],
    });
  const removePathRule = (index: number) => onChange({ ...s, pathRules: s.pathRules.filter((_, i) => i !== index) });

  const updateSubpath = (index: number, patch: Partial<SubpathProxyDraft>) =>
    onChange({ ...s, subpathProxies: s.subpathProxies.map((sp, i) => (i === index ? { ...sp, ...patch } : sp)) });
  const addSubpath = () =>
    onChange({
      ...s,
      subpathProxies: [
        ...s.subpathProxies,
        {
          id: nextDraftId("sp"),
          path: "/api",
          targetKind: "url",
          url: "",
          serviceId: "",
          servicePort: "8080",
          stripPrefix: false,
          sendTargetHost: false,
        },
      ],
    });
  const removeSubpath = (index: number) =>
    onChange({ ...s, subpathProxies: s.subpathProxies.filter((_, i) => i !== index) });

  const updateHeader = (index: number, patch: Partial<HeaderDraft>) =>
    onChange({ ...s, headers: s.headers.map((h, i) => (i === index ? { ...h, ...patch } : h)) });
  const addHeader = () =>
    onChange({ ...s, headers: [...s.headers, { id: nextDraftId("hd"), name: "", value: "", remove: false }] });
  const removeHeader = (index: number) => onChange({ ...s, headers: s.headers.filter((_, i) => i !== index) });

  const activeCount = [
    s.basicAuthEnabled && s.basicAuthUsers.length > 0,
    s.corsEnabled,
    s.rateLimitEnabled,
    s.pathRules.length > 0,
    s.subpathProxies.length > 0,
    s.headers.length > 0,
    s.staticCacheEnabled,
  ].filter(Boolean).length;

  return (
    <Panel
      title={<Trans>Rules</Trans>}
      actions={
        activeCount > 0 ? (
          <Tag tone="cobalt" words>
            {t`${activeCount} active`}
          </Tag>
        ) : undefined
      }
    >
      <div className="website-fields">
        {rulesError ? (
          <Callout tone="critical" title={t`Rule needs attention`}>
            {rulesError}
          </Callout>
        ) : null}

        {/* Rate limit works on every website kind including port proxies */}
        <div className="website-rule-block">
          <Switch
            label={t`Rate limit`}
            description={t`Cap requests per minute from each visitor address, returning 429 above the limit.`}
            checked={s.rateLimitEnabled}
            onChange={(on) => onChange({ ...s, rateLimitEnabled: on })}
          />
          {s.rateLimitEnabled ? (
            <div className="website-rule-row">
              <TextField
                label={t`Requests per minute`}
                inputMode="numeric"
                value={s.rateLimitRpm}
                placeholder="60"
                onChange={(e) => onChange({ ...s, rateLimitRpm: e.target.value })}
              />
              <TextField
                label={t`Burst`}
                inputMode="numeric"
                value={s.rateLimitBurst}
                placeholder="20"
                onChange={(e) => onChange({ ...s, rateLimitBurst: e.target.value })}
              />
            </div>
          ) : null}
        </div>

        {!isPort ? (
          <>
            {/* Basic Auth */}
            <div className="website-rule-block">
              <Switch
                label={t`Basic Auth`}
                description={t`Require a username and password. Passwords are stored as Dockwell secrets and hashed for Caddy.`}
                checked={s.basicAuthEnabled}
                onChange={(on) =>
                  onChange({
                    ...s,
                    basicAuthEnabled: on,
                    basicAuthUsers:
                      on && s.basicAuthUsers.length === 0
                        ? [{ id: nextDraftId("ba"), username: "", secret: "", password: randomPassword() }]
                        : s.basicAuthUsers,
                  })
                }
              />
              {s.basicAuthEnabled ? (
                <div className="website-rule-nested">
                  <div className="website-rule-row">
                    <TextField
                      label={t`Path prefix`}
                      mono
                      placeholder="/"
                      value={s.basicAuthPath}
                      hint={t`Empty protects the whole website.`}
                      onChange={(e) => onChange({ ...s, basicAuthPath: e.target.value })}
                    />
                    <TextField
                      label={t`Prompt label`}
                      placeholder={t`Restricted`}
                      value={s.basicAuthRealm}
                      onChange={(e) => onChange({ ...s, basicAuthRealm: e.target.value })}
                    />
                  </div>
                  {s.basicAuthUsers.map((u, idx) => (
                    <div key={u.id} className="website-rule-item">
                      <TextField
                        label={t`Username`}
                        mono
                        value={u.username}
                        placeholder="editor"
                        onChange={(e) => updateUser(idx, { username: e.target.value })}
                      />
                      <TextField
                        label={u.secret ? t`New password (optional)` : t`Password`}
                        type="password"
                        value={u.password}
                        placeholder={u.secret ? t`Saved as Dockwell secret` : t`Enter password`}
                        onChange={(e) => updateUser(idx, { password: e.target.value })}
                      />
                      <div className="website-rule-item-actions">
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => updateUser(idx, { password: randomPassword() })}
                        >
                          <Trans>Generate</Trans>
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          icon="trash"
                          aria-label={t`Remove user`}
                          onClick={() => removeUser(idx)}
                        />
                      </div>
                    </div>
                  ))}
                  <Button size="sm" variant="secondary" icon="plus" onClick={addUser}>
                    <Trans>Add user</Trans>
                  </Button>
                </div>
              ) : null}
            </div>

            {/* CORS */}
            <div className="website-rule-block">
              <Switch
                label={t`CORS`}
                description={t`Allow cross-origin browser requests from specific origins.`}
                checked={s.corsEnabled}
                onChange={(on) => onChange({ ...s, corsEnabled: on })}
              />
              {s.corsEnabled ? (
                <div className="website-rule-nested">
                  <label className="website-textarea-field">
                    <span className="dw-label">
                      <Trans>Allowed origins</Trans>
                    </span>
                    <textarea
                      className="website-textarea"
                      rows={2}
                      placeholder="https://app.example.com"
                      value={s.corsOrigins}
                      onChange={(e) => onChange({ ...s, corsOrigins: e.target.value })}
                    />
                    <span className="websites-muted">
                      <Trans>One origin per line, or * for any origin.</Trans>
                    </span>
                  </label>
                  <div className="website-rule-row">
                    <TextField
                      label={t`Allowed methods`}
                      mono
                      placeholder="GET, POST, PUT, PATCH, DELETE, OPTIONS"
                      value={s.corsMethods}
                      onChange={(e) => onChange({ ...s, corsMethods: e.target.value })}
                    />
                    <TextField
                      label={t`Preflight cache (seconds)`}
                      inputMode="numeric"
                      placeholder="3600"
                      value={s.corsMaxAge}
                      onChange={(e) => onChange({ ...s, corsMaxAge: e.target.value })}
                    />
                  </div>
                  <Switch
                    label={t`Allow credentials`}
                    description={t`Send Access-Control-Allow-Credentials: true. Not allowed with *.`}
                    checked={s.corsCredentials}
                    onChange={(on) => onChange({ ...s, corsCredentials: on })}
                  />
                </div>
              ) : null}
            </div>

            {/* Redirects & rewrites */}
            <div className="website-rule-block">
              <div className="website-rule-head">
                <div>
                  <strong>
                    <Trans>Redirects and rewrites</Trans>
                  </strong>
                  <p className="websites-muted">
                    <Trans>Send visitors from an old path to a new path, or rewrite internally.</Trans>
                  </p>
                </div>
                <Button size="sm" variant="secondary" icon="plus" onClick={addPathRule}>
                  <Trans>Add rule</Trans>
                </Button>
              </div>
              {s.pathRules.map((r, idx) => (
                <div key={r.id} className="website-rule-item">
                  <div className="website-rule-row">
                    <TextField
                      label={t`From`}
                      mono
                      placeholder="/old/*"
                      value={r.from}
                      onChange={(e) => updatePathRule(idx, { from: e.target.value })}
                    />
                    <TextField
                      label={t`To`}
                      mono
                      placeholder="/new/*"
                      value={r.to}
                      onChange={(e) => updatePathRule(idx, { to: e.target.value })}
                    />
                  </div>
                  <div className="website-rule-row">
                    <SelectField
                      label={t`Action`}
                      value={r.kind}
                      options={[
                        { value: "redirect", label: t`Redirect` },
                        { value: "rewrite", label: t`Rewrite` },
                      ]}
                      onChange={(e) => updatePathRule(idx, { kind: e.target.value as "redirect" | "rewrite" })}
                    />
                    {r.kind === "redirect" ? (
                      <SelectField
                        label={t`Status`}
                        value={r.permanent ? "301" : "302"}
                        options={[
                          { value: "301", label: t`Permanent (301)` },
                          { value: "302", label: t`Temporary (302)` },
                        ]}
                        onChange={(e) => updatePathRule(idx, { permanent: e.target.value === "301" })}
                      />
                    ) : null}
                    <Button
                      size="sm"
                      variant="ghost"
                      icon="trash"
                      aria-label={t`Remove path rule`}
                      onClick={() => removePathRule(idx)}
                    />
                  </div>
                </div>
              ))}
            </div>

            {/* Sub-path proxy */}
            <div className="website-rule-block">
              <div className="website-rule-head">
                <div>
                  <strong>
                    <Trans>Sub-path proxies</Trans>
                  </strong>
                  <p className="websites-muted">
                    <Trans>Forward a path such as /api to another app or address.</Trans>
                  </p>
                </div>
                <Button size="sm" variant="secondary" icon="plus" onClick={addSubpath}>
                  <Trans>Add sub-path</Trans>
                </Button>
              </div>
              {s.subpathProxies.map((sp, idx) => (
                <div key={sp.id} className="website-rule-item">
                  <div className="website-rule-row">
                    <TextField
                      label={t`Path`}
                      mono
                      placeholder="/api"
                      value={sp.path}
                      onChange={(e) => updateSubpath(idx, { path: e.target.value })}
                    />
                    <SelectField
                      label={t`Target`}
                      value={sp.targetKind}
                      options={[
                        { value: "url", label: t`Address (URL)` },
                        ...(serverServices.length > 0 ? [{ value: "service", label: t`App on this server` }] : []),
                      ]}
                      onChange={(e) =>
                        updateSubpath(idx, {
                          targetKind: e.target.value as "url" | "service",
                          serviceId: e.target.value === "service" ? sp.serviceId || serverServices[0]?.id || "" : "",
                        })
                      }
                    />
                  </div>
                  {sp.targetKind === "url" ? (
                    <TextField
                      label={t`Target URL`}
                      mono
                      placeholder="http://192.0.2.10:3000"
                      value={sp.url}
                      onChange={(e) => updateSubpath(idx, { url: e.target.value })}
                    />
                  ) : (
                    <div className="website-rule-row">
                      <SelectField
                        label={t`Service`}
                        value={sp.serviceId}
                        options={serverServices.map((svc) => ({ value: svc.id, label: svc.resource?.name ?? svc.id }))}
                        onChange={(e) => updateSubpath(idx, { serviceId: e.target.value })}
                      />
                      <TextField
                        label={t`Port`}
                        mono
                        inputMode="numeric"
                        placeholder="8080"
                        value={sp.servicePort}
                        onChange={(e) => updateSubpath(idx, { servicePort: e.target.value })}
                      />
                    </div>
                  )}
                  <div className="website-rule-row">
                    <Switch
                      label={t`Strip path prefix`}
                      checked={sp.stripPrefix}
                      onChange={(on) => updateSubpath(idx, { stripPrefix: on })}
                    />
                    <Button
                      size="sm"
                      variant="ghost"
                      icon="trash"
                      aria-label={t`Remove sub-path proxy`}
                      onClick={() => removeSubpath(idx)}
                    />
                  </div>
                </div>
              ))}
            </div>

            {/* Extra response headers */}
            <div className="website-rule-block">
              <div className="website-rule-head">
                <div>
                  <strong>
                    <Trans>Response headers</Trans>
                  </strong>
                  <p className="websites-muted">
                    <Trans>Add or remove HTTP response headers for visitors.</Trans>
                  </p>
                </div>
                <Button size="sm" variant="secondary" icon="plus" onClick={addHeader}>
                  <Trans>Add header</Trans>
                </Button>
              </div>
              {s.headers.map((h, idx) => (
                <div key={h.id} className="website-rule-item">
                  <div className="website-rule-row">
                    <TextField
                      label={t`Header name`}
                      mono
                      placeholder="X-Frame-Options"
                      value={h.name}
                      onChange={(e) => updateHeader(idx, { name: e.target.value })}
                    />
                    {!h.remove ? (
                      <TextField
                        label={t`Value`}
                        mono
                        placeholder="SAMEORIGIN"
                        value={h.value}
                        onChange={(e) => updateHeader(idx, { value: e.target.value })}
                      />
                    ) : null}
                  </div>
                  <div className="website-rule-row">
                    <Switch
                      label={t`Remove header`}
                      checked={h.remove}
                      onChange={(on) => updateHeader(idx, { remove: on })}
                    />
                    <Button
                      size="sm"
                      variant="ghost"
                      icon="trash"
                      aria-label={t`Remove header rule`}
                      onClick={() => removeHeader(idx)}
                    />
                  </div>
                </div>
              ))}
            </div>

            {/* Static file caching */}
            <div className="website-rule-block">
              <Switch
                label={t`Static file caching`}
                description={t`Set Cache-Control headers on images, stylesheets, scripts and fonts.`}
                checked={s.staticCacheEnabled}
                onChange={(on) => onChange({ ...s, staticCacheEnabled: on })}
              />
              {s.staticCacheEnabled ? (
                <div className="website-rule-nested">
                  <div className="website-rule-row">
                    <TextField
                      label={t`Max age (seconds)`}
                      inputMode="numeric"
                      placeholder="86400"
                      value={s.staticCacheMaxAge}
                      onChange={(e) => onChange({ ...s, staticCacheMaxAge: e.target.value })}
                    />
                    <TextField
                      label={t`Extensions`}
                      mono
                      placeholder=".css .js .png .webp .woff2"
                      value={s.staticCacheExtensions}
                      hint={t`Empty uses common web asset extensions.`}
                      onChange={(e) => onChange({ ...s, staticCacheExtensions: e.target.value })}
                    />
                  </div>
                  <Switch
                    label={t`Immutable`}
                    description={t`Add immutable for fingerprinted assets that never change at the same URL.`}
                    checked={s.staticCacheImmutable}
                    onChange={(on) => onChange({ ...s, staticCacheImmutable: on })}
                  />
                </div>
              ) : null}
            </div>

            {/* Advanced: custom site-scoped Caddy directives */}
            <Advanced
              label={t`Advanced Caddy config`}
              summary={
                s.customCaddy.trim() !== ""
                  ? t`Custom Caddy directives configured`
                  : t`Custom Caddy directives for this site block`
              }
              badge={
                s.customCaddy.trim() !== "" ? (
                  <Tag tone={customCaddyError ? undefined : "cobalt"} words>
                    {customCaddyError ? t`Invalid` : customCaddyValid ? t`Validated by Caddy` : t`Custom`}
                  </Tag>
                ) : null
              }
            >
              <div className="website-rule-nested">
                <p className="websites-muted">
                  <Trans>
                    Inserted inside this website's :8080 server block and validated by Caddy before any plan applies.
                    Global options, other hostnames and file imports are refused.
                  </Trans>
                </p>
                <label className="website-textarea-field">
                  <span className="dw-label">
                    <Trans>Custom Caddy directives</Trans>
                  </span>
                  <textarea
                    className="website-textarea is-mono"
                    rows={5}
                    spellCheck={false}
                    placeholder={'respond /healthz 200\nheader X-Robots-Tag "noindex"'}
                    value={s.customCaddy}
                    onChange={(e) => onChange({ ...s, customCaddy: e.target.value })}
                  />
                </label>
                {customCaddyError ? (
                  <Callout tone="critical" title={t`Caddy rejected this config`}>
                    {customCaddyError}
                  </Callout>
                ) : null}
              </div>
            </Advanced>
          </>
        ) : null}
      </div>
    </Panel>
  );
}
