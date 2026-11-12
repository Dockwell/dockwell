// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon, type IconName } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { Link, useLocation } from "@tanstack/react-router";

import { useWaitingForAPerson } from "../changes/waiting";
import type { FleetView } from "../servers/server";
import { firstSegment, inActivity, isUpcoming, UPCOMING } from "./sections";

type Tab = "overview" | "apps" | "servers" | "activity" | "more";

/**
 * The tab a path belongs to. Sections without a tab of their own, your
 * profile, Settings, and an environment's variables and secrets live under More.
 */
export function tabOf(pathname: string): Tab {
  const segment = firstSegment(pathname);
  if (segment === "") return "overview";
  if (inActivity(segment)) return "activity";
  if (segment === "apps" || segment === "servers") return segment;
  if (
    segment === "more" ||
    segment === "profile" ||
    segment === "settings" ||
    segment === "variables" ||
    segment === "projects" ||
    segment === "catalog" ||
    segment === "domains" ||
    segment === "databases" ||
    segment === "websites" ||
    (isUpcoming(segment) && UPCOMING[segment].inMore)
  )
    return "more";
  return "overview";
}

/** The phone's tab bar, in place of the rail below 640px. */
export function TabBar({ view }: { view: FleetView }) {
  const { t } = useLingui();
  const pathname = useLocation({ select: (location) => location.pathname });
  const tab = tabOf(pathname);
  const waiting = useWaitingForAPerson().length;
  const props = (id: Tab) => ({
    className: cx("tabbar-item", tab === id && "is-active"),
    "aria-current": tab === id ? ("page" as const) : undefined,
    activeOptions: { exact: true },
  });
  // An app's logs have all of a phone's screen, as what is wrong is what matters.
  if (/^\/apps\/[^/]+\/logs\/?$/.test(pathname)) return null;
  return (
    <nav className="tabbar" aria-label={t`Main`}>
      <Link to="/" {...props("overview")}>
        <TabBody icon="home" label={t`Overview`} />
      </Link>
      <Link to="/apps" {...props("apps")}>
        <TabBody icon="service" label={t`Apps`} />
      </Link>
      {view.only ? (
        <Link to="/servers/$serverId" params={{ serverId: view.only.id }} {...props("servers")}>
          <TabBody icon="server" label={t`Server`} />
        </Link>
      ) : (
        <Link to="/servers" {...props("servers")}>
          <TabBody icon="server" label={view.fleet ? t`Servers` : t`Server`} />
        </Link>
      )}
      <Link to="/activity" {...props("activity")}>
        <TabBody
          icon="activity"
          label={t`Activity`}
          count={
            waiting > 0
              ? plural(waiting, { one: "# change needs your approval", other: "# changes need your approval" })
              : undefined
          }
        />
      </Link>
      <Link to="/more" {...props("more")}>
        <TabBody icon="menu" label={t`More`} />
      </Link>
    </nav>
  );
}

/** A tab: its icon and name, and with count, a dot that says something waits there, in words for those who don't see it. */
function TabBody({ icon, label, count }: { icon: IconName; label: string; count?: string | undefined }) {
  return (
    <>
      <span className="tabbar-icon">
        <Icon name={icon} size={22} />
        {count ? <span className="tabbar-dot" aria-hidden="true" /> : null}
      </span>
      <span>{label}</span>
      {count ? (
        <>
          {" "}
          <span className="dw-sr">{count}</span>
        </>
      ) : null}
    </>
  );
}
