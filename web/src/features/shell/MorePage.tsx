// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { BackupState, DomainService, SystemService } from "@dockwell/api";
import { Icon, type IconName, type ThemePreference, useThemePreference } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useTimeWords } from "../../lib/time";
import { useSignedInUser } from "../auth/useAuthState";
import { useSignOut } from "../auth/useSignOut";
import { useBackups } from "../backups/useBackups";
import { useUpgrades } from "../catalog/useCatalog";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { Page } from "./Page";
import { AFTER_DOMAINS, BEFORE_DOMAINS, UPCOMING, type UpcomingSection } from "./sections";
import { shortVersion } from "./version";
import "./MorePage.css";

/**
 * The phone's More tab: every section the tab bar has no room for, an
 * environment's variables and secrets, the control plane's status, your
 * profile, the theme, and signing out.
 */
export function MorePage() {
  const { t, i18n } = useLingui();
  const time = useTimeWords();
  const now = useControlPlaneNow(15_000);
  const upgrades = useUpgrades();
  const { fleet } = useFleetView();
  const version = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  const [theme, setTheme] = useThemePreference();
  const user = useSignedInUser();
  const signOut = useSignOut();
  const themes: Record<ThemePreference, string> = {
    system: t({ message: "System", context: "theme" }),
    light: t`Light`,
    dark: t`Dark`,
  };

  // What M-More says beside a section: how many domains, and when the last backup was. Read once.
  const domains =
    useQuery(DomainService.method.listDomains, {}, { staleTime: Number.POSITIVE_INFINITY }).data?.domains.length ?? 0;
  const lastBackup = useBackups().data?.backups.find((b) => b.state === BackupState.DONE)?.startedAt;
  const values: Partial<Record<UpcomingSection, string>> = {
    backups: lastBackup ? time.ago(timestampDate(lastBackup), now) : undefined,
  };
  const section = (id: UpcomingSection) => {
    const { label, icon } = UPCOMING[id];
    return (
      <Link key={id} to="/$section" params={{ section: id }} className="more-row">
        <RowBody icon={icon} label={i18n._(label)} value={values[id]} />
      </Link>
    );
  };

  const operate: UpcomingSection[] = [...(fleet ? (["fleet-actions"] as const) : []), "alerts", "scheduled-tasks"];
  return (
    <Page title={t`More`}>
      <nav className="dw-panel more-group" aria-label={t`Resources`}>
        <Link to="/variables" className="more-row">
          <RowBody icon="key" label={t`Variables and secrets`} />
        </Link>
        <Link to="/websites" className="more-row">
          <RowBody icon="browser" label={t`Websites`} />
        </Link>
        <Link to="/databases" className="more-row">
          <RowBody icon="database" label={t`Databases`} />
        </Link>
        {BEFORE_DOMAINS.map(section)}
        <Link to="/domains" className="more-row">
          <RowBody icon="link" label={t`Domains`} value={domains > 0 ? String(domains) : undefined} />
        </Link>
        {AFTER_DOMAINS.map(section)}
        <Link to="/catalog" className="more-row">
          <RowBody icon="grid" label={t`Catalog`} value={upgrades > 0 ? String(upgrades) : undefined} />
        </Link>
      </nav>
      <nav className="dw-panel more-group" aria-label={t`Operate`}>
        {operate.map(section)}
      </nav>
      <nav className="dw-panel more-group" aria-label={t`Settings, status and account`}>
        <Link to="/settings" className="more-row">
          <RowBody icon="settings" label={t`Settings`} />
        </Link>
        <Link to="/settings/control-plane" className="more-row">
          <RowBody icon="gauge" label={t`Control plane`} value={version ? shortVersion(version) : undefined} />
        </Link>
        <Link to="/profile" className="more-row">
          <RowBody icon="user" label={t`Profile and security`} value={user?.name} />
        </Link>
      </nav>
      <div className="dw-panel more-group more-split">
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
        <button
          type="button"
          className="more-row more-button"
          aria-busy={signOut.busy}
          disabled={signOut.busy}
          onClick={() => void signOut.run()}
        >
          <span className="more-icon">
            <Icon name="logout" size={18} />
          </span>
          <span className="more-label">{t`Sign out`}</span>
        </button>
      </div>
      {signOut.problem ? (
        <p className="more-error" role="alert">
          {signOut.problem}
        </p>
      ) : null}
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
