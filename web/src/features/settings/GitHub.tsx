// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type GitHubAppInfo, GitProviderService, StepUpAction } from "@dockwell/api";
import { Button, ButtonLink, Callout, Icon, SegmentedControl, Sheet, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { StepUpDeclined, withStepUpIfAsked } from "../auth/stepUp";
import { ReadFailed } from "../shell/ReadFailed";
import { GitHubManageSheet, type Removed } from "./GitHubManage";
import { installUrl, useDeliveryWords, useGitHubApps } from "./github";
import "./GitHub.css";

/**
 * Integrations' Git section (Settings-Integrations.dc.html): GitHub, connected as an
 * App Dockwell makes in one click, so apps build from private repositories and deploy
 * on push with nothing to paste (ADR 0098). Its state is one line, and its actions
 * are Connect, then Manage.
 */
export function GitHubSection() {
  const { t } = useLingui();
  const may = useMay();
  const apps = useGitHubApps();
  const mayConnect = may(GitProviderService.method.startGitHubApp);
  const [connecting, setConnecting] = useState(false);
  const [managing, setManaging] = useState<string>();
  // What removing an App left to do on GitHub, which keeps it until it's deleted there.
  const [removed, setRemoved] = useState<Removed>();
  const list = apps.data?.apps ?? [];
  const managed = list.find((a) => a.id === managing);
  // As many of the page's columns as its cards, as the page lays out each section (IntegrationsPage, #786).
  const cards = String(Math.min(4, Math.max(1, list.length)));
  const live = list.filter((a) => !a.gone).length;
  return (
    <section className="integrations-section" aria-labelledby="integrations-git" data-cards={cards}>
      <div className="integrations-head">
        <h2 id="integrations-git" className="dw-text-heading">
          <Trans>Git</Trans>
        </h2>
        {live > 0 ? <span className="integrations-count">{live === 1 ? t`1 app` : t`${live} apps`}</span> : null}
      </div>
      {apps.error && !apps.data ? (
        <ReadFailed
          error={apps.error}
          title={t`Dockwell can't list its GitHub Apps`}
          actions={
            <Button size="sm" icon="refresh" loading={apps.isFetching} onClick={() => void apps.refetch()}>
              <Trans>Try again</Trans>
            </Button>
          }
        />
      ) : !apps.data ? (
        <div className="integrations-grid" aria-busy="true">
          <div className="dw-panel integrations-card integrations-skeleton" />
          <span className="dw-sr">{t`Loading GitHub Apps`}</span>
        </div>
      ) : (
        <div className="integrations-grid">
          {list.map((app) => (
            <GitHubCard
              key={app.id}
              app={app}
              onManage={() => setManaging(app.id)}
              onConnect={() => setConnecting(true)}
              onChanged={() => void apps.refetch()}
            />
          ))}
          {list.length > 0 ? null : (
            <article className="dw-panel integrations-card" aria-label="GitHub">
              <div className="integrations-card-head">
                <span className="integrations-mark" aria-hidden="true">
                  <Icon name="branch" size={16} />
                </span>
                <span className="settings-meta">{t`Not connected`}</span>
              </div>
              <div className="integrations-card-text">
                <h3 className="integrations-name">GitHub</h3>
                <p className="integrations-desc">{t`Private repositories, and deploy on push`}</p>
              </div>
              <div>
                <Button size="sm" icon="plus" onClick={() => setConnecting(true)} {...mayConnect.off}>
                  <Trans>Connect</Trans>
                </Button>
              </div>
            </article>
          )}
        </div>
      )}
      {removed ? (
        <Callout
          tone="warning"
          title={t`GitHub keeps the App until you delete it there`}
          actions={
            <>
              <ButtonLink
                size="sm"
                variant="secondary"
                icon="external"
                href={removed.deleteUrl}
                target="_blank"
                rel="noreferrer"
              >
                {t`Delete it on GitHub`}
              </ButtonLink>
              <Button size="sm" variant="ghost" onClick={() => setRemoved(undefined)}>
                <Trans>Done</Trans>
              </Button>
            </>
          }
        >
          {removed.uninstalled === 1
            ? t`Dockwell uninstalled it from 1 account and forgot its key.`
            : t`Dockwell uninstalled it from ${removed.uninstalled} accounts and forgot its key.`}
        </Callout>
      ) : null}
      {connecting ? <ConnectGitHubSheet onClose={() => setConnecting(false)} /> : null}
      {managed ? (
        <GitHubManageSheet
          app={managed}
          onClose={() => setManaging(undefined)}
          onChanged={() => void apps.refetch()}
          onRemoved={(r) => {
            setRemoved(r);
            void apps.refetch();
          }}
        />
      ) : null}
    </section>
  );
}

/** A connected App: where it's installed, how many repositories that reaches, and whether GitHub reaches the panel. */
function GitHubCard({
  app,
  onManage,
  onConnect,
  onChanged,
}: {
  app: GitHubAppInfo;
  onManage: () => void;
  onConnect: () => void;
  /** Reads the Apps again, once something about this one changed. */
  onChanged: () => void;
}) {
  const { t } = useLingui();
  const may = useMay();
  const words = useDeliveryWords();
  const mayManage = may(GitProviderService.method.rotateGitHubWebhookSecret);
  const mayConnect = may(GitProviderService.method.startGitHubApp);
  const installed = app.installations.filter((i) => !i.removed);
  const accounts = installed.map((i) => i.account).join(", ");
  const repositories = installed.reduce((n, i) => n + i.repositoryCount, 0);
  const delivery = words(app.delivery);
  return (
    <article className="dw-panel integrations-card" aria-label="GitHub">
      <div className="integrations-card-head">
        <span className="integrations-mark" aria-hidden="true">
          <Icon name="branch" size={16} />
        </span>
        {app.gone ? (
          <StatusBadge status="down" label={t`Deleted on GitHub`} />
        ) : (
          <StatusBadge status="healthy" label={t`Connected`} />
        )}
      </div>
      <div className="integrations-card-text">
        <h3 className="integrations-name">GitHub</h3>
        <p className="integrations-desc">
          {app.gone ? (
            t`The GitHub App was deleted on GitHub.`
          ) : installed.length === 0 ? (
            t`The GitHub App isn't installed on an account yet.`
          ) : repositories === 1 ? (
            <Trans>
              GitHub App on <code>{accounts}</code> · 1 repository
            </Trans>
          ) : repositories > 0 ? (
            <Trans>
              GitHub App on <code>{accounts}</code> · {repositories} repositories
            </Trans>
          ) : (
            <Trans>
              GitHub App on <code>{accounts}</code>
            </Trans>
          )}
          {!app.gone && delivery.unreached ? (
            <span className="integrations-sub github-unreached">
              {delivery.text}{" "}
              <Link to="/settings/general" className="cp-link">
                {t`Give the panel a domain`}
              </Link>
            </span>
          ) : null}
          {!app.gone && delivery.refused ? (
            <span className="integrations-sub github-unreached">{delivery.text}</span>
          ) : null}
        </p>
      </div>
      <div className="integrations-actions">
        {app.gone ? (
          <Button size="sm" icon="refresh" onClick={onConnect} {...mayConnect.off}>
            <Trans>Connect again</Trans>
          </Button>
        ) : (
          <>
            <Button size="sm" variant="secondary" icon="settings" onClick={onManage} {...mayManage.off}>
              <Trans>Manage</Trans>
            </Button>
            {delivery.refused && mayManage.allowed ? <NewSecretButton app={app} onChanged={onChanged} /> : null}
            {installed.length === 0 && mayManage.allowed ? (
              <ButtonLink size="sm" variant="ghost" icon="external" href={installUrl(app)}>
                {t`Install on GitHub`}
              </ButtonLink>
            ) : null}
          </>
        )}
      </div>
    </article>
  );
}

/**
 * Gives the App a new webhook secret, with a passkey, where the panel refuses
 * GitHub's pushes for theirs: the one action that ends it, beside the line that
 * says so.
 */
function NewSecretButton({ app, onChanged }: { app: GitHubAppInfo; onChanged: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const give = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await withStepUpIfAsked(transport, StepUpAction.ROTATE_GITHUB_APP, app.id, (proof) =>
        createClient(GitProviderService, transport).rotateGitHubWebhookSecret({ id: app.id, proof }),
      );
      onChanged();
    } catch (err) {
      if (!(err instanceof StepUpDeclined)) setProblem(explain(err) || sentence(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <Button size="sm" variant="ghost" icon="key" loading={busy} onClick={() => void give()}>
        <Trans>New webhook secret</Trans>
      </Button>
      {problem ? (
        <span className="integrations-error" role="alert">
          {t`The webhook secret didn't change.`} {problem}
        </span>
      ) : null}
    </>
  );
}

/** Posts the App's manifest to GitHub's page that makes it, as a form, which leaves the panel for GitHub. */
function postManifest(action: string, manifest: string) {
  const form = document.createElement("form");
  form.method = "post";
  form.action = action;
  const field = document.createElement("input");
  field.type = "hidden";
  field.name = "manifest";
  field.value = manifest;
  form.append(field);
  document.body.append(form);
  form.submit();
}

/**
 * Connect GitHub: one choice, your account or an organization by name, then one
 * button, Create on GitHub, which asks for your passkey and posts the App's manifest
 * to GitHub. GitHub makes the App and sends the browser back (ADR 0098).
 */
function ConnectGitHubSheet({ onClose }: { onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const mayConnect = may(GitProviderService.method.startGitHubApp);
  const [owner, setOwner] = useState<"me" | "organization">("me");
  const [organization, setOrganization] = useState("");
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const named = organization.trim();
  const ready = owner === "me" || /^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/.test(named);

  const create = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      const org = owner === "me" ? "" : named;
      // Your browser asks for your passkey, to confirm it's you, once nothing else stops it.
      const started = await withStepUpIfAsked(transport, StepUpAction.CONNECT_GITHUB_APP, "", (proof) =>
        createClient(GitProviderService, transport).startGitHubApp({ organization: org, proof }),
      );
      postManifest(started.actionUrl, started.manifest);
    } catch (err) {
      if (!(err instanceof StepUpDeclined)) setProblem(explain(err) || sentence(err));
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Connect GitHub`}
      subtitle={t`As a GitHub App Dockwell makes, which you install on the repositories you pick`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            icon="external"
            loading={busy}
            disabled={!ready}
            onClick={() => void create()}
            {...mayConnect.off}
          >
            <Trans>Create on GitHub</Trans>
          </Button>
        </>
      }
    >
      <div className="github-connect">
        <div className="app-field">
          <span className="dw-field-label" id="github-owner">
            <Trans>Owned by</Trans>
          </span>
          <SegmentedControl
            label={t`Owned by`}
            value={owner}
            onChange={(v) => setOwner(v as "me" | "organization")}
            options={[
              { value: "me", label: t`Your account` },
              { value: "organization", label: t`An organization` },
            ]}
          />
        </div>
        {owner === "organization" ? (
          <TextField
            label={t`Organization`}
            mono
            autoComplete="off"
            spellCheck={false}
            placeholder="acme"
            value={organization}
            hint={t`Its repositories need an App it owns.`}
            onChange={(event) => setOrganization(event.target.value)}
          />
        ) : null}
        {problem ? (
          <Callout tone="critical" title={t`GitHub isn't connected`}>
            {problem}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
