// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, StepUpAction, StepUpPolicy } from "@dockwell/api";
import { Icon, Panel } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useEffect, useState } from "react";

import { formatClock } from "../../lib/format";
import { PasskeyError } from "../../lib/webauthn";
import { useAuthError } from "./messages";
import { looseness, STEP_UP_ORDER, StepUpChoices, type StepUpOption } from "./StepUpChoices";
import { withStepUpIfAsked } from "./stepUp";

/** The longest a timer waits for a cover to end before reading the choice again, as a page left open does. */
const MAX_WAIT = 24 * 60 * 60 * 1_000;

/**
 * How often your passkey is asked for again (ADR 0075): each time, once an
 * hour, or only when you sign in, never below the least the owner allows, and
 * what that comes to in this session now. Choosing asks for the passkey, as it
 * always does, and the session's cover shows at once.
 */
export function StepUpPanel() {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const policy = useQuery(AuthService.method.getStepUpPolicy, {});
  const [pending, setPending] = useState<StepUpPolicy>();
  const [problem, setProblem] = useState<string>();
  const choice = policy.data?.choice;
  const floor = choice?.floor ?? StepUpPolicy.AT_SIGN_IN;
  const ends = choice?.coverExpireTime ? timestampDate(choice.coverExpireTime).getTime() : 0;
  const { refetch } = policy;

  // The cover ends while the page is open, and says so then, not at the next visit.
  useEffect(() => {
    if (!ends) return;
    const wait = ends - Date.now();
    if (wait > MAX_WAIT) return;
    const timer = setTimeout(() => void refetch(), Math.max(wait, 0) + 1_000);
    return () => clearTimeout(timer);
  }, [ends, refetch]);

  const words: Record<(typeof STEP_UP_ORDER)[number], Omit<StepUpOption, "value">> = {
    [StepUpPolicy.EACH_TIME]: { label: t`Each time`, hint: t`Every sensitive action asks.` },
    [StepUpPolicy.HOURLY]: { label: t`Once an hour`, hint: t`A passkey covers this session for the next hour.` },
    [StepUpPolicy.AT_SIGN_IN]: { label: t`Only when I sign in`, hint: t`Signing in covers the whole session.` },
  };
  const floorWhy =
    floor === StepUpPolicy.HOURLY ? t`The owner asks at least once an hour.` : t`The owner asks each time.`;
  const options = STEP_UP_ORDER.map((value) => ({
    value,
    ...words[value],
    off: looseness(value) > looseness(floor) ? floorWhy : undefined,
  }));

  const choose = async (next: StepUpPolicy) => {
    setPending(next);
    setProblem(undefined);
    try {
      await withStepUpIfAsked(transport, StepUpAction.SET_STEP_UP_POLICY, "", (proof) =>
        createClient(AuthService, transport).setStepUpPolicy({ policy: next, proof }),
      );
      await refetch();
    } catch (err) {
      setProblem(
        err instanceof PasskeyError && err.problem === "cancelled"
          ? t`The passkey prompt closed, so your choice didn't change.`
          : explain(err),
      );
    } finally {
      setPending(undefined);
    }
  };

  // A choice below the floor reads as the floor, and says so.
  const below = choice !== undefined && looseness(choice.policy) > looseness(floor);
  let state: { covered: boolean; words: string } | undefined;
  if (choice && choice.effective !== StepUpPolicy.EACH_TIME) {
    if (choice.coverTime && choice.coverExpireTime) {
      const since = formatClock(timestampDate(choice.coverTime), i18n.locale);
      const until = formatClock(timestampDate(choice.coverExpireTime), i18n.locale);
      state = {
        covered: true,
        words: choice.coverAtSignIn
          ? t`Confirmed when you signed in at ${since}, for as long as this session lasts`
          : t`Confirmed at ${since} · asks again at ${until}`,
      };
    } else {
      state = { covered: false, words: t`Your next passkey covers this session for an hour.` };
    }
  }

  return (
    <Panel title={t`Ask for my passkey`} className="profile-panel">
      <div className="step-up">
        {/* A read that failed isn't an answer to anything done here, so it says so in its place, quietly. */}
        {policy.error ? (
          <p className="step-up-line">
            <Icon name="alert" size={14} />
            <span>{explain(policy.error)}</span>
          </p>
        ) : null}
        <StepUpChoices
          label={t`Ask for my passkey`}
          options={options}
          value={choice?.effective}
          pending={pending}
          disabled={!choice}
          onChoose={(next) => void choose(next)}
        />
        {below ? (
          <p className="step-up-line">
            <Icon name="lock" size={14} />
            <span>
              {floor === StepUpPolicy.EACH_TIME
                ? t`Your choice reads as each time, the least the owner allows.`
                : t`Your choice reads as once an hour, the least the owner allows.`}
            </span>
          </p>
        ) : null}
        {state ? (
          <p className={state.covered ? "step-up-line is-covered" : "step-up-line"} aria-live="polite">
            <Icon name={state.covered ? "check" : "clock"} size={14} />
            <span>{state.words}</span>
          </p>
        ) : null}
        {problem ? (
          <p className="profile-error" role="alert">
            {problem}
          </p>
        ) : null}
        <p className="step-up-line">
          <Icon name="shield" size={14} />
          <span>{t`Changing how you sign in, this choice, and approving someone else's change always ask.`}</span>
        </p>
      </div>
    </Panel>
  );
}
