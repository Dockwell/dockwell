// SPDX-License-Identifier: AGPL-3.0-only

import { Logo } from "@dockwell/ui";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useDocumentTitle } from "../../lib/useDocumentTitle";
import "./StandalonePage.css";

/**
 * A page outside the app shell: the control plane's status, sign-in, a
 * missing page. The logo leads back to the overview, since an installed
 * panel has no back button. The title names the browser tab.
 */
export function StandalonePage({ title, children }: { title: string; children: ReactNode }) {
  useDocumentTitle(title);
  return (
    <main className="standalone">
      <div className="standalone-column">
        <Link to="/" className="standalone-logo">
          <Logo height={16} />
        </Link>
        {children}
      </div>
    </main>
  );
}
