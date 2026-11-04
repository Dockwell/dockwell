// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { SystemService } from "@dockwell/api";
import { cx, Icon, type IconName, Logo } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link, useLocation } from "@tanstack/react-router";

import type { FleetShape } from "../servers/server";
import { firstSegment, OPERATE, RESOURCES, UPCOMING, type UpcomingSection } from "./sections";
import { shortVersion } from "./version";

/**
 * The navigation rail. With one server it names that server under Machine
 * and shows the version; from the second server on, Fleet holds the server
 * list and fleet actions instead. Below 960px it narrows to icons.
 */
export function Rail({ shape }: { shape: FleetShape }) {
  const { t, i18n } = useLingui();
  const segment = firstSegment(useLocation({ select: (location) => location.pathname }));
  const version = useQuery(SystemService.method.getVersion, {}).data?.build?.version;

  const section = (id: UpcomingSection) => {
    const { label, icon } = UPCOMING[id];
    return (
      <Link
        key={id}
        to="/$section"
        params={{ section: id }}
        activeOptions={{ exact: true }}
        {...current(segment === id)}
      >
        <ItemBody icon={icon} label={i18n._(label)} />
      </Link>
    );
  };

  const servers = shape.servers?.length ?? 0;
  const short = version ? shortVersion(version) : undefined;
  return (
    <nav className="rail" aria-label={t`Main`}>
      <div className="rail-brand">
        <Logo variant="logotype" height={14} className="rail-logo" />
        <Logo variant="mark" height={14} className="rail-mark" />
        {!shape.fleet && short ? (
          <Link to="/status" className="rail-version" aria-label={t`Dockwell ${short}: control plane status`}>
            {short}
          </Link>
        ) : null}
      </div>
      <Link to="/" activeOptions={{ exact: true }} {...current(segment === "")}>
        <ItemBody icon="home" label={t`Overview`} />
      </Link>
      {RESOURCES.map(section)}
      {shape.fleet ? (
        <>
          <div className="rail-section">{t`Fleet`}</div>
          <Link to="/servers" activeOptions={{ exact: true }} {...current(segment === "servers")}>
            <ItemBody icon="server" label={t`Servers`} count={String(servers)} />
          </Link>
          {section("fleet-actions")}
        </>
      ) : (
        <>
          <div className="rail-section">{t`Machine`}</div>
          <Link to="/servers" activeOptions={{ exact: true }} {...current(segment === "servers")}>
            <ItemBody icon="server" label={t`Server`} count={shape.only?.name} machineCount />
          </Link>
        </>
      )}
      <div className="rail-section">{t`Operate`}</div>
      {OPERATE.map(section)}
      <div className="rail-fill" />
      {section("settings")}
    </nav>
  );
}

/** Marks the item for the section the page is in, which may be a page below it. */
function current(active: boolean) {
  return {
    className: cx("rail-item", active && "is-active"),
    "aria-current": active ? ("page" as const) : undefined,
  };
}

interface ItemBodyProps {
  icon: IconName;
  label: string;
  count?: string | undefined;
  /** The count is a machine value, such as a server's name. */
  machineCount?: boolean;
}

function ItemBody({ icon, label, count, machineCount = false }: ItemBodyProps) {
  return (
    <>
      <Icon name={icon} size={16} />
      <span className="rail-label">{label}</span>
      {/* The space keeps the name "Server hz-1" where styles don't separate the two. */}
      {count ? (
        <>
          {" "}
          <span className={cx("rail-count", machineCount && "is-machine")}>{count}</span>
        </>
      ) : null}
      {/* The narrow rail's tooltip. The label already names the link, so screen readers skip this copy. */}
      <span className="rail-tip" aria-hidden="true">
        {label}
      </span>
    </>
  );
}
