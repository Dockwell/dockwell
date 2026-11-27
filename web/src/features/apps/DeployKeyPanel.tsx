// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type GitDeployKeyInfo,
  GitDeployKeyService,
  GitDeployKeyTestStatus,
  Role,
  StepUpAction,
  type TestGitDeployKeyResponse,
} from "@dockwell/api";
import { Button, ButtonLink, Callout, Sheet, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useCallback, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useAuthState } from "../auth/useAuthState";
import { CopyButton } from "../system/CopyCommand";
import { gitHost } from "./gitHost";
import { sameRepository } from "./RepositoryField";

import "./DeployKeyPanel.css";

/** Lists SSH deploy keys belonging to a project. */
export function useGitDeployKeys(project: string, enabled = true) {
  const mayList = useMay()(GitDeployKeyService.method.listGitDeployKeys, project ? { project } : undefined).allowed;
  return useQuery(
    GitDeployKeyService.method.listGitDeployKeys,
    { project },
    { enabled: enabled && project !== "" && mayList, retry: false, staleTime: 30_000 },
  );
}

/** Whether the signed-in user holds admin or owner on the install or on projectId. */
function useIsAdmin(projectId: string): boolean {
  const grants = useAuthState().data?.grants ?? [];
  return grants.some(
    (g) => (g.projectId === "" || g.projectId === projectId) && (g.role === Role.ADMIN || g.role === Role.OWNER),
  );
}

/** Label for the external deploy keys button on a recognized host. */
function useHostButtonLabel(hostName: string): string {
  const { t } = useLingui();
  return hostName ? t`Open ${hostName} deploy keys` : t`Open deploy keys`;
}

/**
 * In-place deploy key creation, host-key verification and test inside
 * New app -> From Git (ADR 0113).
 */
