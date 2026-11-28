// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type GitHook,
  type GitHookDelivery,
  GitHookOutcome,
  GitHookService,
  GitLabConnectionState,
  type GitSource,
  Role,
  StepUpAction,
} from "@dockwell/api";
import { Button, ButtonLink, Callout, Icon, Panel, Sheet, Switch, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { shownRef } from "../../lib/built";
import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedInUser } from "../auth/useAuthState";
import { useAgoNow } from "../servers/useFleet";
import {
  installationOf,
  useDeliveryWords,
  useGitHubApps,
  useGitLabProjects,
  useGitRepositories,
} from "../settings/github";
import { CopyButton } from "../system/CopyCommand";
import { useApp, useRevisions } from "./AppContext";
import { environmentOf, nameOf } from "./app";
import { shortCommit, shownRepository } from "./Build";
import { DeployKeySheet, useGitDeployKeys } from "./DeployKeyPanel";
import { deployKeyNameOf, type GitHost, gitHost, gitlabConnectionNameOf, hookOutOfReach } from "./gitHost";
import { sameRepository } from "./RepositoryField";
import { usePlaces } from "./useServices";

import "./Source.css";

/**
 * Where an app that builds from Git builds from (App-Overview's Source): its
 * repository and branch, the commit it runs, and whether a push deploys it.
 */
export function SourcePanel() {
  const { t } = useLingui();
  const { service } = useApp();
  const build = service.spec?.build;
  const source = build?.source.case === "git" ? build.source.value : undefined;
  if (!source) return null;
  const host = gitHost(source.url);
  return (
    <Panel title={t`Source`}>
      <div className="app-source">
        <dl className="app-facts">
          <dt>{t`Repository`}</dt>
          <dd className="app-source-repo">
            {host?.name ? (
              <span>
                {host.name} <code>{host.path}</code>
              </span>
            ) : (
              <code>{shownRepository(source.url)}</code>
            )}
            <Tag icon="branch">{shownRef(source.ref) || t`default branch`}</Tag>
            {build?.gitLogin ? (
              deployKeyNameOf(build.gitLogin) ? (
                <WithDeployKey login={build.gitLogin} />
              ) : gitlabConnectionNameOf(build.gitLogin) ? (
                <ThroughGitLab login={build.gitLogin} />
              ) : (
                <ThroughApp login={build.gitLogin} url={source.url} />
              )
            ) : null}
          </dd>
          <LastCommit serviceId={service.id} source={source} />
        </dl>
        <DeployOnPush source={source} host={host} />
      </div>
    </Panel>
  );
}

/**
 * Which project SSH deploy key a private repository is fetched with, with a
 * button to inspect, test or rotate it, and a warning when the host key changed
 * (ADR 0113).
 */
function WithDeployKey({ login }: { login: string }) {
  const { t } = useLingui();
  const { service } = useApp();
  const places = usePlaces();
  const keyName = deployKeyNameOf(login) ?? "";
  const project = places.place(environmentOf(service))?.scope ?? "";
  const keys = useGitDeployKeys(project);
  const found = keys.data?.keys.find((k) => k.name === keyName);
  const [open, setOpen] = useState(false);

  return (
    <>
      {found?.hostKeyChanged ? (
        <span className="app-source-line is-warning">
          <Icon name="alert" size={14} />
          <span>{t`The SSH host key changed, so fetches are stopped.`}</span>
          <button type="button" className="app-panel-link" onClick={() => setOpen(true)}>
            {t`Review deploy key`}
          </button>
        </span>
      ) : (
        <span className="app-source-line">
          <span className="app-muted">{t`with deploy key ${keyName}`}</span>
          {found ? (
            <button type="button" className="app-panel-link" onClick={() => setOpen(true)}>
              {t`Manage deploy key`}
            </button>
          ) : null}
        </span>
      )}
      {open && found ? (
        <DeployKeySheet deployKey={found} onChanged={() => void keys.refetch()} onClose={() => setOpen(false)} />
      ) : null}
    </>
  );
}

/**
 * Which GitLab connection a private repository is fetched through, and when the
 * connection was removed, narrowed away from this project, or failed auth, that
 * fact with a link to Settings -> Integrations (ADR 0113).
 */
