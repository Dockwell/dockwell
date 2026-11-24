// SPDX-License-Identifier: AGPL-3.0-only

import type { ServiceInfo } from "@dockwell/api";
import { Panel } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { environmentOf, nameOf } from "../apps/app";
import { PickBox, SelectPatches, UpdatePicksBar, useUpdatePicks } from "../apps/UpdateSeveral";
import { installOffer, offered, ofInstall, oneForEachUpdate, UpdateStep } from "../apps/updates";
import { usePlaces } from "../apps/useServices";

/** How many apps with an update the Overview lists; the rest are a link away, on Apps. */
const SHOWN = 6;

/**
 * On the Overview: the apps with an update, each with a box to pick it by, and
 * the way to update the picked ones together, each its own change (ADR 0080).
 * Nothing while no update counts.
 */
export function OverviewUpdates({ services, now }: { services: readonly ServiceInfo[]; now: number }) {
  const { t } = useLingui();
  const places = usePlaces();
  const picks = useUpdatePicks(services, now);
  // A catalog install's update is one, the install's, listed once under its name.
  const list = oneForEachUpdate(services, now);
  if (list.length === 0) return null;
  const shown = list.slice(0, SHOWN);
  const order = shown.filter((s) => picks.can(s.id)).map((s) => s.id);
  const more = list.length - shown.length;
  return (
    <Panel
      title={t`Updates`}
      className="overview-updates-panel"
      actions={picks.selected.length === 0 ? <SelectPatches picks={picks} /> : null}
    >
      <ul className="overview-update-rows" aria-label={t`Apps with an update`}>
        {shown.map((service) => {
          const install = ofInstall(service);
          const its = install ? services.filter((s) => environmentOf(s) === environmentOf(service)) : [service];
          const a = install ? installOffer(its, now)?.offer : offered(service.update);
          if (!a) return null;
          const name = install ? places.place(environmentOf(service))?.label || nameOf(service) : nameOf(service);
          const ids = its.filter((s) => picks.can(s.id)).map((s) => s.id);
          return (
            <li key={service.id} className="overview-update-row">
              {picks.can(service.id) ? (
                <PickBox
                  checked={ids.every((id) => picks.has(id))}
                  label={t`Select ${name}'s update`}
                  onPick={(on, range) => (install ? picks.setAll(ids, on) : picks.toggle(order, service.id, on, range))}
                />
              ) : (
                <span className="overview-update-nopick" aria-hidden="true" />
              )}
              <Link to="/apps/$serviceId" params={{ serviceId: service.id }} className="overview-update-name">
                {name}
              </Link>
              <UpdateStep offer={a} />
            </li>
          );
        })}
      </ul>
      {more > 0 ? (
        <Link to="/apps" search={{ show: "updates" }} className="overview-link">
          {plural(more, { one: "# more on Apps", other: "# more on Apps" })}
        </Link>
      ) : null}
      <UpdatePicksBar picks={picks} />
    </Panel>
  );
}
