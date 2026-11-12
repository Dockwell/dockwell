// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, Role, SettingsService, StepUpAction, StepUpPolicy } from "@dockwell/api";
import { Callout, Icon, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { PasskeyError } from "../../lib/webauthn";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { STEP_UP_ORDER, StepUpChoices, type StepUpOption } from "../auth/StepUpChoices";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedIn } from "../auth/useAuthState";
import { Page } from "../shell/Page";
import "../auth/MayEdit.css";
import "./Settings.css";

/**
 * Security, for the whole install: the least anyone may choose for how often
 * their passkey is asked for again (ADR 0075). The owner sets it, with a
 * passkey; everyone else reads it, with who sets it.
 */
export function SecurityPage() {
  const { t } = useLingui();
  const settings = useQuery(SettingsService.method.getSettings, {});
  const transport = useTransport();
  const queryClient = useQueryClient();
  const explain = useAuthError();
  const may = useMay();
  const owner = useSignedIn()?.user?.role === Role.OWNER;
  const maySet = may(SettingsService.method.setStepUpFloor, { install: true });
  const [pending, setPending] = useState<StepUpPolicy>();
  const [problem, setProblem] = useState<string>();
  const floor = settings.data?.settings?.stepUpFloor;

  const words: Record<(typeof STEP_UP_ORDER)[number], Omit<StepUpOption, "value">> = {
    [StepUpPolicy.EACH_TIME]: { label: t`Each time`, hint: t`Everyone confirms every sensitive action.` },
    [StepUpPolicy.HOURLY]: { label: t`At least once an hour`, hint: t`Nobody can choose only when they sign in.` },
    [StepUpPolicy.AT_SIGN_IN]: {
      label: t`Let each person choose`,
      hint: t`Each time, once an hour, or only when they sign in.`,
    },
  };
  const options = STEP_UP_ORDER.map((value) => ({ value, ...words[value] }));

  const choose = async (next: StepUpPolicy) => {
    setPending(next);
    setProblem(undefined);
    try {
      await withStepUpIfAsked(transport, StepUpAction.SET_STEP_UP_FLOOR, "", (proof) =>
        createClient(SettingsService, transport).setStepUpFloor({ floor: next, proof }),
      );
      // What each person's choice reads as follows the floor.
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: SettingsService.method.getSettings, cardinality: undefined }),
        }),
        queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: AuthService.method.getStepUpPolicy, cardinality: undefined }),
        }),
      ]);
    } catch (err) {
      setProblem(
        err instanceof PasskeyError && err.problem === "cancelled"
          ? t`The passkey prompt closed, so the floor didn't change.`
          : explain(err),
      );
    } finally {
      setPending(undefined);
    }
  };

  const why = !owner ? t`Only the owner sets this.` : maySet.reason;
  return (
    <Page
      width="narrow"
      title={t`Security`}
      lead={t`How often everyone in this install is asked for their passkey again`}
      back="/settings"
    >
      {settings.error ? <Callout tone="critical">{sentence(settings.error)}</Callout> : null}
      <Panel title={t`Ask for a passkey`} className="security-panel">
        <div className="step-up">
          <span className="dw-field-label">
            <Trans>The least anyone may choose</Trans>
          </span>
          {why ? (
            <p className="may-edit-why">
              <Icon name="lock" size={14} />
              <span>{why}</span>
            </p>
          ) : null}
          <StepUpChoices
            label={t`The least anyone may choose`}
            options={options}
            value={floor}
            pending={pending}
            disabled={floor === undefined || !owner || !maySet.allowed}
            onChoose={(next) => void choose(next)}
          />
          {problem ? <Callout tone="critical">{problem}</Callout> : null}
          <p className="step-up-line">
            <Icon name="user" size={14} />
            <span>
              <Trans>
                Each person chooses theirs in{" "}
                <Link to="/profile" className="cp-link">
                  Profile and security
                </Link>
                .
              </Trans>
            </span>
          </p>
        </div>
      </Panel>
    </Page>
  );
}