function ThroughGitLab({ login }: { login: string }) {
  const { t } = useLingui();
  const { service } = useApp();
  const places = usePlaces();
  const connName = gitlabConnectionNameOf(login) ?? "";
  const providers = useGitHubApps().data;
  const project = places.place(environmentOf(service))?.scope ?? "";
  const gl = useGitLabProjects(project).data;
  const conn = providers?.gitlabConnections.find((c) => c.name === connName);
  const projectConn = gl?.connections.find((c) => c.name === connName);

  if (providers && !conn) {
    return (
      <span className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{t`The GitLab connection ${connName} was removed.`}</span>
        <Link to="/settings/integrations" className="app-panel-link">
          {t`Open Integrations`}
        </Link>
      </span>
    );
  }
  const state = conn?.state;
  if (
    state === GitLabConnectionState.AUTH_FAILED ||
    state === GitLabConnectionState.MISSING_SCOPE ||
    state === GitLabConnectionState.UNREACHABLE
  ) {
    return (
      <span className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{t`GitLab connection ${connName} can't read this repository right now.`}</span>
        <Link to="/settings/integrations" className="app-panel-link">
          {t`Open Integrations`}
        </Link>
      </span>
    );
  }
  if (gl && !projectConn) {
    return (
      <span className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{t`GitLab connection ${connName} doesn't reach this project any more.`}</span>
        <Link to="/settings/integrations" className="app-panel-link">
          {t`Open Integrations`}
        </Link>
      </span>
    );
  }
  return <span className="app-muted">{t`through GitLab connection ${connName}`}</span>;
}

/**
 * Which GitHub App installation a private repository is fetched through, and where
 * it can't any more, that fact with the way to fix it on GitHub (ADR 0098).
 */
function ThroughApp({ login, url }: { login: string; url: string }) {
  const { t } = useLingui();
  const { service } = useApp();
  const places = usePlaces();
  const apps = useGitHubApps().data?.apps;
  const project = places.place(environmentOf(service))?.scope ?? "";
  const reached = useGitRepositories(project);
  const found = installationOf(apps, login);
  const account = found?.installation?.account ?? "";
  if (found?.app.gone) {
    return (
      <span className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{t`The GitHub App was deleted on GitHub.`}</span>
        <Link to="/settings/integrations" className="app-panel-link">
          {t`Connect again`}
        </Link>
      </span>
    );
  }
  const listed = reached.data?.repositories;
  if (listed && !listed.some((r) => sameRepository(r.url, url))) {
    const name = gitHost(url)?.path || shownRepository(url);
    return (
      <span className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{t`The GitHub App can't reach ${name} any more.`}</span>
        {found?.installation?.settingsUrl ? (
          <a className="app-panel-link" href={found.installation.settingsUrl} target="_blank" rel="noreferrer">
            {t`Add it on GitHub`}
          </a>
        ) : null}
      </span>
    );
  }
  return (
    <span className="app-muted">{account ? t`through the GitHub App on ${account}` : t`through the GitHub App`}</span>
  );
}

/** The commit the app's build is of, as its build read it. */
function LastCommit({ serviceId, source }: { serviceId: string; source: GitSource }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const { revisions } = useRevisions(serviceId);
  const build = source.commit ? revisions?.find((r) => r.build?.commit === source.commit)?.build : undefined;
  const ended = build?.endTime ?? build?.startTime;
  const when = ended ? timestampDate(ended) : undefined;
  const now = useAgoNow(when);
  if (!source.commit) return null;
  return (
    <>
      <dt>{t`Last commit`}</dt>
      <dd className="app-source-commit">
        {build?.commitSubject ? <span className="app-source-subject">{build.commitSubject}</span> : null}
        <span className="app-muted">
          {[shortCommit(source.commit), build?.commitAuthor ?? "", when ? time.ago(when, now) : ""]
            .filter(Boolean)
            .join(" · ")}
        </span>
      </dd>
    </>
  );
}

/** The hook's sheet, open: with its secret just after it was made, or given a new one, which is shown only then. */
interface Open {
  secret?: string;
  rotated?: boolean;
}

/**
 * The Deploy on push switch (ADR 0046): a hook the repository's host sends each
 * push to, signed with a secret only you and the host have. Turning it on asks
 * for your passkey and opens the hook's sheet, with the secret, once; turning
 * it off needs none. Under it, the hook's state is one line.
 */
