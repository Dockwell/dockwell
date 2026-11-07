// SPDX-License-Identifier: AGPL-3.0-only

import { Icon, type IconName, Panel, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link, Navigate, Outlet, useRouterState } from "@tanstack/react-router";

import { Page } from "../shell/Page";
import "./Settings.css";

/** A section of Settings, where it is, and the group it's in. */
interface Section {
  to: "/settings/general" | "/profile";
  label: string;
  icon: IconName;
}

/** Settings' sections, in their groups, as Settings-General.dc.html has them. */
function useSections(): { label: string; sections: Section[] }[] {
  const { t } = useLingui();
  return [
    { label: t`Install`, sections: [{ to: "/settings/general", label: t`General`, icon: "settings" }] },
    { label: t`Account`, sections: [{ to: "/profile", label: t`Profile and security`, icon: "user" }] },
  ];
}

/**
 * Settings: on wide screens, its sections beside the one that's open, as the
 * mocks have them; on a phone, the one that's open alone, with a way back to the
 * list (SettingsHome).
 */
export function SettingsLayout() {
  const phone = useMediaQuery(PHONE_QUERY);
  if (phone) return <Outlet />;
  return (
    <div className="settings">
      <SettingsNav />
      <div className="settings-page">
        <Outlet />
      </div>
    </div>
  );
}

function SettingsNav() {
  const { t } = useLingui();
  const groups = useSections();
  const path = useRouterState({ select: (s) => s.location.pathname });
  return (
    <nav className="settings-nav" aria-label={t`Settings`}>
      {groups.map((group) => (
        <div key={group.label} className="settings-nav-group">
          <div className="settings-nav-label">{group.label}</div>
          {group.sections.map((section) => {
            const here = path === section.to;
            return (
              <Link
                key={section.to}
                to={section.to}
                className={here ? "settings-nav-item is-active" : "settings-nav-item"}
                aria-current={here ? "page" : undefined}
              >
                {section.label}
              </Link>
            );
          })}
        </div>
      ))}
    </nav>
  );
}

/** /settings: on a phone, the sections to choose from; on wide screens, General, as it leads them. */
export function SettingsHome() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const groups = useSections();
  if (!phone) return <Navigate to="/settings/general" replace />;
  return (
    <Page title={t`Settings`} back="/more">
      {groups.map((group) => (
        <Panel key={group.label} flush className="settings-list">
          <nav aria-label={group.label}>
            {group.sections.map((section) => (
              <Link key={section.to} to={section.to} className="settings-row">
                <span className="settings-row-icon">
                  <Icon name={section.icon} size={18} />
                </span>
                <span className="settings-row-label">{section.label}</span>
                <Icon name="chevron-right" size={16} />
              </Link>
            ))}
          </nav>
        </Panel>
      ))}
    </Page>
  );
}
