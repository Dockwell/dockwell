// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { WebsiteCertificate } from "../../features/websites/WebsiteCertificate";

/** A website's certificate: where it stands on its server, and what to do when it's failing. */
export const Route = createFileRoute("/_shell/websites/$websiteId/certificate")({
  component: WebsiteCertificate,
});
