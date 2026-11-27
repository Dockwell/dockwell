// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type CheckGitLabConnectionResponse,
  type GitLabConnectionInfo,
  GitLabConnectionState,
  GitProviderService,
} from "@dockwell/api";
import { BrandMark, Button, Callout, Sheet, StatusBadge, Switch, TextField } from "@dockwell/ui";
import { msg } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAllResources } from "../projects/useProjects";

function statusBadgeFor(state: GitLabConnectionState) {
  switch (state) {
    case GitLabConnectionState.MISSING_SCOPE:
      return { status: "degraded" as const, label: msg`Missing api scope` };
    case GitLabConnectionState.AUTH_FAILED:
      return { status: "down" as const, label: msg`Token refused` };
    case GitLabConnectionState.UNREACHABLE:
      return { status: "down" as const, label: msg`Unreachable` };
    default:
      return { status: "healthy" as const, label: msg`Connected` };
  }
}

/** Card for one saved GitLab connection in Settings -> Integrations -> Git. */
export function GitLabCard({
  connection,
  onManage,
  onChanged,
}: {
  connection: GitLabConnectionInfo;
  onManage: () => void;
  onChanged: () => void;
}) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayCheck = may(GitProviderService.method.checkGitLabConnection);
  const [checking, setChecking] = useState(false);
  const [problem, setProblem] = useState<string>();

  const badge = statusBadgeFor(connection.state);
  const host = connection.url.replace(/^https?:\/\//, "");
  const scopeLabel = connection.everyProject
    ? t`every project`
    : connection.projects.length === 1
      ? t`1 project`
      : t`${connection.projects.length} projects`;

  const check = async () => {
    setChecking(true);
    setProblem(undefined);
    try {
      await createClient(GitProviderService, transport).checkGitLabConnection({
        name: connection.name,
        url: connection.url,
      });
      onChanged();
    } catch (err) {
      setProblem(explain(err) || sentence(err));
    } finally {
      setChecking(false);
    }
  };

  return (
    <article className="dw-panel integrations-card" aria-label={`GitLab ${connection.name}`}>
      <div className="integrations-card-head">
        <BrandMark brand="gitlab" />
        <StatusBadge status={badge.status} label={i18n._(badge.label)} />
      </div>
      <div className="integrations-card-text">
        <h3 className="integrations-name">{connection.name}</h3>
        <p className="integrations-desc">
          <code>{connection.pathPrefix ? `${host}/${connection.pathPrefix}` : host}</code> · {scopeLabel}
          {connection.detail ? <span className="integrations-sub github-unreached">{connection.detail}</span> : null}
          {problem ? (
            <span className="integrations-error" role="alert">
              {problem}
            </span>
          ) : null}
        </p>
      </div>
      <div className="integrations-actions">
        <Button size="sm" variant="secondary" icon="settings" onClick={onManage} {...mayPlan.off}>
          <Trans>Manage</Trans>
        </Button>
        {mayCheck.allowed ? (
          <Button size="sm" variant="ghost" icon="refresh" loading={checking} onClick={() => void check()}>
            <Trans>Check</Trans>
          </Button>
        ) : null}
      </div>
    </article>
  );
}

/** Empty-state card inviting an admin to connect GitLab for commit statuses. */
export function GitLabConnectCard({ onConnect }: { onConnect: () => void }) {
  const { t } = useLingui();
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  return (
    <article className="dw-panel integrations-card" aria-label="GitLab">
      <div className="integrations-card-head">
        <BrandMark brand="gitlab" />
        <span className="settings-meta">{t`Not connected`}</span>
      </div>
      <div className="integrations-card-text">
        <h3 className="integrations-name">GitLab</h3>
        <p className="integrations-desc">{t`Commit statuses for GitLab CI deploys`}</p>
      </div>
      <div>
        <Button size="sm" icon="plus" onClick={onConnect} {...mayPlan.off}>
          <Trans>Connect</Trans>
        </Button>
      </div>
    </article>
  );
}