export function DeployKeySetup({
  project,
  repository,
  activeKey,
  authRequired,
  sshAddress,
  onCreated,
  onVerified,
}: {
  project: string;
  repository: string;
  activeKey: GitDeployKeyInfo | undefined;
  /** True when an HTTPS lookup answered 401/403/404. */
  authRequired: boolean;
  /** True when the typed repository uses ssh:// or user@host:path. */
  sshAddress: boolean;
  /** Called right after creating a key so the form pins gitLogin and SSH URL. */
  onCreated: (key: GitDeployKeyInfo) => void;
  /** Called when Test succeeds so the branch selector and detection refresh immediately. */
  onVerified: (key: GitDeployKeyInfo, branches: readonly string[], defaultBranch: string) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const isAdmin = useIsAdmin(project);
  const mayCreate = may(GitDeployKeyService.method.createGitDeployKey, project ? { project } : undefined);
  const client = useMemo(() => createClient(GitDeployKeyService, transport), [transport]);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [createdKey, setCreatedKey] = useState<GitDeployKeyInfo>();
  const [lastTest, setLastTest] = useState<TestGitDeployKeyResponse>();

  // Prefer freshly created/updated key matching this repository, otherwise the project's matching key.
  const key =
    createdKey && sameRepository(createdKey.repository, repository)
      ? createdKey
      : activeKey && sameRepository(activeKey.repository, repository)
        ? activeKey
        : undefined;

  const host = gitHost(key?.repository || repository);
  const deployKeysUrl = key?.deployKeysUrl || host?.deployKeysPage || "";
  const hostName = key?.hostName || host?.name || "";
  const hostButtonLabel = useHostButtonLabel(hostName);

  const createKey = useCallback(async () => {
    if (!project) return;
    setBusy(true);
    setError(undefined);
    setLastTest(undefined);
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.CREATE_GIT_DEPLOY_KEY, project, (proof) =>
        client.createGitDeployKey({ project, repository: repository.trim(), proof }),
      );
      if (res.key) {
        setCreatedKey(res.key);
        onCreated(res.key);
      }
    } catch (err) {
      setError(explain(err) || sentence(err));
    } finally {
      setBusy(false);
    }
  }, [client, explain, onCreated, project, repository, transport]);

  const runTest = useCallback(
    async (accept?: { algorithm: string; fingerprint: string; changed: boolean }) => {
      if (!key) return;
      setBusy(true);
      setError(undefined);
      try {
        const res = accept?.changed
          ? await withStepUpIfAsked(transport, StepUpAction.ACCEPT_GIT_HOST_KEY, key.id, (proof) =>
              client.testGitDeployKey({
                id: key.id,
                acceptHostKey: true,
                hostKeyAlgorithm: accept.algorithm,
                hostKeyFingerprint: accept.fingerprint,
                proof,
              }),
            )
          : await client.testGitDeployKey({
              id: key.id,
              ...(accept
                ? {
                    acceptHostKey: true,
                    hostKeyAlgorithm: accept.algorithm,
                    hostKeyFingerprint: accept.fingerprint,
                  }
                : {}),
            });
        setLastTest(res);
        if (res.key) setCreatedKey(res.key);
        if (res.status === GitDeployKeyTestStatus.OK && res.key) {
          onVerified(res.key, res.branches, res.defaultBranch);
        }
      } catch (err) {
        setError(explain(err) || sentence(err));
      } finally {
        setBusy(false);
      }
    },
    [client, explain, key, onVerified, transport],
  );

  // Nothing to show unless SSH address, HTTPS auth_required, or an active key for this repo.
  if (!sshAddress && !authRequired && !key) return null;

  // No key exists yet for this repository: show one short in-place prompt with "Use a deploy key".
  if (!key) {
    const noProjectReason = !project ? t`Choose a project first.` : "";
    const disabled = !project || Boolean(mayCreate.off.disabled);
    return (
      <div className="deploy-key-card" data-testid="deploy-key-offer">
        <div className="deploy-key-head">
          <p className="deploy-key-title">
            {sshAddress ? <Trans>Connect over SSH</Trans> : <Trans>Private repository</Trans>}
          </p>
          <Button
            size="sm"
            variant="secondary"
            loading={busy}
            disabled={disabled}
            off={noProjectReason || mayCreate.off.off}
            onClick={() => void createKey()}
          >
            <Trans>Use a deploy key</Trans>
          </Button>
        </div>
        <p className="deploy-key-copy">
          {noProjectReason ||
            mayCreate.reason ||
            (sshAddress ? (
              <Trans>Create a read-only deploy key for this project.</Trans>
            ) : (
              <Trans>Create a read-only deploy key to fetch this repository.</Trans>
            ))}
        </p>
        {error ? (
          <Callout tone="critical" title={t`Couldn't create the deploy key`}>
            {error}
          </Callout>
        ) : null}
      </div>
    );
  }

  // Key exists and is verified: only show Ready after a passing connection test
  // and only while no host-key change is pending admin review (L1, N11).
  const verified =
    !key.hostKeyChanged &&
    (lastTest?.status === GitDeployKeyTestStatus.OK || (key.verified && key.hostKeyPinned && lastTest === undefined));

  if (verified) {
    return (
      <div className="deploy-key-card" data-testid="deploy-key-ready">
        <div className="deploy-key-head">
          <p className="deploy-key-title">
            <Trans>
              Deploy key <code>{key.name}</code>
            </Trans>
          </p>
          <StatusBadge status="healthy" label={t`Ready`} />
        </div>
        <p className="deploy-key-copy">
          <Trans>
            Host key pinned (<code className="deploy-key-fingerprint">{key.hostKeyFingerprint}</code>).
          </Trans>
        </p>
      </div>
    );
  }

  const unverified = lastTest?.status === GitDeployKeyTestStatus.HOST_KEY_UNVERIFIED;
  const changed = lastTest?.status === GitDeployKeyTestStatus.HOST_KEY_CHANGED || key.hostKeyChanged;
  const obsAlgo = lastTest?.observedHostKeyAlgorithm || key.observedHostKeyAlgorithm;
  const obsFP = lastTest?.observedHostKeyFingerprint || key.observedHostKeyFingerprint;

  return (
    <div className="deploy-key-card" data-testid="deploy-key-setup">
      <div className="deploy-key-head">
        <p className="deploy-key-title">
          <Trans>
            Deploy key <code>{key.name}</code>
          </Trans>
        </p>
        {key.hostKeyPinned ? (
          <StatusBadge status={changed ? "down" : "healthy"} label={changed ? t`Host key changed` : t`Host pinned`} />
        ) : (
          <StatusBadge status="unknown" label={t`Not tested yet`} />
        )}
      </div>

      <div className="deploy-key-block">
        <p className="deploy-key-copy">
          {hostName ? (
            <Trans>Add this public key to {hostName} (read-only), then test.</Trans>
          ) : (
            <Trans>Add this public key to your Git host (read-only), then test.</Trans>
          )}
        </p>
        <code className="deploy-key-pubkey" data-testid="deploy-key-pubkey">
          {key.publicKey}
        </code>
        <div className="deploy-key-actions">
          <CopyButton text={key.publicKey} label={t`Copy key`} copiedLabel={t`Key copied`} variant="secondary" />
          {deployKeysUrl ? (
            <ButtonLink href={deployKeysUrl} target="_blank" rel="noreferrer" variant="ghost" size="sm">
              {hostButtonLabel}
            </ButtonLink>
          ) : null}
          <Button size="sm" variant="primary" loading={busy} onClick={() => void runTest()}>
            <Trans>Test</Trans>
          </Button>
        </div>
      </div>

      {unverified && obsAlgo && obsFP ? (
        <Callout
          tone="info"
          stacked
          title={t`Verify ${key.host}'s SSH host key`}
          actions={
            <Button
              size="sm"
              variant="primary"
              loading={busy}
              onClick={() => void runTest({ algorithm: obsAlgo, fingerprint: obsFP, changed: false })}
            >
              <Trans>Accept host key and test</Trans>
            </Button>
          }
        >
          <Trans>
            Compare <code className="deploy-key-fingerprint">{obsAlgo}</code>{" "}
            <code className="deploy-key-fingerprint">{obsFP}</code> with {key.host}'s published key.
          </Trans>
        </Callout>
      ) : null}

      {changed && obsAlgo && obsFP ? (
        <Callout
          tone="warning"
          stacked
          title={t`The SSH host key for ${key.host} changed`}
          actions={
            isAdmin ? (
              <Button
                size="sm"
                variant="secondary"
                loading={busy}
                onClick={() => void runTest({ algorithm: obsAlgo, fingerprint: obsFP, changed: true })}
              >
                <Trans>Accept new host key</Trans>
              </Button>
            ) : undefined
          }
        >
          <Trans>
            Fetches are stopped until an admin verifies <code className="deploy-key-fingerprint">{obsAlgo}</code>{" "}
            <code className="deploy-key-fingerprint">{obsFP}</code> (pinned:{" "}
            <code className="deploy-key-fingerprint">{key.hostKeyFingerprint}</code>).
          </Trans>
        </Callout>
      ) : null}

      {lastTest &&
      lastTest.status !== GitDeployKeyTestStatus.OK &&
      lastTest.status !== GitDeployKeyTestStatus.HOST_KEY_UNVERIFIED &&
      lastTest.status !== GitDeployKeyTestStatus.HOST_KEY_CHANGED ? (
        <Callout tone="warning" title={t`Connection test failed`}>
          {lastTest.message}
        </Callout>
      ) : null}

      {error ? (
        <Callout tone="critical" title={t`Deploy key error`}>
          {error}
        </Callout>
      ) : null}
    </div>
  );
}

