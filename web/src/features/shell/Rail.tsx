// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { AlertService, Role, SettingsService, SystemService } from "@dockwell/api";
import { cx, Icon, type IconName, Logo } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { Link, useLocation } from "@tanstack/react-router";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useSignedInUser } from "../auth/useAuthState";
import { initials, useRoleName } from "../auth/user";
import { useUpgrades } from "../catalog/useCatalog";
import { useWaitingForAPerson } from "../changes/waiting";
import type { FleetView } from "../servers/server";
import {
  AFTER_DOMAINS,
  BEFORE_DOMAINS,
  firstSegment,
  inActivity,
  OPERATE,
  UPCOMING,
  type UpcomingSection,
} from "./sections";
import { shortVersion } from "./version";

/**
 * The navigation rail. With one server it names that server under Machine
 * and shows the version; from the second server on, Fleet holds the server
 * list and fleet actions instead. You, signed in, are at the foot, a link to
 * your profile. Below 960px it narrows to icons.
 */
export function Rail({ view }: { view: FleetView }) {
  const { t, i18n } = useLingui();
  const upgrades = useUpgrades();
  const waiting = useWaitingForAPerson().length;
  const segment = firstSegment(useLocation({ select: (location) => location.pathname }));
  const version = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  // How many apps there are, beside Apps, as the rail has it in the mocks.
  const apps = useServices().data?.length;

  const may = useMay();
  const section = (id: UpcomingSection) => {
    // A way to a place you can't read is left out (ADR 0059).
    if (id === "alerts" && !may(AlertService.method.listAlerts).allowed) return null;
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

  const servers = view.servers?.length ?? 0;
  const short = version ? shortVersion(version) : undefined;
  return (
    <nav className="rail" aria-label={t`Main`}>
      <div className="rail-brand">
        <Logo variant="logotype" height={14} className="rail-logo" />
        <Logo variant="mark" height={14} className="rail-mark" />
        {!view.fleet && short && !may(SettingsService.method.getControlPlane).allowed ? (
          <span className="rail-version">{short}</span>
        ) : !view.fleet && short ? (
          <Link
            to="/settings/control-plane"
            className="rail-version"
            aria-label={t`Dockwell ${short}: control plane status`}
          >
            {short}
          </Link>
        ) : null}
      </div>
      <Link to="/" activeOptions={{ exact: true }} {...current(segment === "")}>
        <ItemBody icon="home" label={t`Overview`} />
      </Link>
      <Link to="/apps" {...current(segment === "apps")}>
        <ItemBody icon="service" label={t`Apps`} count={apps ? String(apps) : undefined} countSeenOnly />
      </Link>
      <Link to="/websites" {...current(segment === "websites")}>
        <ItemBody icon="browser" label={t`Websites`} />
      </Link>
      <Link to="/databases" {...current(segment === "databases")}>
        <ItemBody icon="database" label={t`Databases`} />
      </Link>
      {BEFORE_DOMAINS.map(section)}
      <Link to="/domains" {...current(segment === "domains")}>
        <ItemBody icon="link" label={t`Domains`} />
      </Link>
      {AFTER_DOMAINS.map(section)}
      <Link to="/catalog" {...current(segment === "catalog")}>
        <ItemBody icon="grid" label={t`Catalog`} count={upgrades > 0 ? String(upgrades) : undefined} />
      </Link>
      {view.fleet ? (
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
          {view.only ? (
            <Link
              to="/servers/$serverId"
              params={{ serverId: view.only.id }}
              activeOptions={{ exact: true }}
              {...current(segment === "servers")}
            >
              <ItemBody icon="server" label={t`Server`} count={view.only.name} machineCount />
            </Link>
          ) : (
            <Link to="/servers" activeOptions={{ exact: true }} {...current(segment === "servers")}>
              <ItemBody icon="server" label={t`Server`} />
            </Link>
          )}
        </>
      )}
      <div className="rail-section">{t`Operate`}</div>
      <Link to="/activity" activeOptions={{ exact: true }} {...current(inActivity(segment))}>
        <ItemBody
          icon="activity"
          label={t`Activity`}
          count={waiting > 0 ? String(waiting) : undefined}
          countWords={
            waiting > 0
              ? plural(waiting, { one: "# change needs your approval", other: "# changes need your approval" })
              : undefined
          }
        />
      </Link>
      {OPERATE.map(section)}
      <div className="rail-fill" />
      <Link to="/settings" {...current(segment === "settings")}>
        <ItemBody icon="settings" label={t`Settings`} />
      </Link>
      <Profile active={segment === "profile"} />
    </nav>
  );
}

/** Who is signed in, with their role, leading to their profile and security settings. */
function Profile({ active }: { active: boolean }) {
  const user = useSignedInUser();
  const role = useRoleName(user?.role ?? Role.UNSPECIFIED);
  if (!user) return null;
  const { className, ...rest } = current(active);
  return (
    <Link to="/profile" className={cx(className, "rail-profile")} {...rest}>
      <span className="rail-avatar" aria-hidden="true">
        {initials(user.name)}
      </span>
      <span className="rail-who">
        <span className="rail-label rail-name">{user.name}</span>
        {role ? (
          <>
            {" "}
            <span className="rail-role">{role}</span>
          </>
        ) : null}
      </span>
      <span className="rail-tip" aria-hidden="true">
        {user.name}
      </span>
    </Link>
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
  /** What the count says, for those who don't see it, in place of the bare number. */
  countWords?: string | undefined;
  /** The count is a machine value, such as a server's name. */
  machineCount?: boolean;
  /** The count is a glance at what the page says in full, so screen readers hear the label alone. */
  countSeenOnly?: boolean;
}

function ItemBody({ icon, label, count, countWords, machineCount = false, countSeenOnly = false }: ItemBodyProps) {
  return (
    <>
      <Icon name={icon} size={16} />
      <span className="rail-label">{label}</span>
      {/* The space keeps the name "Server hz-1" where styles don't separate the two. */}
      {count ? (
        <>
          {" "}
          <span
            className={cx("rail-count", machineCount && "is-machine")}
            aria-hidden={countWords || countSeenOnly ? true : undefined}
          >
            {count}
          </span>
          {countWords ? <span className="dw-sr">{countWords}</span> : null}
        </>
      ) : null}
      {/* The narrow rail's tooltip. The label already names the link, so screen readers skip this copy. */}
      <span className="rail-tip" aria-hidden="true">
        {label}
      </span>
    </>
  );
}