/** Connect or manage a GitLab connection as a normal reviewed change. */
export function GitLabSheet({
  editing,
  onClose,
  onChanged,
}: {
  editing?: GitLabConnectionInfo;
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const sheet = useChangeSheet();
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayCheck = may(GitProviderService.method.checkGitLabConnection);
  const resources = useAllResources();
  const projects = (resources.data ?? [])
    .filter((r) => r.resource?.kind === "project")
    .map((r) => ({ id: r.id, label: r.displayName || r.resource?.name || r.id }));

  const [name, setName] = useState(editing?.name ?? "gitlab");
  const [url, setUrl] = useState(editing?.url ?? "https://gitlab.com");
  const [token, setToken] = useState("");
  const [pathPrefix, setPathPrefix] = useState(editing?.pathPrefix ?? "");
  const [someProjects, setSomeProjects] = useState(editing ? !editing.everyProject : false);
  const [picked, setPicked] = useState<string[]>(editing?.projects ?? []);
  const [checking, setChecking] = useState(false);
  const [checked, setChecked] = useState<CheckGitLabConnectionResponse>();
  const [problem, setProblem] = useState<string>();
  const [removing, setRemoving] = useState(false);

  const trimmedName = name.trim();
  const trimmedURL = url.trim();
  const trimmedToken = token.trim();
  const trimmedPrefix = pathPrefix.trim();
  const urlChanged = editing ? trimmedURL.replace(/\/+$/, "") !== editing.url.replace(/\/+$/, "") : false;
  const needsToken = !editing || urlChanged;
  const ready =
    trimmedName !== "" &&
    trimmedURL !== "" &&
    (!needsToken || trimmedToken !== "") &&
    (!someProjects || picked.length > 0);

  const check = async () => {
    setChecking(true);
    setProblem(undefined);
    setChecked(undefined);
    try {
      const res = await createClient(GitProviderService, transport).checkGitLabConnection({
        name: editing?.name ?? "",
        url: trimmedURL,
        token: trimmedToken,
      });
      setChecked(res);
      if (editing) onChanged();
    } catch (err) {
      setProblem(explain(err) || sentence(err));
    } finally {
      setChecking(false);
    }
  };

  const review = () => {
    sheet.open({
      kind: "propose",
      summary: editing ? t`Update the GitLab connection ${trimmedName}` : t`Connect GitLab ${trimmedName}`,
      edits: [
        {
          resource: { kind: "gitlab.connection", name: trimmedName },
          spec: {
            kind: {
              case: "gitlabConnection",
              value: {
                instanceUrl: trimmedURL,
                token: trimmedToken,
                pathPrefix: trimmedPrefix,
                projects: someProjects ? picked : [],
              },
            },
          },
          ...(editing ? { baseRevision: editing.revision } : {}),
        },
      ],
      onClose: (change) => {
        if (change) {
          if (mayCheck.allowed) {
            void createClient(GitProviderService, transport)
              .checkGitLabConnection({ name: trimmedName, url: trimmedURL })
              .catch(() => {})
              .finally(() => onChanged());
          } else {
            onChanged();
          }
          onClose();
        }
      },
    });
  };

  const remove = () => {
    if (!editing) return;
    sheet.open({
      kind: "propose",
      summary: t`Remove the GitLab connection ${editing.name}`,
      edits: [
        {
          resource: { kind: "gitlab.connection", name: editing.name },
          delete: true,
          baseRevision: editing.revision,
        },
      ],
      onClose: (change) => {
        if (change) {
          onChanged();
          onClose();
        }
      },
    });
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`GitLab connection ${editing.name}` : t`Connect GitLab`}
      subtitle={t`Report CI deploy commit statuses to gitlab.com or self-managed GitLab`}
      footer={
        removing ? (
          <>
            <Button variant="secondary" onClick={() => setRemoving(false)}>
              <Trans>Keep it</Trans>
            </Button>
            <Button variant="danger" onClick={remove} {...mayPlan.off}>
              <Trans>Review removal</Trans>
            </Button>
          </>
        ) : (
          <>
            {editing ? (
              <Button variant="ghost" className="github-remove" onClick={() => setRemoving(true)} {...mayPlan.off}>
                <Trans>Remove</Trans>
              </Button>
            ) : null}
            <Button variant="secondary" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
            <Button variant="primary" disabled={!ready} onClick={review} {...mayPlan.off}>
              <Trans>Review plan</Trans>
            </Button>
          </>
        )
      }
    >
      <div className="github-manage">
        {removing ? (
          <Callout tone="warning" title={t`Remove this GitLab connection?`}>
            {t`Deploys from matching GitLab repositories won't report commit statuses until another connection covers them.`}
          </Callout>
        ) : null}

        <TextField
          label={t`Name`}
          value={name}
          onChange={(e) => setName(e.target.value)}
          disabled={Boolean(editing)}
          placeholder="gitlab"
          hint={t`Lowercase letters, digits and hyphens, such as gitlab or acme-gitlab.`}
        />

        <TextField
          label={t`GitLab URL`}
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://gitlab.com"
          hint={t`Use https://gitlab.com or your self-managed instance base URL.`}
        />

        <TextField
          label={t`Access token`}
          type="password"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          placeholder={editing && !urlChanged ? t`Leave blank to keep the saved token` : "glpat-..."}
          hint={
            urlChanged
              ? t`Enter a token for the new GitLab URL. Needs the api scope.`
              : t`Personal, group or project access token with the api scope.`
          }
        />

        <TextField
          label={t`Path prefix`}
          value={pathPrefix}
          onChange={(e) => setPathPrefix(e.target.value)}
          placeholder={t`Optional, such as acme or acme/platform`}
          hint={t`Leave blank to match every repository on this GitLab host.`}
        />

        <div className="github-manage-part">
          <Switch
            checked={someProjects}
            onChange={setSomeProjects}
            label={t`Only some projects`}
            description={t`Limit which projects' deploys can report statuses through this connection.`}
          />
          {someProjects ? (
            <fieldset className="github-project-list">
              <legend className="dw-sr">{t`Projects`}</legend>
              {projects.map((p) => (
                <label key={p.id} className="github-project">
                  <input
                    type="checkbox"
                    checked={picked.includes(p.id)}
                    onChange={(event) =>
                      setPicked((was) => (event.target.checked ? [...was, p.id] : was.filter((x) => x !== p.id)))
                    }
                  />
                  <span>{p.label}</span>
                </label>
              ))}
            </fieldset>
          ) : null}
        </div>

        <div className="integrations-actions">
          <Button
            size="sm"
            variant="secondary"
            icon="refresh"
            loading={checking}
            disabled={!trimmedURL || (needsToken && !trimmedToken)}
            onClick={() => void check()}
            {...mayCheck.off}
          >
            <Trans>Check connection</Trans>
          </Button>
        </div>

        {checked ? (
          checked.state === GitLabConnectionState.CONNECTED ? (
            <Callout tone="healthy" title={t`Connected to GitLab`}>
              {checked.tokenName
                ? t`Token ${checked.tokenName} can post commit statuses to ${checked.url}.`
                : t`This token can post commit statuses to ${checked.url}.`}
            </Callout>
          ) : (
            <Callout
              tone={checked.state === GitLabConnectionState.MISSING_SCOPE ? "warning" : "critical"}
              title={i18n._(statusBadgeFor(checked.state).label)}
            >
              {checked.detail}
            </Callout>
          )
        ) : null}

        {problem ? (
          <Callout tone="critical" title={t`Connection check failed`}>
            {problem}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
