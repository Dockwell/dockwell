// SPDX-License-Identifier: AGPL-3.0-only

import { PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useLocation } from "@tanstack/react-router";
import { type ReactNode, useEffect, useRef } from "react";

import { fleetShape } from "../servers/server";
import { FleetProvider, useFleet } from "../servers/useFleet";
import { CommandMenu } from "./CommandMenu";
import { Rail } from "./Rail";
import { TabBar } from "./TabBar";
import { Topbar } from "./Topbar";
import "./Shell.css";

/**
 * The frame every screen sits in: the rail and the top bar on wide screens,
 * the phone header and the tab bar below 640px, and the command bar on ⌘K.
 * The shell and its screen share one watch of the fleet.
 */
export function Shell({ children }: { children: ReactNode }) {
  return (
    <FleetProvider>
      <CommandMenu>
        <Frame>{children}</Frame>
      </CommandMenu>
    </FleetProvider>
  );
}

function Frame({ children }: { children: ReactNode }) {
  const { t } = useLingui();
  const shape = fleetShape(useFleet());
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
      {phone ? null : <Rail shape={shape} />}
      <div className="shell-column">
        {phone ? null : <Topbar shape={shape} />}
        <main id="main" ref={main} tabIndex={-1} className="shell-main">
          {children}
        </main>
        {phone ? <TabBar shape={shape} /> : null}
      </div>
    </div>
  );
}
