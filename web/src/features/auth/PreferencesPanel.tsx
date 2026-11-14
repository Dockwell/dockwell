// SPDX-License-Identifier: AGPL-3.0-only

import { Panel, SegmentedControl, SelectField, type ThemePreference, useThemePreference } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { chooseLocale, chosenLocale, LOCALES, type Locale, pickLocale } from "../../app/i18n";
import { RouterButtonLink } from "../../lib/links";

/**
 * How the panel looks for you, as Profile has it: the theme, which this browser
 * keeps, as More and the command bar set it too; the language, this browser's too;
 * and where alerts reach you. On a phone the theme's options are as tall as
 * Profile's other buttons, in ProfilePage.css.
 */
export function PreferencesPanel() {
  const { t } = useLingui();
  const [theme, setTheme] = useThemePreference();
  const [chosen, setChosen] = useState<Locale | "">(() => chosenLocale() ?? "");
  // A browser may say no languages at all, as a page whose navigator is stood in for does.
  const browser = LOCALES[pickLocale(navigator.languages ?? [])];
  const said: Record<ThemePreference, string> = {
    system: t`System follows your device`,
    light: t`Light, whatever your device uses`,
    dark: t`Dark, whatever your device uses`,
  };
  return (
    <Panel title={t`Preferences`} className="profile-panel">
      <div className="profile-prefs">
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
        <div className="profile-pref">
          <div className="profile-pref-text">
            <span className="profile-pref-name" id="profile-language">{t`Language`}</span>
            <span className="profile-row-meta">{t`This browser only`}</span>
          </div>
          <SelectField
            size="sm"
            className="profile-pref-select"
            aria-labelledby="profile-language"
            options={[
              { value: "", label: t`Same as this browser (${browser})` },
              ...(Object.keys(LOCALES) as Locale[]).map((l) => ({ value: l, label: LOCALES[l] })),
            ]}
            value={chosen}
            onChange={(event) => {
              const next = event.target.value as Locale | "";
              setChosen(next);
              void chooseLocale(next === "" ? undefined : next);
            }}
          />
        </div>
        <div className="profile-pref">
          <div className="profile-pref-text">
            <span className="profile-pref-name">{t`Notifications`}</span>
            <span className="profile-row-meta">{t`Which alerts reach you, and where`}</span>
          </div>
          <RouterButtonLink to="/alerts/channels" size="sm" icon="bell">
            {t`Alert channels`}
          </RouterButtonLink>
        </div>
      </div>
    </Panel>
  );
}
