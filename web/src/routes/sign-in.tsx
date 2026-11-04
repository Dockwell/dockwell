// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { SignInPage } from "../features/auth/SignInPage";

interface SignInSearch {
  /** Where to go after signing in: a path on this panel. */
  next?: string;
}

export const Route = createFileRoute("/sign-in")({
  staticData: { public: true },
  validateSearch: (search: Record<string, unknown>): SignInSearch =>
    typeof search["next"] === "string" ? { next: search["next"] } : {},
  component: function SignIn() {
    const { next } = Route.useSearch();
    return <SignInPage next={next} />;
  },
});
