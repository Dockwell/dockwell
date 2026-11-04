// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { RecoverPage } from "../features/auth/RecoverPage";

export const Route = createFileRoute("/recover")({
  staticData: { public: true },
  component: RecoverPage,
});
