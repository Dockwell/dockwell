// SPDX-License-Identifier: AGPL-3.0-only

import { ButtonLink } from "@dockwell/ui";
import { createLink } from "@tanstack/react-router";

/** A ButtonLink that goes to a page of the panel through the router, without reloading it. */
export const RouterButtonLink = createLink(ButtonLink);
