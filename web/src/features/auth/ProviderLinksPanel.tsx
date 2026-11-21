// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, type ProviderLink, ProviderLinkState, type SignInProvider, StepUpAction } from "@dockwell/api";
import { Button, Icon, Loading, Panel, SkeletonRows, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { formatDay } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow } from "../servers/useFleet";
import { useAuthError } from "./messages";
import { withStepUpIfAsked } from "./stepUp";

/**
 * The accounts at sign-in providers you sign in to Dockwell with, on the panel's own
 * domain, and a button to link one at each provider that signs people in here
 * (ADR 0095). Linking asks for your passkey each time. Nothing shows where no
 * provider signs in and nothing is linked.
 */
export function ProviderLinksPanel({ providers }: { providers: readonly SignInProvider[] }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const links = useQuery(AuthService.method.listProviderLinks, {});
  const now = useControlPlaneNow();
  const [busy, setBusy] = useState<string>();
  const [problem, setProblem] = useState<string>();
  const list = links.data?.links ?? [];
  if (providers.length === 0 && list.length === 0) return null;
  const unlinked = providers.filter((p) => !list.some((l) => l.providerId === p.id));

  const link = async (provider: SignInProvider) => {
    setBusy(provider.id);
    setProblem(undefined);
    try {
      const begun = await withStepUpIfAsked(transport, StepUpAction.LINK_PROVIDER, provider.id, (proof) =>
        createClient(AuthService, transport).beginLinkProvider({ provider: provider.id, proof }),
      );
      window.location.assign(begun.url);
    } catch (err) {
      setProblem(explain(err));
      setBusy(undefined);
    }
  };

  return (
    <Panel
      title={t`Sign-in providers`}
      className="profile-panel"
      flush
      actions={
        unlinked.length > 0 ? (
          <div className="profile-panel-actions">
            {unlinked.map((p) => {
              const name = p.displayName;
              return (
                <Button
                  key={p.id}
                  size="sm"
                  icon="plus"
                  loading={busy === p.id}
                  disabled={busy !== undefined}
                  onClick={() => void link(p)}
                >
                  <Trans>Link {name}</Trans>
                </Button>
              );
            })}
          </div>
        ) : undefined
      }
    >
      {problem ? (
        <p className="profile-error" role="alert">
          {problem}
        </p>
      ) : null}
      {links.data ? (
        list.length > 0 ? (
          <ul className="profile-list">
            {list.map((l) => (
              <LinkRow key={l.id} link={l} now={now} onUnlinked={() => void links.refetch()} />
            ))}
          </ul>
        ) : (
          <p className="profile-empty">
            <Trans>Link an account to sign in with it here. Your passkey still confirms what's risky.</Trans>
          </p>
        )
      ) : (
        <Loading label={t`Loading sign-in providers`}>
          <SkeletonRows rows={1} lead="mark" end="value" className="profile-skeleton" />
        </Loading>
      )}
    </Panel>
  );
}

function LinkRow({ link, now, onUnlinked }: { link: ProviderLink; now: number; onUnlinked: () => void }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const transport = useTransport();
  const explain = useAuthError();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const provider = link.providerName;
  const account = link.account || provider;
  const linked = link.createTime ? formatDay(timestampDate(link.createTime), i18n.locale) : "";
  const used = link.lastUsedTime ? t`used ${words.ago(timestampDate(link.lastUsedTime), now)}` : t`never used`;
  const stopped =
    link.state === ProviderLinkState.PROVIDER_OFF
      ? t`Stopped working: ${provider} no longer signs people in to Dockwell`
      : link.state === ProviderLinkState.ISSUER_MOVED
        ? t`Stopped working: ${provider} moved to another issuer`
        : "";

  const unlink = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(AuthService, transport).deleteProviderLink({ id: link.id });
      onUnlinked();
    } catch (err) {
      setProblem(explain(err));
      setConfirming(false);
    } finally {
      setBusy(false);
    }
  };

  return (
    <li className="profile-row">
      <span className="profile-row-icon" aria-hidden="true">
        <Icon name="link" size={16} />
      </span>
      <div className="profile-row-text">
        <span className="profile-row-name">{account}</span>
        <span className="profile-row-meta">
          {provider} · <Trans>Linked {linked}</Trans> · {used}
        </span>
        {stopped ? <StatusBadge status="down" label={stopped} /> : null}
        {problem ? (
          <span className="profile-error" role="alert">
            {problem}
          </span>
        ) : null}
      </div>
      {confirming ? (
        <div className="profile-row-confirm">
          {link.here ? <span className="profile-row-warn">{t`This signs you out here`}</span> : null}
          <Button size="sm" variant="danger" loading={busy} onClick={() => void unlink()}>
            <Trans>Unlink</Trans>
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
            <Trans>Keep</Trans>
          </Button>
        </div>
      ) : (
        <Button size="sm" variant="ghost" aria-label={t`Unlink ${account}`} onClick={() => setConfirming(true)}>
          <Trans>Unlink</Trans>
        </Button>
      )}
    </li>
  );
}
