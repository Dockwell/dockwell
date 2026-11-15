// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type GitHookDelivery, GitHookOutcome, GitHookService, type GitSource, StepUpAction } from "@dockwell/api";
import { Button, Callout, Panel, Sheet, Switch, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";
import { shownRef } from "../../lib/built";
import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useAgoNow } from "../servers/useFleet";
import { CopyButton } from "../system/CopyCommand";
import { useApp, useRevisions } from "./AppContext";
import { nameOf } from "./app";
import { shortCommit, shownRepository } from "./Build";

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
  return (
    <Panel title={t`Source`}>
      <div className="app-source">
        <dl className="app-facts">
          <dt>{t`Repository`}</dt>
          <dd className="app-source-repo">
            <code>{shownRepository(source.url)}</code>
            <Tag icon="branch">{shownRef(source.ref) || t`default branch`}</Tag>
          </dd>
          <LastCommit serviceId={service.id} source={source} />
        </dl>
        <DeployOnPush serviceId={service.id} name={nameOf(service)} branch={shownRef(source.ref)} />
      </div>
    </Panel>
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

/** A hook just made, or given a new secret, with that secret, which is shown only now. */
interface Made {
  url: string;
  secret: string;
  rotated: boolean;
}

/**
 * The Deploy on push switch: a hook GitHub, Gitea or Forgejo sends each push to,
 * whose secret only you and the provider have (ADR 0046). Turning it on asks for
 * your passkey, then shows its URL and secret, once; turning it off needs none.
 */
function DeployOnPush({ serviceId, name, branch }: { serviceId: string; name: string; branch: string }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const query = useQuery(GitHookService.method.getGitHook, { id: serviceId });
  const hook = query.data?.hook;
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const [made, setMade] = useState<Made>();
  const [turnedOff, setTurnedOff] = useState(false);
  const shownBranch = branch || t`the default branch`;

  const turn = async (on: boolean) => {
    setBusy(true);
    setProblem(undefined);
    setTurnedOff(false);
    const hooks = createClient(GitHookService, transport);
    try {
      if (on) {
        // Your browser asks for your passkey, to confirm it's you, once nothing else stops it.
        const res = await withStepUpIfAsked(transport, StepUpAction.CREATE_GIT_HOOK, serviceId, (proof) =>
          hooks.createGitHook({ id: serviceId, proof }),
        );
        setMade({ url: res.hook?.url ?? "", secret: res.secret, rotated: false });
      } else {
        await hooks.deleteGitHook({ id: serviceId });
        setTurnedOff(true);
      }
    } catch (err) {
      setProblem(explain(err) || sentence(err));
    } finally {
      setBusy(false);
      void query.refetch();
    }
  };

  const rotate = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.ROTATE_GIT_HOOK, serviceId, (proof) =>
        createClient(GitHookService, transport).rotateGitHookSecret({ id: serviceId, proof }),
      );
      setMade({ url: res.hook?.url ?? "", secret: res.secret, rotated: true });
    } catch (err) {
      setProblem(explain(err) || sentence(err));
    } finally {
      setBusy(false);
      void query.refetch();
    }
  };

  const on = hook !== undefined;
  const last = hook?.deliveries[0];
  return (
    <div className="app-source-push">
      <Switch
        checked={on}
        disabled={busy || query.isPending}
        onChange={(next) => void turn(next)}
        label={t`Deploy on push`}
        description={
          on
            ? t`Each push to ${shownBranch} builds ${name} and deploys it. A plan that needs a person waits for one.`
            : t`Pushes to ${shownBranch} don't deploy ${name}. Turn this on to deploy each one.`
        }
      />
      {turnedOff ? (
        <p className="app-muted app-source-note">
          <Trans>
            Deploy on push is off. Its hook's URL now refuses pushes, so remove the hook from the repository too.
          </Trans>
        </p>
      ) : null}
      {hook ? (
        <>
          <div className="app-source-hook">
            <span className="copy-command">
              <code>{hook.url}</code>
              <CopyButton text={hook.url} label={t`Copy URL`} copiedLabel={t`URL copied`} iconOnly />
            </span>
            <Button size="sm" variant="ghost" icon="key" onClick={() => void rotate()} disabled={busy}>
              <Trans>New secret</Trans>
            </Button>
          </div>
          {unreachable(hook.url) ? (
            <Callout tone="warning" title={t`GitHub can't reach this address`}>
              <Trans>
                The panel answers only on this machine, so no provider can send it a push. Give the panel a domain in
                Settings, and the hook's URL moves to it.
              </Trans>
            </Callout>
          ) : null}
          {last ? (
            <LastPush delivery={last} name={name} />
          ) : (
            <p className="app-muted app-source-note">{t`No push has come yet.`}</p>
          )}
        </>
      ) : null}
      {problem ? (
        <Callout tone="critical" title={on ? t`Deploy on push didn't change` : t`Deploy on push isn't on`}>
          {problem}
        </Callout>
      ) : null}
      {made ? <HookSheet made={made} name={name} branch={shownBranch} onClose={() => setMade(undefined)} /> : null}
    </div>
  );
}

