// SPDX-License-Identifier: AGPL-3.0-only

import { ButtonLink, TabLink } from "@dockwell/ui";
import { createLink } from "@tanstack/react-router";

/**
 * A ButtonLink that goes to a page of the panel through the router, without
 * a reload, and takes the router's to, params and search.
 */
export const RouterButtonLink = createLink(ButtonLink);

/** A TabLink that goes to a page of the panel through the router, as RouterButtonLink does. */
export const RouterTabLink = createLink(TabLink);