/**
 * Sheet for inspecting, testing, rotating, accepting a changed host key, or
 * deleting a project SSH deploy key from an app's Source panel (ADR 0113).
 */
export function DeployKeySheet({
  deployKey,
  onChanged,
  onClose,
}: {
  deployKey: GitDeployKeyInfo;
  onChanged: () => void;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const [current, setCurrent] = useState<GitDeployKeyInfo>(deployKey);
  const isAdmin = useIsAdmin(current.projectId);
  const client = useMemo(() => createClient(GitDeployKeyService, transport), [transport]);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [lastTest, setLastTest] = useState<TestGitDeployKeyResponse>();
  const [rotatedOldFP, setRotatedOldFP] = useState<string>();
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  const where = { project: current.projectId };
  const mayTest = may(GitDeployKeyService.method.testGitDeployKey, where);
  const mayRotate = may(GitDeployKeyService.method.beginRotateGitDeployKey, where);
  const mayCancelRotate = may(GitDeployKeyService.method.cancelRotateGitDeployKey, where);
  const mayDelete = may(GitDeployKeyService.method.deleteGitDeployKey, where);
  const host = gitHost(current.repository);
  const deployKeysUrl = current.deployKeysUrl || host?.deployKeysPage || "";
  const hostName = current.hostName || host?.name || "";
  const hostButtonLabel = useHostButtonLabel(hostName);

  const runTest = async (opts?: {
    usePending?: boolean;
    accept?: { algorithm: string; fingerprint: string; changed: boolean };
  }) => {
    setBusy(true);
    setError(undefined);
    try {
      const res = opts?.accept?.changed
        ? await withStepUpIfAsked(transport, StepUpAction.ACCEPT_GIT_HOST_KEY, current.id, (proof) =>
            client.testGitDeployKey({
              id: current.id,
              usePending: opts.usePending ?? false,
              acceptHostKey: true,
              hostKeyAlgorithm: opts.accept?.algorithm ?? "",
              hostKeyFingerprint: opts.accept?.fingerprint ?? "",
              proof,
            }),
          )
        : await client.testGitDeployKey({
            id: current.id,
            usePending: opts?.usePending ?? false,
            ...(opts?.accept
              ? {
                  acceptHostKey: true,
                  hostKeyAlgorithm: opts.accept.algorithm,
                  hostKeyFingerprint: opts.accept.fingerprint,
                }
              : {}),
          });
      setLastTest(res);
      if (res.key) setCurrent(res.key);
      if (res.promoted && res.oldKeyFingerprint) {
        setRotatedOldFP(res.oldKeyFingerprint);
      }
      onChanged();
    } catch (err) {
      setError(explain(err) || sentence(err));
    } finally {
      setBusy(false);
    }
  };

  const beginRotate = async () => {
    setBusy(true);
    setError(undefined);
    setRotatedOldFP(undefined);
    setLastTest(undefined);
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.ROTATE_GIT_DEPLOY_KEY, current.id, (proof) =>
        client.beginRotateGitDeployKey({ id: current.id, proof }),
      );
      if (res.key) setCurrent(res.key);
      onChanged();
    } catch (err) {
      setError(explain(err) || sentence(err));
    } finally {
      setBusy(false);
    }
  };

  const cancelRotate = async () => {
    setBusy(true);
    setError(undefined);
    setLastTest(undefined);
    try {
      const res = await client.cancelRotateGitDeployKey({ id: current.id });
      if (res.key) setCurrent(res.key);
      onChanged();
    } catch (err) {
      setError(explain(err) || sentence(err));
    } finally {
      setBusy(false);
    }
  };

  const deleteKey = async () => {
    setBusy(true);
    setError(undefined);
    try {
      await withStepUpIfAsked(transport, StepUpAction.DELETE_GIT_DEPLOY_KEY, current.id, (proof) =>
        client.deleteGitDeployKey({ id: current.id, proof }),
      );
      onChanged();
      onClose();
    } catch (err) {
      setError(explain(err) || sentence(err));
    } finally {
      setBusy(false);
    }
  };

  const createdAt = current.createTime ? formatDateTime(timestampDate(current.createTime), i18n.locale) : "";
  const rotatedAt = current.rotateTime ? formatDateTime(timestampDate(current.rotateTime), i18n.locale) : "";
  const pending = current.pendingPublicKey !== "";
  const unverified = lastTest?.status === GitDeployKeyTestStatus.HOST_KEY_UNVERIFIED;
  const changed = lastTest?.status === GitDeployKeyTestStatus.HOST_KEY_CHANGED || current.hostKeyChanged;
  const obsAlgo = lastTest?.observedHostKeyAlgorithm || current.observedHostKeyAlgorithm;
  const obsFP = lastTest?.observedHostKeyFingerprint || current.observedHostKeyFingerprint;

  return (
    <Sheet open onClose={onClose} title={t`Deploy key ${current.name}`}>
      <div className="deploy-key-sheet">
        <dl className="deploy-key-facts">
          <dt>{t`Repository`}</dt>
          <dd>
            <code>{current.repository}</code>
          </dd>
          <dt>{t`Key type`}</dt>
          <dd>
            <code>{current.keyType}</code>
          </dd>
          <dt>{t`Key fingerprint`}</dt>
          <dd>
            <code className="deploy-key-fingerprint">{current.keyFingerprint}</code>
          </dd>
          <dt>{t`SSH host key`}</dt>
          <dd>
            {current.hostKeyPinned ? (
              <code className="deploy-key-fingerprint">
                {current.hostKeyAlgorithm} {current.hostKeyFingerprint}
              </code>
            ) : (
              <span className="app-muted">{t`Not pinned yet`}</span>
            )}
          </dd>
          {createdAt ? (
            <>
              <dt>{t`Created`}</dt>
              <dd>{createdAt}</dd>
            </>
          ) : null}
          {rotatedAt ? (
            <>
              <dt>{t`Last rotated`}</dt>
              <dd>{rotatedAt}</dd>
            </>
          ) : null}
          {current.usedBy.length > 0 ? (
            <>
              <dt>{t`Used by`}</dt>
              <dd>{current.usedBy.join(", ")}</dd>
            </>
          ) : null}
        </dl>

        {changed && obsAlgo && obsFP ? (
          <Callout
            tone="warning"
            stacked
            title={t`The SSH host key for ${current.host} changed`}
            actions={
              isAdmin ? (
                <Button
                  size="sm"
                  variant="secondary"
                  loading={busy}
                  onClick={() => void runTest({ accept: { algorithm: obsAlgo, fingerprint: obsFP, changed: true } })}
                >
                  <Trans>Accept new host key</Trans>
                </Button>
              ) : undefined
            }
          >
            <Trans>
              Fetches are stopped until an admin verifies <code className="deploy-key-fingerprint">{obsAlgo}</code>{" "}
              <code className="deploy-key-fingerprint">{obsFP}</code>.
            </Trans>
          </Callout>
        ) : null}

        {rotatedOldFP ? (
          <Callout
            tone="healthy"
            stacked
            title={t`Deploy key rotated`}
            actions={
              deployKeysUrl ? (
                <ButtonLink href={deployKeysUrl} target="_blank" rel="noreferrer" variant="secondary" size="sm">
                  {hostButtonLabel}
                </ButtonLink>
              ) : undefined
            }
          >
            <Trans>
              Dockwell now uses the new key. Delete the old key (
              <code className="deploy-key-fingerprint">{rotatedOldFP}</code>) from your Git host.
            </Trans>
          </Callout>
        ) : null}

        <div className="deploy-key-section">
          <h3 className="deploy-key-section-title">
            <Trans>Active public key</Trans>
          </h3>
          <code className="deploy-key-pubkey">{current.publicKey}</code>
          <div className="deploy-key-actions">
            <CopyButton text={current.publicKey} label={t`Copy key`} copiedLabel={t`Key copied`} variant="secondary" />
            {deployKeysUrl ? (
              <ButtonLink href={deployKeysUrl} target="_blank" rel="noreferrer" variant="ghost" size="sm">
                {hostButtonLabel}
              </ButtonLink>
            ) : null}
            <Button size="sm" variant="secondary" loading={busy} onClick={() => void runTest()} {...mayTest.off}>
              <Trans>Test connection</Trans>
            </Button>
          </div>
        </div>

        {unverified && obsAlgo && obsFP ? (
          <Callout
            tone="info"
            stacked
            title={t`Verify ${current.host}'s SSH host key`}
            actions={
              <Button
                size="sm"
                variant="primary"
                loading={busy}
                onClick={() => void runTest({ accept: { algorithm: obsAlgo, fingerprint: obsFP, changed: false } })}
              >
                <Trans>Accept host key and test</Trans>
              </Button>
            }
          >
            <Trans>
              Compare <code className="deploy-key-fingerprint">{obsAlgo}</code>{" "}
              <code className="deploy-key-fingerprint">{obsFP}</code> with {current.host}'s published key.
            </Trans>
          </Callout>
        ) : null}

        {lastTest?.status === GitDeployKeyTestStatus.OK && !lastTest.promoted ? (
          <Callout tone="healthy" title={t`Connection works`}>
            <Trans>
              Read {lastTest.branches.length} branches from {current.host}.
            </Trans>
          </Callout>
        ) : null}

        {lastTest &&
        lastTest.status !== GitDeployKeyTestStatus.OK &&
        lastTest.status !== GitDeployKeyTestStatus.HOST_KEY_UNVERIFIED &&
        lastTest.status !== GitDeployKeyTestStatus.HOST_KEY_CHANGED ? (
          <Callout tone="warning" title={t`Connection test failed`}>
            {lastTest.message}
          </Callout>
        ) : null}

        <div className="deploy-key-section">
          <h3 className="deploy-key-section-title">
            <Trans>Rotate key</Trans>
          </h3>
          {pending ? (
            <div className="deploy-key-card" data-testid="deploy-key-rotate-pending">
              <p className="deploy-key-copy">
                <Trans>Add this key alongside the current one (read-only), then test.</Trans>
              </p>
              <code className="deploy-key-pubkey" data-testid="deploy-key-pending-pubkey">
                {current.pendingPublicKey}
              </code>
              <div className="deploy-key-actions">
                <CopyButton
                  text={current.pendingPublicKey}
                  label={t`Copy new key`}
                  copiedLabel={t`New key copied`}
                  variant="secondary"
                />
                {deployKeysUrl ? (
                  <ButtonLink href={deployKeysUrl} target="_blank" rel="noreferrer" variant="ghost" size="sm">
                    {hostButtonLabel}
                  </ButtonLink>
                ) : null}
                <Button
                  size="sm"
                  variant="primary"
                  loading={busy}
                  onClick={() => void runTest({ usePending: true })}
                  {...mayTest.off}
                >
                  <Trans>Test new key</Trans>
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy}
                  onClick={() => void cancelRotate()}
                  {...mayCancelRotate.off}
                >
                  <Trans>Cancel rotation</Trans>
                </Button>
              </div>
            </div>
          ) : (
            <>
              <div className="deploy-key-actions">
                <Button
                  size="sm"
                  variant="secondary"
                  loading={busy}
                  onClick={() => void beginRotate()}
                  {...mayRotate.off}
                >
                  <Trans>Rotate key</Trans>
                </Button>
                {current.usedBy.length === 0 && !confirmingDelete ? (
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={busy}
                    onClick={() => setConfirmingDelete(true)}
                    {...mayDelete.off}
                  >
                    <Trans>Delete key</Trans>
                  </Button>
                ) : null}
              </div>
              {current.usedBy.length === 0 && confirmingDelete ? (
                <div className="deploy-key-card" data-testid="deploy-key-delete-confirm">
                  <p className="deploy-key-copy">
                    <Trans>Deleting this key can't be undone. Remove it on {hostName || current.host} too.</Trans>
                  </p>
                  <div className="deploy-key-actions">
                    <Button
                      size="sm"
                      variant="danger"
                      loading={busy}
                      onClick={() => void deleteKey()}
                      {...mayDelete.off}
                    >
                      <Trans>Delete key</Trans>
                    </Button>
                    {deployKeysUrl ? (
                      <ButtonLink href={deployKeysUrl} target="_blank" rel="noreferrer" variant="ghost" size="sm">
                        {hostButtonLabel}
                      </ButtonLink>
                    ) : null}
                    <Button size="sm" variant="ghost" disabled={busy} onClick={() => setConfirmingDelete(false)}>
                      <Trans>Cancel</Trans>
                    </Button>
                  </div>
                </div>
              ) : null}
            </>
          )}
        </div>

        {error ? (
          <Callout tone="critical" title={t`Deploy key error`}>
            {error}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