function DeployOnPush({ source, host }: { source: GitSource; host: GitHost | undefined }) {
  const { t } = useLingui();
  const { service } = useApp();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const query = useQuery(GitHookService.method.getGitHook, { id: service.id });
  const hook = query.data?.hook;
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const [open, setOpen] = useState<Open>();
  const [turnedOff, setTurnedOff] = useState(false);
  const name = nameOf(service);
  const branch = shownRef(source.ref) || t`the default branch`;
  const on = hook !== undefined;
  const where = { environment: environmentOf(service) };
  // A hook acts as a developer at most, so where the environment is protected a
  // push plans its deploy for a person to apply, unless its maker let it deploy
  // without waiting (ADR 0071).
  const guarded = usePlaces().place(where.environment)?.protected ?? false;
  const pushes =
    guarded && hook?.role !== Role.ADMIN
      ? t`Each push to ${branch} plans a deploy of ${name}, which waits for a person here.`
      : t`Each push to ${branch} deploys ${name}.`;
  const mayTurnOn = may(GitHookService.method.createGitHook, where);
  const mayTurnOff = may(GitHookService.method.deleteGitHook, where);
  const offer = on ? mayTurnOff : mayTurnOn;

  const turn = async (next: boolean) => {
    setBusy(true);
    setProblem(undefined);
    setTurnedOff(false);
    const hooks = createClient(GitHookService, transport);
    try {
      if (next) {
        // Your browser asks for your passkey, to confirm it's you, once nothing else stops it.
        const res = await withStepUpIfAsked(transport, StepUpAction.CREATE_GIT_HOOK, service.id, (proof) =>
          hooks.createGitHook({ id: service.id, proof }),
        );
        // Pushes through a GitHub App need nothing set on the repository: no sheet, no secret (ADR 0098).
        if (!res.hook?.githubApp) setOpen({ secret: res.secret });
      } else {
        await hooks.deleteGitHook({ id: service.id });
        // A hook through a GitHub App has nothing on the repository to remove.
        setTurnedOff(!hook?.githubApp);
      }
    } catch (err) {
      setProblem(explain(err) || sentence(err));
    } finally {
      setBusy(false);
      void query.refetch();
    }
  };

  return (
    <div className="app-source-push">
      <Switch
        checked={on}
        disabled={busy || query.isPending || !offer.allowed}
        onChange={(next) => void turn(next)}
        label={t`Deploy on push`}
        description={offer.reason || (on ? pushes : t`Pushes to ${branch} don't deploy ${name}.`)}
      />
      {hook ? <HookLine hook={hook} source={source} host={host} onOpen={() => setOpen({})} /> : null}
      {hook?.githubApp ? (
        // Pushes through a GitHub App have no sheet: whether they wait is chosen here.
        <WithoutWaiting hook={hook} guarded={guarded} onChanged={() => void query.refetch()} />
      ) : null}
      {!hook && turnedOff && host ? (
        <p className="app-source-line">
          <span className="app-muted">{t`The hook now refuses pushes. Remove it from the repository too.`}</span>
          <a className="app-panel-link" href={host.hooksPage.replace(/\/new$/, "")} target="_blank" rel="noreferrer">
            {t`Open webhooks`}
          </a>
        </p>
      ) : null}
      {problem ? (
        <Callout tone="critical" title={on ? t`Deploy on push didn't change` : t`Deploy on push isn't on`}>
          {problem}
        </Callout>
      ) : null}
      {open && hook ? (
        <HookSheet
          hook={hook}
          open={open}
          host={host}
          guarded={guarded}
          onOpen={setOpen}
          onChanged={() => void query.refetch()}
          onClose={() => setOpen(undefined)}
        />
      ) : null}
    </div>
  );
}

/**
 * The hook's state in one line, with its action: that its host can't reach it,
 * that no push has come, or what the last one came to.
 */
function HookLine({
  hook,
  source,
  host,
  onOpen,
}: {
  hook: GitHook;
  source: GitSource;
  host: GitHost | undefined;
  onOpen: () => void;
}) {
  const { t } = useLingui();
  const last = hook.deliveries[0];
  if (hook.githubApp) return <AppHookLine hook={hook} />;
  if (hookOutOfReach(hook.url, source.url)) {
    const who = host?.name || t`The repository's host`;
    return (
      <p className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{t`${who} can't reach this panel's address.`}</span>
        <Link to="/settings/general" className="app-panel-link">
          {t`Give the panel a domain`}
        </Link>
      </p>
    );
  }
  return (
    <p className="app-source-line">
      {last ? <LastPush delivery={last} /> : <span className="app-muted">{t`No push has come yet.`}</span>}
      <button type="button" className="app-panel-link" onClick={onOpen}>
        {last ? t`Hook settings` : t`Set up`}
      </button>
    </p>
  );
}