/** The newest push, and what came of it. */
function LastPush({ delivery, name }: { delivery: GitHookDelivery; name: string }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const came = delivery.time ? timestampDate(delivery.time) : undefined;
  const now = useAgoNow(came);
  const ago = came ? time.ago(came, now) : "";
  const commit = shortCommit(delivery.commit);
  const ref = shownRef(delivery.ref);
  let what: string;
  switch (delivery.outcome) {
    case GitHookOutcome.DEPLOYING:
      what = t`started a deploy of ${commit}`;
      break;
    case GitHookOutcome.NEEDS_PERSON:
      what = t`planned ${commit}, which waits for a person to apply it`;
      break;
    case GitHookOutcome.QUEUED:
      what = t`waits for the deploy under way`;
      break;
    case GitHookOutcome.NOTHING_NEW:
      what = t`${name} runs ${commit} already`;
      break;
    case GitHookOutcome.SUPERSEDED:
      what = t`a newer push took its place`;
      break;
    case GitHookOutcome.FAILED:
      what = t`it deployed nothing`;
      break;
    default:
      what = ref ? t`a push to ${ref}, which ${name} doesn't build` : t`not a push, so nothing to deploy`;
  }
  return (
    <p className="app-source-note">
      <span className="app-muted">{t`Last push, ${ago}:`}</span> {what}
      {delivery.outcome === GitHookOutcome.FAILED && delivery.detail ? (
        <span className="app-source-why">{`${delivery.detail}.`}</span>
      ) : null}
    </p>
  );
}

/**
 * The hook's URL and its secret, once, with where each goes on GitHub, Gitea
 * and Forgejo.
 */
function HookSheet({ made, name, branch, onClose }: { made: Made; name: string; branch: string; onClose: () => void }) {
  const { t } = useLingui();
  return (
    <Sheet
      open
      onClose={onClose}
      title={made.rotated ? t`A new secret for ${name}'s hook` : t`Deploy ${name} on push`}
      subtitle={t`Add this hook to the repository`}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>You see the secret once, right now.</Trans>
          </span>
          <Button variant="primary" onClick={onClose}>
            <Trans>Done</Trans>
          </Button>
        </>
      }
    >
      <div className="app-hook-sheet">
        <Callout tone="healthy" title={t`Copy the secret now. It won't be shown again.`}>
          {made.rotated ? (
            <Trans>The old secret stopped working. Put this one in the repository's hook in its place.</Trans>
          ) : (
            <Trans>
              Each push to {branch} the repository sends here builds {name} and deploys it.
            </Trans>
          )}
        </Callout>
        <dl className="app-facts app-hook-fields">
          <dt>{t`Payload URL`}</dt>
          <dd>
            <span className="copy-command">
              <code>{made.url}</code>
              <CopyButton text={made.url} label={t`Copy URL`} copiedLabel={t`URL copied`} iconOnly />
            </span>
          </dd>
          <dt>{t`Secret`}</dt>
          <dd>
            <span className="copy-command">
              <code>{made.secret}</code>
              <CopyButton text={made.secret} label={t`Copy secret`} copiedLabel={t`Secret copied`} iconOnly />
            </span>
          </dd>
          <dt>{t`Content type`}</dt>
          <dd>
            <code>application/json</code>
          </dd>
          <dt>{t`Events`}</dt>
          <dd>{t`Just the push event`}</dd>
        </dl>
        <div className="app-hook-where">
          <h3>{t`Where it goes`}</h3>
          <ul>
            <li>
              <Trans>
                <b>GitHub:</b> the repository's Settings, then Webhooks, then Add webhook.
              </Trans>
            </li>
            <li>
              <Trans>
                <b>Gitea and Forgejo:</b> the repository's Settings, then Webhooks, then Add webhook, then Gitea or
                Forgejo. Choose push events.
              </Trans>
            </li>
          </ul>
        </div>
      </div>
    </Sheet>
  );
}

/** A hook's URL no provider can reach: the panel on this machine alone. */
function unreachable(url: string): boolean {
  try {
    const host = new URL(url).hostname;
    return host === "localhost" || host === "[::1]" || host.startsWith("127.");
  } catch {
    return false;
  }
}
