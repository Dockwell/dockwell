// SPDX-License-Identifier: AGPL-3.0-only

import { Panel, SegmentedControl, type ThemePreference, useThemePreference } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

/**
 * How the panel looks for you, as Profile has it: the theme, which this browser
 * keeps, as More and the command bar set it too. Language follows the browser.
 * On a phone its options are as tall as Profile's other buttons, in
 * ProfilePage.css.
 */
export function PreferencesPanel() {
  const { t } = useLingui();
  const [theme, setTheme] = useThemePreference();
  const said: Record<ThemePreference, string> = {
    system: t`System follows your device`,
    light: t`Light, whatever your device uses`,
    dark: t`Dark, whatever your device uses`,
  };
  return (
    <Panel title={t`Preferences`} className="profile-panel">
      <div className="profile-pref">
        <div className="profile-pref-text">
          <span className="profile-pref-name">{t`Theme`}</span>
          <span className="profile-row-meta">{said[theme]}</span>
        </div>
        <SegmentedControl<ThemePreference>
          size="sm"
          label={t`Theme`}
          value={theme}
          onChange={setTheme}
          options={[
            { value: "system", label: t({ message: "System", context: "theme" }) },
            { value: "light", label: t`Light`, icon: "sun" },
            { value: "dark", label: t`Dark`, icon: "moon" },
          ]}
        />
      </div>
    </Panel>
  );
}
