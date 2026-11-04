// SPDX-License-Identifier: AGPL-3.0-only

import { Logo } from "@dockwell/ui";
import type { ReactNode } from "react";

import "./StandalonePage.css";

/**
 * A page outside the app shell: the control plane's status, sign-in, a
 * missing page. The logo leads back to the overview, since an installed
 * panel has no back button.
 */
export function StandalonePage({ children }: { children: ReactNode }) {
  return (
    <main className="standalone">
      <div className="standalone-column">
        <a href="/" className="standalone-logo">
          <Logo height={16} />
        </a>
        {children}
      </div>
    </main>
  );
}
