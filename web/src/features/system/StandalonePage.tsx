// SPDX-License-Identifier: AGPL-3.0-only

import { Logo } from "@dockwell/ui";
import type { ReactNode } from "react";

import "./StandalonePage.css";

/**
 * A page outside the app shell, for moments before there is a shell to show:
 * first contact with the control plane, sign-in, a missing page.
 */
export function StandalonePage({ children }: { children: ReactNode }) {
  return (
    <main className="standalone">
      <div className="standalone-column">
        <Logo height={16} className="standalone-logo" />
        {children}
      </div>
    </main>
  );
}
