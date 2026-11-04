// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ProfilePage, type ProfileVia } from "../features/auth/ProfilePage";

interface ProfileSearch {
  via?: ProfileVia;
}

export const Route = createFileRoute("/profile")({
  validateSearch: (search: Record<string, unknown>): ProfileSearch =>
    search["via"] === "link" || search["via"] === "recovery-code" ? { via: search["via"] } : {},
  component: function Profile() {
    const { via } = Route.useSearch();
    return <ProfilePage via={via} />;
  },
});
