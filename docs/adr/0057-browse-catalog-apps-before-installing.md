# 0057. Browse catalog apps before installing

- Status: Accepted
- Date: 2026-11-09

## Context

The catalog took a person straight from a short card into an installation form. It did not provide enough information to decide whether an app was useful, or show what it looked like. Installed entries also skipped their catalog information and opened a running service.

## Decision

Every card opens `/catalog/<id>`, a read-only app page with its own logo, description, features, genuine screenshots, project links, version and licence. It names the included services and existing installations. Its explicit Install action opens `/catalog/<id>/install`; closing the form returns to the app. Search and category filters survive the round trip. The existing revision, plan and apply flow stays in charge of installation.

The optional `description`, `icon`, `screenshots`, `documentation`, `app_source` and `features` fields belong to the public entry schema. The converter carries authored values through overlays. Old and custom entries remain valid without them, using their summary and a monogram when needed. Descriptions and captions are plain text.

Media ships with the panel under `/catalog-media/`. Entry validation restricts media to that path, screenshots to raster formats, and project and source links to HTTPS without credentials. Browsing makes no external image requests and needs no change to the panel's content security policy. A missing image has a usable fallback. A custom entry may refer to bundled media; supplying new artwork requires a panel build.

All 24 built-in entries have app information and a project logo. Apps with an interface have previews from their projects. A database preview names the included administration tool, and Vaultwarden's preview names the compatible Bitwarden browser extension. Redis and Cloudflare Tunnel explain that they have no local web interface. Screenshots do not promise that a project's current demo exactly matches the pinned release.

The [media manifest](../../web/public/catalog-media/manifest.json) records original URLs, commit IDs where available, hashes and technical transformations. The [notices](../../web/public/catalog-media/NOTICE.txt) retain attribution and source terms. Artwork is not relicensed as Dockwell code, and no preview is fabricated. Catalog descriptions and feature lists are authored here, as [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md) requires.

## Consequences

Choosing an app comes before installation settings. Existing installations remain reachable from the app's page, and another install is an explicit action.

Bundled WebP images add about 1 MB before notices to the panel. They keep browsing private and available offline, at the cost of binary size and the pages read when the panel starts. Measure the final panel size and the unchanged idle memory budgets with this change.

Entry releases increase when presentation changes, but their Compose files do not. The existing upgrade comparison therefore offers no plan for this metadata update. Catalog metadata remains in the entry's language; panel labels are translated as before.
