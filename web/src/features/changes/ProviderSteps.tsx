// SPDX-License-Identifier: AGPL-3.0-only

import { type Plan, PlanAction, type PlanStep, ProviderAction, type ProviderStep } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";

/** Whether a step is done, or still to do, for how its line reads. */
export type ProviderTense = "plan" | "done";

/**
 * What a step at Cloudflare does, in a line that says where it runs (ADR 0104, section
 * 10): a DNS record, a tunnel's rule, or a zone's mode.
 */
export function useProviderLine(): (p: ProviderStep, tense: ProviderTense, writes?: boolean) => string {
  const { t } = useLingui();
  return (p, tense, writes = true) => {
    const { name, target, to } = p;
    const done = tense === "done";
    // A step that writes nothing lets go of what's there, or hands it over, as it is
    // (the review's B-M3).
    if (!writes) {
      switch (p.action) {
        case ProviderAction.DELETE_RECORD:
        case ProviderAction.TAKE_ON_RECORD:
          return done
            ? t`Left the DNS record for ${name} as it is at Cloudflare`
            : t`Leave the DNS record for ${name} as it is at Cloudflare`;
        case ProviderAction.REMOVE_RULE:
        case ProviderAction.KEEP_RULE:
          return done
            ? t`Left ${name} in the tunnel ${target} as it is`
            : t`Leave ${name} in the tunnel ${target} as it is`;
      }
    }
    switch (p.action) {
      case ProviderAction.CREATE_RECORD:
        return done
          ? t`Wrote the DNS record for ${name} at Cloudflare`
          : t`Write the DNS record for ${name} at Cloudflare`;
      case ProviderAction.DELETE_RECORD:
        return done
          ? t`Removed the DNS record for ${name} at Cloudflare`
          : t`Remove the DNS record for ${name} at Cloudflare`;
      case ProviderAction.REPLACE_RECORD:
        return done
          ? t`Replaced the DNS record for ${name} at Cloudflare, keeping what was there`
          : t`Replace the DNS record for ${name} at Cloudflare, keeping what was there`;
      case ProviderAction.TAKE_ON_RECORD:
        return done
          ? t`Took on the DNS record for ${name}, made in Cloudflare`
          : t`Take on the DNS record for ${name}, made in Cloudflare`;
      case ProviderAction.RESTORE_RECORD:
        return done
          ? t`Put back the DNS record for ${name} at Cloudflare`
          : t`Put back the DNS record for ${name} at Cloudflare`;
      case ProviderAction.ADD_RULE:
        return done
          ? t`Added ${name} to the tunnel ${target} at Cloudflare`
          : t`Add ${name} to the tunnel ${target} at Cloudflare`;
      case ProviderAction.REMOVE_RULE:
        return done
          ? t`Removed ${name} from the tunnel ${target} at Cloudflare`
          : t`Remove ${name} from the tunnel ${target} at Cloudflare`;
      case ProviderAction.TAKE_ON_RULE:
        return done
          ? t`Took on ${name} in the tunnel ${target}, made in Cloudflare`
          : t`Take on ${name} in the tunnel ${target}, made in Cloudflare`;
      case ProviderAction.MOVE_RULE:
        return done
          ? t`Moved ${name} in the tunnel ${target} to the server's proxy`
          : t`Move ${name} in the tunnel ${target} to the server's proxy`;
      case ProviderAction.KEEP_RULE:
        return done ? t`Kept ${name} in the tunnel ${target}` : t`Keep ${name} in the tunnel ${target}`;
      case ProviderAction.SET_ZONE_MODE:
        return done ? t`Set ${name} to ${to} at Cloudflare` : t`Set ${name} to ${to} at Cloudflare`;
      case ProviderAction.RESTORE_RULE:
        return done ? t`Put back ${name} in the tunnel ${target}` : t`Put back ${name} in the tunnel ${target}`;
    }
    return done ? t`Wrote ${name} at Cloudflare` : t`Write ${name} at Cloudflare`;
  };
}

/** What a step at Cloudflare finds and leaves, under its line: what's there now, what it writes, and what else it moves. */
export function ProviderMeta({ step }: { step: ProviderStep }) {
  const { t } = useLingui();
  const { was, to, routing } = step;
  const zoneMode = step.action === ProviderAction.SET_ZONE_MODE;
  return (
    <span className="plan-step-meta">
      {was && !zoneMode ? <span>{t`Now ${was}`}</span> : null}
      {zoneMode && was ? <span>{t`From ${was}`}</span> : null}
      {to && !zoneMode && step.action !== ProviderAction.DELETE_RECORD ? <span>{t`Writes ${to}`}</span> : null}
      {/* What else moves is a notice where a rule stays for names made in Cloudflare; what stays as it went is a fact. */}
      {routing ? (
        <span className={step.action === ProviderAction.KEEP_RULE ? "plan-step-notice" : undefined}>{routing}</span>
      ) : null}
    </span>
  );
}

/** Every step a plan takes at Cloudflare, in the order they run. */
export function providerStepsOf(plan: Plan | undefined): { step: ProviderStep; writes: boolean }[] {
  return (plan?.servers ?? []).flatMap((s) =>
    s.steps.flatMap((st) => (st.provider ? [{ step: st.provider, writes: writesAt(st) }] : [])),
  );
}

/** Whether a step at Cloudflare writes there: one planned as no action lets go, or hands over. */
export function writesAt(step: PlanStep): boolean {
  return step.action !== PlanAction.NONE;
}

/**
 * A plan's steps at Cloudflare, as its review shows them beside what changes: each with
 * where it runs, what's there now and what it writes, as the plan for one server shows
 * no steps of its own.
 */
export function CloudflareSteps({ plan }: { plan: Plan | undefined }) {
  const { t } = useLingui();
  const line = useProviderLine();
  const steps = providerStepsOf(plan);
  if (steps.length === 0) return null;
  return (
    <section className="plan-section" aria-label={t`At Cloudflare`}>
      <h3 className="dw-text-label plan-heading">
        <Trans>At Cloudflare</Trans>
      </h3>
      <ol className="plan-steps">
        {steps.map(({ step, writes }, i) => (
          // Two steps can share an action and a name, as an A and an AAAA record do.
          // biome-ignore lint/suspicious/noArrayIndexKey: the plan's order is the steps' own, and never moves.
          <li key={`${i}-${step.action}-${step.name}`} className="plan-step">
            <span className="plan-step-what">
              <span className="plan-step-line">{line(step, "plan", writes)}</span>
            </span>
            <ProviderMeta step={step} />
          </li>
        ))}
      </ol>
    </section>
  );
}
