// SPDX-License-Identifier: AGPL-3.0-only

import { type ServiceInfo, UpdateKind } from "@dockwell/api";
import { cx, Panel, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { nameOf } from "../apps/app";
import { PickBox, SelectPatches, UpdatePicksBar, useUpdatePicks } from "../apps/UpdateSeveral";
import { offered, stepOf, useKindWord, withUpdates } from "../apps/updates";

/** How many apps with an update the Overview lists; the rest are a link away, on Apps. */
const SHOWN = 6;

/**
 * On the Overview: the apps with an update, each with a box to pick it by, and
 * the way to update the picked ones together, each its own change (ADR 0080).
 * Nothing while no update counts.
 */
export function OverviewUpdates({ services, now }: { services: readonly ServiceInfo[]; now: number }) {
  const { t } = useLingui();
  const kindWord = useKindWord();
  const picks = useUpdatePicks(services, now);
  const list = withUpdates(services, now);
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
          const a = offered(service.update);
          if (!a) return null;
          const name = nameOf(service);
          return (
            <li key={service.id} className="overview-update-row">
              {picks.can(service.id) ? (
                <PickBox
                  checked={picks.has(service.id)}
                  label={t`Select ${name}'s update`}
                  onPick={(on, range) => picks.toggle(order, service.id, on, range)}
                />
              ) : (
                <span className="overview-update-nopick" aria-hidden="true" />
              )}
              <Link to="/apps/$serviceId" params={{ serviceId: service.id }} className="overview-update-name">
                {name}
              </Link>
              <code className="update-step">{stepOf(a)}</code>
              <Tag className={cx("update-kind", a.kind === UpdateKind.MAJOR && "is-major")}>{kindWord(a.kind)}</Tag>
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