/**
 * Pushes through a GitHub App's one webhook, in one line: the last push, or that
 * pushes come through the App; where GitHub can't reach the panel, that Dockwell
 * checks for pushes itself, with the fix (ADR 0098).
 */
function AppHookLine({ hook }: { hook: GitHook }) {
  const { t } = useLingui();
  const words = useDeliveryWords();
  const app = useGitHubApps().data?.apps.find((a) => a.name === hook.githubApp);
  const delivery = words(app?.delivery);
  const last = hook.deliveries[0];
  if (delivery.refused) {
    return (
      <p className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{delivery.text}</span>
        <Link to="/settings/integrations" className="app-panel-link">
          {t`Open Integrations`}
        </Link>
      </p>
    );
  }
  if (delivery.unreached) {
    return (
      <p className="app-source-line is-warning">
        <Icon name="alert" size={14} />
        <span>{delivery.text}</span>
        <Link to="/settings/general" className="app-panel-link">
          {t`Give the panel a domain`}
        </Link>
      </p>
    );
  }
  return (
    <p className="app-source-line">
      {last ? <LastPush delivery={last} /> : <span className="app-muted">{delivery.text}</span>}
    </p>
  );
}

/** The newest push, and what came of it. */
function LastPush({ delivery }: { delivery: GitHookDelivery }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const { service } = useApp();
  const name = nameOf(service);
  const came = delivery.time ? timestampDate(delivery.time) : undefined;
  const now = useAgoNow(came);
  const ago = came ? time.ago(came, now) : "";
  const commit = shortCommit(delivery.commit);
  let what: string;
  switch (delivery.outcome) {
    case GitHookOutcome.DEPLOYING:
      what = t`Last push ${ago} started a deploy of ${commit}.`;
      break;
    case GitHookOutcome.NEEDS_PERSON:
      what = t`Last push ${ago} planned ${commit}, which waits for a person to apply it.`;
      break;
    case GitHookOutcome.QUEUED:
      what = t`Last push ${ago} waits for the deploy under way.`;
      break;
    case GitHookOutcome.NOTHING_NEW:
      what = t`Last push ${ago}: ${name} runs ${commit} already.`;
      break;
    case GitHookOutcome.SUPERSEDED:
      what = t`Last push ${ago} gave its place to a newer one.`;
      break;
    case GitHookOutcome.FAILED:
      what = t`Last push ${ago} deployed nothing.`;
      break;
    default:
      what = t`Last push ${ago} wasn't to the branch ${name} builds.`;
  }
  const words = (
    <span className={delivery.outcome === GitHookOutcome.FAILED ? "app-source-failed" : "app-muted"}>{what}</span>
  );
  return (
    <>
      {delivery.changeId ? (
        <Link to="/changes/$changeId" params={{ changeId: delivery.changeId }} className="app-source-change">
          {words}
        </Link>
      ) : (
        words
      )}
      {delivery.outcome === GitHookOutcome.FAILED && delivery.detail ? (
        <span className="app-source-why">{delivery.detail}</span>
      ) : null}
    </>
  );
}

/**
 * The hook's URL, and its secret just after it was made, with a link to where
 * the repository's host adds a webhook. Each field is named as that host's form
 * names it. GitHub, GitLab, Bitbucket, Gitea and Forgejo send a push on their
 * own, so there's nothing more to set. Where the environment is protected, the
 * hook's maker, an admin there, may let its pushes deploy without waiting for a
 * person, which takes their passkey (ADR 0071).
 */
