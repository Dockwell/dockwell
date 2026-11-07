// SPDX-License-Identifier: AGPL-3.0-only

import { PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useLocation } from "@tanstack/react-router";
import { type ReactNode, useEffect, useRef } from "react";

import { ChangeSheetProvider } from "../changes/ChangeSheet";
import { FleetProvider, useFleetView } from "../servers/useFleet";
import { CommandMenu } from "./CommandMenu";
import { Rail } from "./Rail";
import { TabBar } from "./TabBar";
import { Topbar } from "./Topbar";
import "./Shell.css";
import { ServicesLive } from "../apps/ServicesLive";

/**
 * The frame every screen sits in: the rail and the top bar on wide screens,
 * the phone header and the tab bar below 640px, the command bar on ⌘K, and
 * the plan sheet. The shell and its screen share one watch of the fleet.
 */
export function Shell({ children }: { children: ReactNode }) {
  return (
    <FleetProvider>
      <ServicesLive>
        <ChangeSheetProvider>
          <CommandMenu>
            <Frame>{children}</Frame>
          </CommandMenu>
        </ChangeSheetProvider>
      </ServicesLive>
    </FleetProvider>
  );
}

function Frame({ children }: { children: ReactNode }) {
  const { t } = useLingui();
  // The view, not the whole fleet: the frame redraws when a server joins, drops or is renamed, not for each sample.
  const view = useFleetView();
  const phone = useMediaQuery(PHONE_QUERY);
  const main = useRef<HTMLElement>(null);
  const pathname = useLocation({ select: (location) => location.pathname });

  // After a move to another page, focus starts at the new page, so a keyboard
  // or screen reader user doesn't land back at the top of the rail.
  const shown = useRef(pathname);
  useEffect(() => {
    if (shown.current === pathname) return;
    shown.current = pathname;
    main.current?.focus({ preventScroll: true });
  }, [pathname]);

  return (
    <div className="shell">
      <a className="shell-skip" href="#main">
        {t`Skip to content`}
      </a>
      {phone ? null : <Rail view={view} />}
      <div className="shell-column">
        {phone ? null : <Topbar view={view} />}
        <main id="main" ref={main} tabIndex={-1} className="shell-main">
          {children}
        </main>
        {phone ? <TabBar view={view} /> : null}
      </div>
    </div>
  );
}
