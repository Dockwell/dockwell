// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  type ComposeRefusal,
  ComposeService,
  type ComposeWorkspaceOmission,
  type ComposeWorkspaceRemoval,
  type GetComposeWorkspaceResponse,
} from "@dockwell/api";
import { Button, Callout, CodeField, Icon, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { mergeComposeDraft } from "./mergeComposeDraft";

export function ComposeResourceKind({ kind }: { kind: string }) {
  const { t } = useLingui();
  const label = kind === "service" ? t`Service` : kind === "network" ? t`Network` : t`Volume`;
  return <Tag words>{label}</Tag>;
}

export function KeptValues({ omissions }: { omissions: readonly ComposeWorkspaceOmission[] }) {
  return (
    <ul className="compose-keep-list">
      {omissions.map((item) => (
        <li key={`${item.kind}/${item.name}/${item.path}`}>
          <code>
            {item.name} · {item.path}
          </code>
          <Tag words tone={item.sealed ? "cobalt" : undefined}>
            {item.sealed ? <Trans>Sealed value</Trans> : <Trans>Kept in Dockwell</Trans>}
          </Tag>
        </li>
      ))}
    </ul>
  );
}

export function ComposeEditor({
  opened,
  reload,
  serverId,
}: {
  opened: GetComposeWorkspaceResponse;
  serverId: string;
  reload: () => Promise<GetComposeWorkspaceResponse | undefined>;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const [base, setBase] = useState(opened);
  const [draft, setDraft] = useState(opened.workspace?.yaml ?? "");
  const [previous, setPrevious] = useState<string>();
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState(false);
  const [stale, setStale] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [refusal, setRefusal] = useState<ComposeRefusal>();
  const [removals, setRemovals] = useState<ComposeWorkspaceRemoval[]>([]);
  const kept = base.omissions.filter((item) => item.sealed || item.path.startsWith("environment."));
  const environmentId = base.workspace?.environmentId ?? "";
  const mayEdit = useMay()(ComposeService.method.planComposeWorkspace, { environment: environmentId });
  const reloadBase = async (keepText: boolean) => {
    setBusy(true);
    setFailure(undefined);
    try {
      const fresh = await reload();
      if (!fresh) throw new Error(t`Couldn't load the latest file. Try again.`);
      const latest = fresh.workspace?.yaml ?? "";
      const merged = keepText ? mergeComposeDraft(base.workspace?.yaml ?? "", draft, latest) : latest;
      setPrevious(merged === undefined ? draft : undefined);
      setCopied(false);
      setBase(fresh);
      setDraft(merged ?? latest);
      setRefusal(undefined);
      setRemovals([]);
      setStale(false);
    } catch (error) {
      setFailure(sentence(error));
    } finally {
      setBusy(false);
    }
  };
  const review = async () => {
    setBusy(true);
    setFailure(undefined);
    setRefusal(undefined);
    try {
      const result = await createClient(ComposeService, transport).planComposeWorkspace({
        environmentId,
        snapshot: base.snapshot,
        yaml: draft,
        serverId,
        explicitRemovals: removals,
      });
      if (result.refusal) {
        setRefusal(result.refusal);
      } else if (result.removals.length) {
        setRemovals(result.removals);
      } else if (result.change) {
        sheet.open({
          kind: "plan",
          make: async () => result,
          onClose: (change) => {
            if (change?.state === ChangeState.APPLIED) void reloadBase(false);
          },
        });
      }
    } catch (error) {
      const reason = reasonOf(error);
      if (reason === "compose_workspace_stale" || reason === "plan_stale") setStale(true);
      else setFailure(sentence(error));
    } finally {
      setBusy(false);
    }
  };
  const line = refusal?.line ?? 0;
  const error = refusal ? (line ? t`Line ${line}: ${refusal.message}` : refusal.message) : undefined;
  return (
    <section className="compose-detail" aria-label={t`Compose file`}>
      {stale ? (
        <Callout
          tone="warning"
          title={t`This stack changed since you opened it`}
          actions={
            <Button size="sm" icon="refresh" disabled={busy} onClick={() => void reloadBase(true)}>
              <Trans>Reload and keep my edits</Trans>
            </Button>
          }
        >
          <Trans>Review your edits against the latest file.</Trans>
        </Callout>
      ) : null}
      {failure ? (
        <Callout tone="critical" title={t`Can't review this draft`}>
          {failure}
        </Callout>
      ) : null}
      {previous !== undefined ? (
        <Callout tone="warning" title={t`Some edits overlap the latest file`}>
          <Trans>Both versions are below.</Trans>
        </Callout>
      ) : null}
      <div className={previous === undefined ? undefined : "compose-merge"}>
        <CodeField
          aria-label={previous === undefined ? t`Compose file` : undefined}
          label={previous === undefined ? undefined : t`Latest file`}
          value={draft}
          readOnly={!mayEdit.allowed}
          lines={16}
          error={error}
          markedLine={refusal?.line}
          markedTo={refusal?.endLine}
          onChange={(event) => {
            setDraft(event.target.value);
            setRefusal(undefined);
            setRemovals([]);
            setFailure(undefined);
          }}
        />
        {previous !== undefined ? (
          <CodeField
            label={t`Previous draft`}
            value={previous}
            readOnly
            lines={16}
            actions={
              <Button
                size="sm"
                icon="copy"
                onClick={() =>
                  void navigator.clipboard.writeText(previous).then(
                    () => setCopied(true),
                    (error: unknown) => setFailure(sentence(error)),
                  )
                }
              >
                {copied ? <Trans>Copied</Trans> : <Trans>Copy</Trans>}
              </Button>
            }
          />
        ) : null}
      </div>
      {removals.length ? (
        <Callout tone="warning" title={t`Removing these resources needs review`}>
          <ul className="compose-keep-list">
            {removals.map((item) => (
              <li key={`${item.kind}/${item.name}`}>
                <code>{item.name}</code>
                <ComposeResourceKind kind={item.kind} />
              </li>
            ))}
          </ul>
        </Callout>
      ) : null}
      {mayEdit.allowed ? (
        <div className="compose-actions">
          <Button
            variant="primary"
            icon="commit"
            disabled={busy || stale || draft === base.workspace?.yaml}
            onClick={() => void review()}
          >
            {removals.length ? <Trans>Review plan with removals</Trans> : <Trans>Review plan</Trans>}
          </Button>
        </div>
      ) : null}
      {kept.length ? (
        <details className="compose-disclosure">
          <summary>
            <Icon name="chevron-right" size={14} className="compose-disclosure-caret" />
            <Icon name="lock" size={15} />
            <Trans>Kept in Dockwell</Trans>
          </summary>
          {kept.some((item) => item.path.startsWith("environment.")) ? (
            <p className="compose-muted">
              <Trans>Replace a placeholder to change its value, or delete the line to remove it.</Trans>
            </p>
          ) : null}
          <KeptValues omissions={kept} />
        </details>
      ) : null}
    </section>
  );
}
