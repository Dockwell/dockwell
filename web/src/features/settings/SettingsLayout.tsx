// SPDX-License-Identifier: AGPL-3.0-only

import { AuditService, MemberService, SettingsService } from "@dockwell/api";
import { Icon, type IconName, Panel, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link, Navigate, Outlet, useRouterState } from "@tanstack/react-router";
import { useId } from "react";

import { Page } from "../shell/Page";
import "./Settings.css";
import { useMay } from "../auth/may";

/** A section of Settings, where it is, and the group it's in. */
interface Section {
  to:
    | "/settings/general"
    | "/settings/members"
    | "/settings/api-tokens"
    | "/settings/audit-log"
    | "/settings/control-plane"
    | "/settings/integrations"
    | "/profile";
  label: string;
  icon: IconName;
}

/** Settings' sections, in their groups, as Settings-General.dc.html has them. */
function useSections(): { label: string; sections: Section[] }[] {
  const { t } = useLingui();
  const may = useMay();
  // A way to a place you can't read is left out (ADR 0059).
  const members: Section[] = may(MemberService.method.listMembers).allowed
    ? [{ to: "/settings/members", label: t`Members`, icon: "users" }]
    : [];
  const audit: Section[] = may(AuditService.method.listAuditEvents).allowed
    ? [{ to: "/settings/audit-log", label: t`Audit log`, icon: "file" }]
    : [];
  const controlPlane: Section[] = may(SettingsService.method.getControlPlane).allowed
    ? [{ to: "/settings/control-plane", label: t`Control plane`, icon: "server" }]
    : [];
  return [
    {
      label: t`Install`,
      sections: [
        { to: "/settings/general", label: t`General`, icon: "settings" },
        ...members,
        { to: "/settings/api-tokens", label: t`API tokens`, icon: "key" },
        ...audit,
      ],
    },
    {
      label: t`Platform`,
      sections: [...controlPlane, { to: "/settings/integrations", label: t`Integrations`, icon: "layers" }],
    },
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
  // The page keeps its place in the tree at every width, so turning a phone or narrowing a window doesn't make it
  // anew and lose what it holds, such as a token shown once.
  return (
    <div className={phone ? "settings is-phone" : "settings"}>
      {phone ? null : <SettingsNav />}
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
  // Each group's heading names it by its place, as its label is translated and may hold anything.
  const id = useId();
  if (!phone) return <Navigate to="/settings/general" replace />;
  return (
    <Page title={t`Settings`} back="/more">
      {groups.map((group, i) => (
        <section key={group.label} className="settings-list-group" aria-labelledby={`${id}-${i}`}>
          <h2 id={`${id}-${i}`} className="settings-list-label">
            {group.label}
          </h2>
          <Panel flush className="settings-list">
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
        </section>
      ))}
    </Page>
  );
}
