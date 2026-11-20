// SPDX-License-Identifier: AGPL-3.0-only

import { type AlertCheckInfo, ChangeService } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";

import { useAllDomains } from "../apps/domains";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { CHECK_DEFAULTS, MOST_CHECKS, nameFor, unwatchedDomains } from "./checks";

/**
 * Routed domains no check watches yet, said once above the checks with one way to
 * watch them all, as one plan on the defaults (#835). Past the install's 200 checks,
 * it offers what fits and says so. Nothing shows where every routed domain is
 * watched, or for someone who may not plan.
 */
export function WatchRouted({ checks }: { checks: readonly AlertCheckInfo[] }) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const domains = useAllDomains().last?.domains;
  if (!domains || !mayPlan.allowed) return null;
  const unwatched = unwatchedDomains(domains, checks);
  if (unwatched.length === 0) return null;
  const room = Math.max(0, MOST_CHECKS - checks.length);
  const watched = unwatched.slice(0, room);
  const count = unwatched.length;
  const watch = () => {
    const taken = checks.map((c) => c.name);
    // Each marked as its route's, so it goes when the route does, as one Dockwell
    // makes for a new route (#835).
    const edits = watched.map(({ url, routeId }) => {
      const name = nameFor(url, taken);
      taken.push(name);
      return {
        resource: { kind: "alert.check", name },
        spec: {
          kind: {
            case: "alertCheck" as const,
            value: {
              address: url,
              every: CHECK_DEFAULTS.every,
              downAfter: CHECK_DEFAULTS.downAfter,
              forRoute: routeId,
            },
          },
        },
      };
    });
    sheet.open({
      kind: "propose",
      summary: plural(edits.length, { one: "Watch # routed domain", other: "Watch # routed domains" }),
      edits,
    });
  };
  return (
    <Callout
      tone="info"
      className="alerts-watch-routed"
      title={plural(count, {
        one: "# routed domain isn't watched",
        other: "# routed domains aren't watched",
      })}
      actions={
        watched.length > 0 ? (
          <Button size="sm" icon="eye" onClick={watch}>
            {watched.length === count
              ? plural(count, { one: "Watch it", other: "Watch them" })
              : plural(watched.length, { one: "Watch the first #", other: "Watch the first #" })}
          </Button>
        ) : undefined
      }
    >
      {watched.length === count
        ? t`Every minute, down after 2 failures, from the control server.`
        : room === 0
          ? t`The install has ${MOST_CHECKS} checks, the most it keeps. Remove one to watch another.`
          : t`The install keeps ${MOST_CHECKS} checks, so ${room} more fit.`}
    </Callout>
  );
}