function HookSheet({
  hook,
  open,
  host,
  guarded,
  onOpen,
  onChanged,
  onClose,
}: {
  hook: GitHook;
  open: Open;
  host: GitHost | undefined;
  guarded: boolean;
  onOpen: (open: Open) => void;
  onChanged: () => void;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const { service } = useApp();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const where = { environment: environmentOf(service) };
  const mayRotate = may(GitHookService.method.rotateGitHookSecret, where);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const name = nameOf(service);
  const hostName = host?.name ?? "";
  const urlLabel =
    host?.kind === "github" ? t`Payload URL` : host?.kind === "gitea" && !host.name ? t`Target URL` : t`URL`;
  const secretLabel = host?.kind === "gitlab" ? t`Secret token` : t`Secret`;

  const rotate = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.ROTATE_GIT_HOOK, service.id, (proof) =>
        createClient(GitHookService, transport).rotateGitHookSecret({ id: service.id, proof }),
      );
      onOpen({ secret: res.secret, rotated: true });
    } catch (err) {
      setProblem(explain(err) || sentence(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={open.rotated ? t`A new secret for ${name}'s hook` : t`Deploy ${name} on push`}
      subtitle={hostName ? t`A webhook on ${hostName}` : t`A webhook on the repository's host`}
      footer={
        <>
          <Button variant={host ? "secondary" : "primary"} onClick={onClose}>
            <Trans>Done</Trans>
          </Button>
          {host ? (
            <ButtonLink variant="primary" icon="external" href={host.hooksPage} target="_blank" rel="noreferrer">
              {hostName ? t`Open ${hostName}` : t`Open the repository's webhooks`}
            </ButtonLink>
          ) : null}
        </>
      }
    >
      <div className="app-hook-sheet">
        {open.secret ? (
          <Callout tone="healthy" title={t`Copy the secret now. It won't be shown again.`}>
            {open.rotated ? t`The old secret stopped working. Give the hook this one.` : null}
          </Callout>
        ) : null}
        <dl className="app-facts app-hook-fields">
          <dt>{urlLabel}</dt>
          <dd>
            <span className="copy-command">
              <code>{hook.url}</code>
              <CopyButton text={hook.url} label={t`Copy URL`} copiedLabel={t`URL copied`} iconOnly variant="ghost" />
            </span>
          </dd>
          <dt>{secretLabel}</dt>
          <dd>
            {open.secret ? (
              <span className="copy-command">
                <code>{open.secret}</code>
                <CopyButton
                  text={open.secret}
                  label={t`Copy secret`}
                  copiedLabel={t`Secret copied`}
                  iconOnly
                  variant="ghost"
                />
              </span>
            ) : (
              <span className="app-hook-secret">
                <span className="app-muted">{t`Shown once, when made`}</span>
                <Button
                  size="sm"
                  variant="ghost"
                  icon="key"
                  loading={busy}
                  onClick={() => void rotate()}
                  {...mayRotate.off}
                >
                  <Trans>New secret</Trans>
                </Button>
              </span>
            )}
          </dd>
        </dl>
        {problem ? (
          <Callout tone="critical" title={t`The secret didn't change`}>
            {problem}
          </Callout>
        ) : null}
        <WithoutWaiting hook={hook} guarded={guarded} onChanged={onChanged} />
      </div>
    </Sheet>
  );
}

/**
 * Deploy without waiting (ADR 0071): where the app's environment is protected, a
 * hook's push plans its deploy for a person to apply, unless the hook's maker, an
 * admin there, lets it deploy without waiting, which takes their passkey. It's
 * theirs alone to choose; anyone else sees nothing here.
 */
function WithoutWaiting({ hook, guarded, onChanged }: { hook: GitHook; guarded: boolean; onChanged: () => void }) {
  const { t } = useLingui();
  const { service } = useApp();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const where = { environment: environmentOf(service) };
  const me = useSignedInUser();
  const chooses =
    guarded && may(GitHookService.method.setGitHookRole, where).allowed && hook.createdBy?.id === me?.id && !!me;
  const place = usePlaces().label(where.environment) || t`this environment`;
  const [choosing, setChoosing] = useState(false);
  const [unchosen, setUnchosen] = useState<string>();

  const choose = async (withoutWaiting: boolean) => {
    setChoosing(true);
    setUnchosen(undefined);
    const hooks = createClient(GitHookService, transport);
    try {
      if (withoutWaiting) {
        // Your browser asks for your passkey, to confirm it's you, once nothing else stops it.
        await withStepUpIfAsked(transport, StepUpAction.WIDEN_GIT_HOOK, service.id, (proof) =>
          hooks.setGitHookRole({ id: service.id, role: Role.ADMIN, proof }),
        );
      } else {
        await hooks.setGitHookRole({ id: service.id, role: Role.DEVELOPER });
      }
    } catch (err) {
      setUnchosen(explain(err) || sentence(err));
    } finally {
      setChoosing(false);
      onChanged();
    }
  };
  if (!chooses) return null;
  return (
    <>
      <Switch
        checked={hook.role === Role.ADMIN}
        disabled={choosing}
        onChange={(next) => void choose(next)}
        label={t`Deploy without waiting`}
        description={t`Each push into ${place} applies with no person's approval.`}
      />
      {unchosen ? (
        <Callout tone="critical" title={t`Deploy without waiting didn't change`}>
          {unchosen}
        </Callout>
      ) : null}
    </>
  );
}
