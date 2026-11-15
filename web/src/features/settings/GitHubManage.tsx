// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type GitHubAppInfo,
  type GitHubInstallation,
  GitProviderService,
  type ReplaceGitHubPrivateKeyResponse,
  StepUpAction,
} from "@dockwell/api";
import { Button, ButtonLink, Callout, Sheet, Switch } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type DragEvent, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { StepUpDeclined, withStepUpIfAsked } from "../auth/stepUp";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAllResources } from "../projects/useProjects";
import { installUrl, useDeliveryWords } from "./github";

/** What removing the App left to do on GitHub, which keeps it until it's deleted there. */
export interface Removed {
  deleteUrl: string;
  uninstalled: number;
}

/**
 * Manage a GitHub App (ADR 0098): where it's installed, and which projects each
 * installation reaches, changed as a plan; whether GitHub reaches the panel; a new
 * webhook secret, and a new private key from the file GitHub's page downloads, each
 * with a passkey; and removing it.
 */
export function GitHubManageSheet({
  app,
  onClose,
  onChanged,
  onRemoved,
}: {
  app: GitHubAppInfo;
  onClose: () => void;
  /** Reads the App again, once something about it changed. */
  onChanged: () => void;
  /** Hears what removing it left to do on GitHub. */
  onRemoved: (removed: Removed) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const words = useDeliveryWords();
  const client = createClient(GitProviderService, transport);
  const mayRotate = may(GitProviderService.method.rotateGitHubWebhookSecret);
  const mayKey = may(GitProviderService.method.replaceGitHubPrivateKey);
  const mayRemove = may(GitProviderService.method.removeGitHubApp);
  const [busy, setBusy] = useState<"secret" | "key" | "remove">();
  const [problem, setProblem] = useState<{ title: string; text: string }>();
  const [rotated, setRotated] = useState(false);
  const [newKey, setNewKey] = useState<ReplaceGitHubPrivateKeyResponse>();
  const [keying, setKeying] = useState(false);
  const [removing, setRemoving] = useState(false);
  const file = useRef<HTMLInputElement>(null);
  const installed = app.installations.filter((i) => !i.removed);
  const delivery = words(app.delivery);

  const act = async <T,>(what: "secret" | "key" | "remove", title: string, call: () => Promise<T>) => {
    setBusy(what);
    setProblem(undefined);
    try {
      return await call();
    } catch (err) {
      if (!(err instanceof StepUpDeclined)) setProblem({ title, text: explain(err) || sentence(err) });
      return undefined;
    } finally {
      setBusy(undefined);
    }
  };

  const rotate = async () => {
    const done = await act("secret", t`The webhook secret didn't change`, () =>
      withStepUpIfAsked(transport, StepUpAction.ROTATE_GITHUB_APP, app.id, (proof) =>
        client.rotateGitHubWebhookSecret({ id: app.id, proof }),
      ),
    );
    if (done) {
      setRotated(true);
      onChanged();
    }
  };

  const replaceKey = async (picked: File | undefined) => {
    if (!picked) return;
    const privateKey = await picked.text();
    const done = await act("key", t`The private key didn't change`, () =>
      withStepUpIfAsked(transport, StepUpAction.ROTATE_GITHUB_APP, app.id, (proof) =>
        client.replaceGitHubPrivateKey({ id: app.id, privateKey, proof }),
      ),
    );
    if (done) {
      setNewKey(done);
      setKeying(false);
      onChanged();
    }
  };

  const remove = async () => {
    const done = await act("remove", t`The GitHub App wasn't removed`, () =>
      withStepUpIfAsked(transport, StepUpAction.REMOVE_GITHUB_APP, app.id, (proof) =>
        client.removeGitHubApp({ id: app.id, proof }),
      ),
    );
    if (done) {
      onRemoved({ deleteUrl: done.deleteUrl, uninstalled: done.uninstalled });
      onClose();
    }
  };

  const dropped = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    void replaceKey(event.dataTransfer.files[0]);
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`GitHub App ${app.name}`}
      subtitle={app.ownerIsOrganization ? t`Owned by the organization ${app.owner}` : t`Owned by ${app.owner}`}
      footer={
        removing ? (
          <>
            <Button variant="secondary" onClick={() => setRemoving(false)}>
              <Trans>Keep it</Trans>
            </Button>
            <Button variant="danger" loading={busy === "remove"} onClick={() => void remove()} {...mayRemove.off}>
              <Trans>Remove</Trans>
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" className="github-remove" onClick={() => setRemoving(true)} {...mayRemove.off}>
              <Trans>Remove</Trans>
            </Button>
            <Button variant="primary" onClick={onClose}>
              <Trans>Done</Trans>
            </Button>
          </>
        )
      }
    >
      <div className="github-manage">
        {removing ? (
          <Callout tone="warning" title={t`Remove the GitHub App from Dockwell?`}>
            {t`Apps that build through it can't fetch their private repository until it's connected again.`}
          </Callout>
        ) : null}

        <section className="github-manage-part" aria-labelledby="github-installed">
          <div className="github-manage-head">
            <h3 id="github-installed" className="dw-text-body-strong">
              <Trans>Installed on</Trans>
            </h3>
            <ButtonLink size="sm" variant="ghost" icon="plus" href={installUrl(app)}>
              {t`Install on another account`}
            </ButtonLink>
          </div>
          {installed.length === 0 ? (
            <p className="settings-meta">{t`No account yet.`}</p>
          ) : (
            <ul className="github-installations">
              {installed.map((i) => (
                <InstallationRow key={String(i.id)} app={app} installation={i} onChanged={onChanged} />
              ))}
            </ul>
          )}
        </section>

        <p className={delivery.unreached || delivery.refused ? "github-delivery is-unreached" : "github-delivery"}>
          <span>{delivery.text}</span>
          {delivery.unreached ? (
            <Link to="/settings/general" className="cp-link">
              {t`Give the panel a domain`}
            </Link>
          ) : null}
        </p>

        <section className="github-manage-part" aria-labelledby="github-keys">
          <h3 id="github-keys" className="dw-text-body-strong">
            <Trans>Secret and key</Trans>
          </h3>
          <dl className="app-facts github-facts">
            <dt>{t`Webhook secret`}</dt>
            <dd>
              <span className="settings-meta">{t`Only Dockwell and GitHub have it`}</span>
              <Button
                size="sm"
                variant="ghost"
                icon="key"
                loading={busy === "secret"}
                onClick={() => void rotate()}
                {...mayRotate.off}
              >
                <Trans>New webhook secret</Trans>
              </Button>
            </dd>
            <dt>{t`Private key`}</dt>
            <dd>
              <code className="github-fingerprint">{app.keyFingerprint}</code>
              <Button size="sm" variant="ghost" icon="key" onClick={() => setKeying((k) => !k)} {...mayKey.off}>
                <Trans>New private key</Trans>
              </Button>
            </dd>
          </dl>
          {rotated ? (
            <Callout tone="healthy" title={t`GitHub has the new webhook secret`}>
              {t`The old one works for 10 more minutes, for pushes already sent.`}
            </Callout>
          ) : null}
          {keying ? (
            // biome-ignore lint/a11y/noStaticElementInteractions: a drop target; the button inside picks a file from the keyboard
            <div className="github-key-drop" onDragOver={(event) => event.preventDefault()} onDrop={dropped}>
              <ButtonLink
                size="sm"
                variant="secondary"
                icon="external"
                href={app.settingsUrl}
                target="_blank"
                rel="noreferrer"
              >
                {t`Generate a key on GitHub`}
              </ButtonLink>
              <span className="settings-meta">{t`Then drop the .pem file here.`}</span>
              <input
                ref={file}
                type="file"
                className="dw-sr"
                tabIndex={-1}
                aria-hidden="true"
                accept=".pem,application/x-pem-file"
                onChange={(event) => {
                  void replaceKey(event.target.files?.[0]);
                  event.target.value = "";
                }}
              />
              <Button
                size="sm"
                variant="ghost"
                icon="upload"
                loading={busy === "key"}
                onClick={() => file.current?.click()}
              >
                <Trans>Choose a file</Trans>
              </Button>
            </div>
          ) : null}
          {newKey ? (
            <Callout tone="healthy" title={t`Dockwell signs with the new key`}>
              <Trans>
                Delete the old one on GitHub: <code>{newKey.oldFingerprint}</code>
              </Trans>{" "}
              <a href={newKey.keysUrl} target="_blank" rel="noreferrer" className="cp-link">
                {t`Open the App's keys`}
              </a>
            </Callout>
          ) : null}
        </section>

        {problem ? (
          <Callout tone="critical" title={problem.title}>
            {problem.text}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}

/**
 * One account the App is installed on: how many repositories it reaches, where to add
 * more on GitHub, and which projects may build through it, every one unless narrowed,
 * as a plan.
 */
function InstallationRow({
  app,
  installation: i,
  onChanged,
}: {
  app: GitHubAppInfo;
  installation: GitHubInstallation;
  onChanged: () => void;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const may = useMay();
  // Which projects an installation reaches is the App's spec, changed as a plan, an admin's (ADR 0098).
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const resources = useAllResources();
  const projects = (resources.data ?? [])
    .filter((r) => r.resource?.kind === "project")
    .map((r) => ({ id: r.id, label: r.displayName || r.resource?.name || r.id }));
  const [editing, setEditing] = useState(false);
  const [some, setSome] = useState(!i.everyProject);
  const [picked, setPicked] = useState<string[]>(i.projects);
  const who = i.everyProject
    ? t`every project`
    : i.projects.length === 1
      ? t`1 project`
      : t`${i.projects.length} projects`;
  const reach = i.allRepositories
    ? t`every repository`
    : i.repositoryCount === 1
      ? t`1 repository`
      : t`${i.repositoryCount} repositories`;

  const review = () => {
    // Every installation's reach goes in the plan, as the App's spec holds them all.
    const reaches = app.installations
      .filter((x) => !x.removed)
      .flatMap((x) => {
        const ps = x.id === i.id ? (some ? picked : undefined) : x.everyProject ? undefined : x.projects;
        return ps === undefined ? [] : [{ installationId: x.id, projects: ps }];
      });
    sheet.open({
      kind: "propose",
      summary: some
        ? t`Let only some projects build through ${i.account}`
        : t`Let every project build through ${i.account}`,
      edits: [
        {
          resource: { kind: "github.app", name: app.name },
          spec: { kind: { case: "githubApp", value: { reaches } } },
        },
      ],
      onClose: (change) => {
        if (change) {
          setEditing(false);
          onChanged();
        }
      },
    });
  };

  return (
    <li className="github-installation">
      <div className="github-installation-line">
        <code>{i.account}</code>
        <span className="settings-meta">{i.suspended ? t`Suspended on GitHub` : t`${reach} · for ${who}`}</span>
        <span className="github-spacer" />
        <a href={i.settingsUrl} target="_blank" rel="noreferrer" className="cp-link">
          {t`Repositories on GitHub`}
        </a>
        <Button size="sm" variant="ghost" onClick={() => setEditing((e) => !e)} {...mayPlan.off}>
          {editing ? <Trans>Cancel</Trans> : <Trans>Projects</Trans>}
        </Button>
      </div>
      {editing ? (
        <div className="github-projects">
          <Switch
            checked={some}
            onChange={setSome}
            label={t`Only some projects`}
            description={t`Their apps alone build through ${i.account}.`}
          />
          {some ? (
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
          <div>
            <Button size="sm" variant="primary" disabled={some && picked.length === 0} onClick={review}>
              <Trans>Review plan</Trans>
            </Button>
          </div>
        </div>
      ) : null}
    </li>
  );
}
