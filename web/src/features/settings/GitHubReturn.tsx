// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { GitProviderService } from "@dockwell/api";
import { Callout, Glyph, STATUS } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useAuthError } from "../auth/messages";
import { Page } from "../shell/Page";
import "./Settings.css";
import "./GitHub.css";

/** A step of connecting GitHub, under way or refused, with the way back to Integrations. */
function Step({ title, working, problem }: { title: string; working: ReactNode; problem: string | undefined }) {
  const { t } = useLingui();
  return (
    <Page width="narrow" title={title} back="/settings">
      {problem ? (
        <Callout
          tone="critical"
          title={t`GitHub isn't connected`}
          actions={
            <Link to="/settings/integrations" className="dw-btn dw-btn-secondary dw-btn-sm">
              <span>
                <Trans>Back to Integrations</Trans>
              </span>
            </Link>
          }
        >
          {problem}
        </Callout>
      ) : (
        <p className="github-delivery" role="status" aria-live="polite">
          <Glyph shape={STATUS.deploying.shape} size={12} />
          <span>{working}</span>
        </p>
      )}
    </Page>
  );
}

/**
 * Where GitHub sends the browser once it made the App (ADR 0098): Dockwell trades
 * the code for the App, keeps it, and sends the browser on to install it on the
 * repositories you pick. The code works once, so it's sent once.
 */
export function GitHubCreated({ code, state }: { code: string; state: string }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const [problem, setProblem] = useState<string>();
  const sent = useRef(false);
  useEffect(() => {
    if (sent.current) return;
    sent.current = true;
    if (!code || !state) {
      setProblem(t`GitHub sent the browser back without the App it made. Connect GitHub again from Integrations.`);
      return;
    }
    createClient(GitProviderService, transport)
      .finishGitHubApp({ code, state })
      .then((res) => window.location.assign(res.installUrl))
      .catch((err: unknown) => setProblem(explain(err) || sentence(err)));
  }, [code, state, transport, explain, t]);
  return <Step title={t`Connecting GitHub`} working={t`Keeping the App GitHub made`} problem={problem} />;
}

/**
 * Where GitHub sends the browser once the App is installed on an account (ADR 0098):
 * Dockwell asks GitHub, as the App, that the installation is its own before it keeps
 * it, then shows Integrations. An organization's owner who must approve it first
 * sends nothing yet.
 */
export function GitHubInstalled({
  /** The App, by the ID GitHub's setup URL names: its webhook's, as the manifest came before the App's own. */
  appId,
  installationId,
  action,
}: {
  appId: string;
  installationId: string;
  action: string;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const navigate = useNavigate();
  const [problem, setProblem] = useState<string>();
  const sent = useRef(false);
  const requested = action === "request";
  useEffect(() => {
    if (sent.current || requested) return;
    sent.current = true;
    if (!/^\d+$/.test(installationId)) {
      setProblem(t`GitHub sent the browser back without the installation. Install the App again from Integrations.`);
      return;
    }
    createClient(GitProviderService, transport)
      .addGitHubInstallation({ id: appId, installationId: BigInt(installationId) })
      .then(() => navigate({ to: "/settings/integrations" }))
      .catch((err: unknown) => setProblem(explain(err) || sentence(err)));
  }, [appId, installationId, requested, transport, navigate, explain, t]);
  if (requested) {
    return (
      <Page width="narrow" title={t`Installing the GitHub App`} back="/settings">
        <Callout tone="info" title={t`The organization's owner approves it first`}>
          {t`GitHub asked them. Once they approve it, its repositories show here.`}
        </Callout>
      </Page>
    );
  }
  return (
    <Step title={t`Installing the GitHub App`} working={t`Checking the installation with GitHub`} problem={problem} />
  );
}
