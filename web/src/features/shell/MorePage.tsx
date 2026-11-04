// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { SystemService } from "@dockwell/api";
import { Icon, type IconName, type ThemePreference, useThemePreference } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useFleetView } from "../servers/useFleet";
import { Page } from "./Page";
import { RESOURCES, UPCOMING, type UpcomingSection } from "./sections";
import { shortVersion } from "./version";
import "./MorePage.css";

/**
 * The phone's More tab: every section the tab bar has no room for, the
 * control plane's status, and the theme.
 */
export function MorePage() {
  const { t, i18n } = useLingui();
  const { fleet } = useFleetView();
  const version = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  const [theme, setTheme] = useThemePreference();
  const themes: Record<ThemePreference, string> = { system: t`System`, light: t`Light`, dark: t`Dark` };

  const section = (id: UpcomingSection) => {
    const { label, icon } = UPCOMING[id];
    return (
      <Link key={id} to="/$section" params={{ section: id }} className="more-row">
        <RowBody icon={icon} label={i18n._(label)} />
      </Link>
    );
  };

  const operate: UpcomingSection[] = [...(fleet ? (["fleet-actions"] as const) : []), "alerts", "scheduled-tasks"];
  return (
    <Page title={t`More`}>
      <nav className="dw-panel more-group" aria-label={t`Resources`}>
        {RESOURCES.filter((id) => UPCOMING[id].inMore).map(section)}
      </nav>
      <nav className="dw-panel more-group" aria-label={t`Operate`}>
        {operate.map(section)}
      </nav>
      <nav className="dw-panel more-group" aria-label={t`Settings and status`}>
        {section("settings")}
        <Link to="/status" className="more-row">
          <RowBody icon="gauge" label={t`Control plane`} value={version ? shortVersion(version) : undefined} />
        </Link>
      </nav>
      <div className="dw-panel more-group">
        <div className="more-row more-choice">
          <RowBody icon="sun" label={t`Theme`} value={themes[theme]} chevron="chevron-up-down" />
          <select
            className="more-select"
            aria-label={t`Theme`}
            value={theme}
            onChange={(event) => setTheme(event.target.value as ThemePreference)}
          >
            {(Object.keys(themes) as ThemePreference[]).map((id) => (
              <option key={id} value={id}>
                {themes[id]}
              </option>
            ))}
          </select>
        </div>
      </div>
    </Page>
  );
}

interface RowBodyProps {
  icon: IconName;
  label: ReactNode;
  value?: ReactNode;
  chevron?: IconName;
}

function RowBody({ icon, label, value, chevron = "chevron-right" }: RowBodyProps) {
  return (
    <>
      <span className="more-icon">
        <Icon name={icon} size={18} />
      </span>
      <span className="more-label">{label}</span>
      {value ? (
        <>
          {" "}
          <span className="more-value">{value}</span>
        </>
      ) : null}
      <span className="more-chevron">
        <Icon name={chevron} size={16} />
      </span>
    </>
  );
}
