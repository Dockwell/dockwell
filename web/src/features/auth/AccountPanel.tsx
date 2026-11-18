// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { formatDay } from "../../lib/format";
import { useClaimCommand, useSignedIn } from "./useAuthState";
import { initials, useRoleName } from "./user";

/**
 * Your account, first on Profile and security, as the Profile mock has it: who you
 * are here, your role and since when, the email you sign in with, and the way back in
 * when every other is lost (#801). How you sign in was on Members, and is here now.
 */
export function AccountPanel() {
  const { t, i18n } = useLingui();
  const auth = useSignedIn();
  const user = auth?.user;
  const role = useRoleName(user?.role ?? 0);
  // dockwell recover runs as dockwell claim does, as root under the system service.
  const recover = useClaimCommand().replace(/claim$/, "recover");
  const since = user?.createTime ? formatDay(timestampDate(user.createTime), i18n.locale) : "";
  const meta = [role, since ? t`member since ${since}` : ""].filter(Boolean).join(" · ");
  return (
    <Panel title={t`Account`} className="profile-panel">
      <div className="profile-account" aria-busy={user === undefined}>
        <div className="profile-account-who">
          <span className="profile-avatar" aria-hidden="true">
            {initials(user?.name ?? "")}
          </span>
          <span className="dw-cell">
            <span className="profile-account-name">{user?.name}</span>
            {meta ? <span className="settings-meta">{meta}</span> : null}
          </span>
        </div>
        <dl className="settings-facts">
          <dt>{t`Email`}</dt>
          <dd>
            <code>{user?.email}</code>
          </dd>
          <dt>{t`Last resort`}</dt>
          <dd>
            <Trans>
              <code>{recover}</code> on the control server
            </Trans>
          </dd>
        </dl>
      </div>
    </Panel>
  );
}
