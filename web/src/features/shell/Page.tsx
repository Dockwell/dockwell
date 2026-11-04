// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useDocumentTitle } from "../../lib/useDocumentTitle";
import { useOpenCommandBar } from "./CommandMenu";
import "./Page.css";

type Back = "/" | "/more" | "/servers";

export interface PageProps {
  /** The page's name: its heading, and the start of the document title. */
  title: string;
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
  /** A quiet note on the state of the page's data, such as "Reconnecting". */
  status?: ReactNode;
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
  machineTitle = false,
  badge,
  lead,
  actions,
  phoneTitle,
  phoneMachineTitle = false,
  phoneSub,
  back,
  status,
  children,
  className,
}: PageProps) {
  const phone = useMediaQuery(PHONE_QUERY);

  useDocumentTitle(title);

  return (
    <>
      {phone ? (
        <MobileHeader
          title={phoneTitle ?? title}
          machineTitle={phoneTitle === undefined ? machineTitle : phoneMachineTitle}
          sub={phoneSub}
          back={back}
        />
      ) : null}
      <div className={cx("page", className)}>
        {phone ? (
          status ? (
            <div className="page-status">{status}</div>
          ) : null
        ) : (
          <div className="page-head">
            <div className="page-heading">
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
    </>
  );
}

interface MobileHeaderProps {
  title: string;
  machineTitle: boolean;
  sub?: ReactNode;
  back?: Back | undefined;
}

/** The phone's header: back where there's somewhere to go back to, the title, and search. */
function MobileHeader({ title, machineTitle, sub, back }: MobileHeaderProps) {
  const { t } = useLingui();
  const openCommandBar = useOpenCommandBar();
  return (
    <header className="mobile-header">
      {back ? (
        <Link to={back} className="mobile-header-button" aria-label={t`Back`}>
          <Icon name="arrow-left" size={20} />
        </Link>
      ) : (
        <span className="mobile-header-gap" />
      )}
      <div className="mobile-header-titles">
        <h1 className={cx("mobile-header-title", machineTitle && "is-machine")}>{title}</h1>
        {sub ? <p className="mobile-header-sub">{sub}</p> : null}
      </div>
      <button
        type="button"
        className="mobile-header-button"
        aria-label={t`Search or run a command`}
        onClick={openCommandBar}
      >
        <Icon name="search" size={20} />
      </button>
    </header>
  );
}
