// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, type Session } from "@dockwell/api";
import { Button, Icon, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { useTimeWords } from "../../lib/time";
import { describeUserAgent } from "../../lib/userAgent";
import { useControlPlaneNow } from "../servers/useFleet";
import { useDeviceLabel } from "./device";
import { useAuthError } from "./messages";

/** Where you're signed in, and how to sign any of it out. */
export function SessionsPanel() {
  const { t } = useLingui();
  const sessions = useQuery(AuthService.method.listSessions, {});
  const transport = useTransport();
  const explain = useAuthError();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const others = (sessions.data?.sessions ?? []).filter((s) => !s.current).length;
  // Last seen is on the control plane's clock, as the server pages' times are.
  const now = useControlPlaneNow();

  const signOutOthers = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(AuthService, transport).revokeOtherSessions({});
      await sessions.refetch();
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel
      title={t`Sessions`}
      className="profile-panel"
      flush
      actions={
        <Button size="sm" icon="logout" loading={busy} disabled={others === 0} onClick={() => void signOutOthers()}>
          <Trans>Sign out everywhere else</Trans>
        </Button>
      }
    >
      {problem ? (
        <p className="profile-error" role="alert">
          {problem}
        </p>
      ) : null}
      {sessions.data ? (
        <ul className="profile-list">
          {sessions.data.sessions.map((s) => (
            <SessionRow key={s.id} session={s} now={now} onEnded={() => void sessions.refetch()} />
          ))}
        </ul>
      ) : (
        <p className="profile-empty" aria-busy="true">
          <Trans>Loading sessions</Trans>
        </p>
      )}
      <p className="profile-note">
        <Trans>
          Signing out everywhere else keeps this browser signed in. A session ends after 12 h unused, or 7 days.
        </Trans>
      </p>
    </Panel>
  );
}

function SessionRow({ session, now, onEnded }: { session: Session; now: number; onEnded: () => void }) {
  const { t } = useLingui();
  const words = useTimeWords();
  const transport = useTransport();
  const explain = useAuthError();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const label = useDeviceLabel();
  const device = label(session.userAgent) || t`Unknown browser`;
  const { browser } = describeUserAgent(session.userAgent);
  let seen = "";
  if (session.current) seen = t`now`;
  else if (session.lastSeenTime) seen = words.ago(timestampDate(session.lastSeenTime), now);

  const end = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(AuthService, transport).revokeSession({ id: session.id });
      onEnded();
    } catch (err) {
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <li className="profile-row">
      <span className="profile-row-icon" aria-hidden="true">
        <Icon name={browser ? "browser" : "terminal"} size={16} />
      </span>
      <div className="profile-row-text">
        <span className="profile-row-name">
          {session.current ? (
            <>
              <Trans>This browser</Trans>
              <span className="profile-current">
                <Trans>Current</Trans>
              </span>
            </>
          ) : (
            device
          )}
        </span>
        <span className="profile-row-meta">
          {device} · <code>{session.address}</code> · {session.rpId} · {seen}
        </span>
        {problem ? (
          <span className="profile-error" role="alert">
            {problem}
          </span>
        ) : null}
      </div>
      {session.current ? null : (
        <Button size="sm" variant="ghost" loading={busy} aria-label={t`Sign out ${device}`} onClick={() => void end()}>
          <Trans>Sign out</Trans>
        </Button>
      )}
    </li>
  );
}
