// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon, PHONE_QUERY, useMediaQuery, useOffReasons } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useDocumentTitle } from "../../lib/useDocumentTitle";
import { useOpenCommandBar } from "./CommandMenu";
import "./Page.css";

type Back = "/" | "/more" | "/servers" | "/apps" | "/settings" | "/catalog" | "/projects";

/**
 * How wide a page grows, by its kind, as Layout in design/system/README.md sets out. Each is centred right of the
 * rail, and each fills the room it has at 1440px and below.
 */
export type PageWidth =
  /**
   * Pages that use the width: grids, dashboards and a server's pages, up to page-wide. Their grids gain columns and
   * their rows of panels sit side by side.
   */
  | "wide"
  /**
   * Detail pages, such as an app's, and lists whose content is a table of few columns, such as Apps: up to
   * page-standard, so a list's head, filters and table share their edges. The default.
   */
  | "standard"
  /** Forms and settings: up to page-narrow. */
  | "narrow";

export interface PageProps {
  /** The page's name: its heading, and the start of the document title. */
  title: string;
  /** Above the title on wide screens: where the page sits, such as "Apps / api". */
  breadcrumb?: ReactNode;
  /** Sets the title in mono, for machine names such as a server's. */
  machineTitle?: boolean;
  /** Next to the title on wide screens, such as a role tag. */
  badge?: ReactNode;
  /** One line of context under the title on wide screens. */
  lead?: ReactNode;
  /** Buttons on the right of the title on wide screens. */
  actions?: ReactNode;
  /** The phone header's title, when it differs, such as the server's name on the overview. */
  phoneTitle?: string;
  /** Sets the phone header's own title in mono. */
  phoneMachineTitle?: boolean;
  /** One line under the phone header's title. */
  phoneSub?: ReactNode;
  /** Where the phone header's back button goes. Pages a tab opens have none. */
  back?: Back;
  /** Where the phone header's back button goes, if not one of the usual places. */
  backLink?: ReactNode;
  /** Takes the place of the phone header's search, for a page that has its own. */
  phoneAction?: ReactNode;
  /** A quiet note on the state of the page's data, such as "Reconnecting". */
  status?: ReactNode;
  /** How wide the page grows on wide screens, by its kind. */
  width?: PageWidth;
  /**
   * The body under the head and tabs takes the whole width, as a log view or a terminal does. The head and tabs
   * keep the column of the page's kind, so they stay put when a tab with such a body opens.
   */
  fullBody?: boolean;
  children?: ReactNode;
  className?: string;
}

/**
 * A screen inside the shell. Wide screens get the title, its context and
 * actions; phones get the phone header in their place, with search and a way
 * back. Either way there is one h1.
 */
export function Page({
  title,
  breadcrumb,
  machineTitle = false,
  badge,
  lead,
  actions,
  phoneTitle,
  phoneMachineTitle = false,
  phoneSub,
  back,
  backLink,
  phoneAction,
  status,
  width = "standard",
  fullBody = false,
  children,
  className,
}: PageProps) {
  const phone = useMediaQuery(PHONE_QUERY);
  // Why its controls are off, for a phone to say once: nothing hovers there (ADR 0059).
  const off = useOffReasons();

  useDocumentTitle(title);

  return (
    <off.Provider>
      {phone ? (
        <MobileHeader
          title={phoneTitle ?? title}
          machineTitle={phoneTitle === undefined ? machineTitle : phoneMachineTitle}
          sub={phoneSub}
          back={back}
          backLink={backLink}
          action={phoneAction}
        />
      ) : null}
      <div className={cx("page", width !== "standard" && `is-${width}`, fullBody && "has-full-body", className)}>
        {phone ? (
          <>
            {status ? <div className="page-status">{status}</div> : null}
            {/* Hidden from assistive technology, which hears it on each control it explains. */}
            {off.first ? (
              <p className="page-off-note" aria-hidden="true">
                {off.first}
              </p>
            ) : null}
          </>
        ) : (
          <div className="page-head">
            <div className="page-heading">
              {breadcrumb}
              <div className="page-title-row">
                <h1 className={cx("dw-text-title", "page-title", machineTitle && "is-machine")}>{title}</h1>
                {badge}
                {status ? <div className="page-status">{status}</div> : null}
              </div>
              {lead ? <p className="page-lead">{lead}</p> : null}
            </div>
            {actions ? <div className="page-actions">{actions}</div> : null}
          </div>
        )}
        {children}
      </div>
    </off.Provider>
  );
}

interface MobileHeaderProps {
  title: string;
  machineTitle: boolean;
  sub?: ReactNode;
  back?: Back | undefined;
  backLink?: ReactNode;
  action?: ReactNode;
}

/** The phone's header: back where there's somewhere to go back to, the title, and search. */
function MobileHeader({ title, machineTitle, sub, back, backLink, action }: MobileHeaderProps) {
  const { t } = useLingui();
  const openCommandBar = useOpenCommandBar();
  return (
    <header className="mobile-header">
      {backLink ? (
        backLink
      ) : back ? (
        // Exact, as the page it leads back to is above this one, never this one: the router marks a parent as current.
        <Link to={back} className="mobile-header-button" aria-label={t`Back`} activeOptions={{ exact: true }}>
          <Icon name="arrow-left" size={20} />
        </Link>
      ) : (
        <span className="mobile-header-gap" />
      )}
      <div className="mobile-header-titles">
        <h1 className={cx("mobile-header-title", machineTitle && "is-machine")}>{title}</h1>
        {sub ? <p className="mobile-header-sub">{sub}</p> : null}
      </div>
      {action ?? (
        <button
          type="button"
          className="mobile-header-button"
          aria-label={t`Search or run a command`}
          onClick={openCommandBar}
        >
          <Icon name="search" size={20} />
        </button>
      )}
    </header>
  );
}
