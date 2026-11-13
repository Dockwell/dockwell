# 0083. A store's own logos and words, and stores one click away

- Status: Accepted. Amends [ADR 0077](0077-dockwells-catalog-in-its-own-repository.md) for SVG images, and [ADR 0078](0078-other-stores-read-on-the-server.md) for what a converted store brings.
- Date: 2026-11-12

## Context

On 2026-11-12 the owner added 1Panel's app store as a source and found three things wanting (#568): adding a known store was two small buttons, and a store's button vanished once it was added; the source's card listed every app as a chip, 84 of them; and the store's apps showed letter tiles, no description and no screenshots, where 1Panel shows a logo and a page for each.

ADR 0078 read a store's Compose files and forms, and left its artwork for later. ADR 0077 let a source hold images, WebP, PNG or JPEG alone, as an SVG can hold a script. We measured what the three stores keep on 12 Nov 2026, at 1Panel's `55061b99`, Dokploy's `4f0ddfb5` and Coolify's `93c7e53e`:

| Store | Logos | Words | Screenshots |
| --- | --- | --- | --- |
| 1Panel's app store | `apps/<key>/logo.png`, 269, 1.9 MiB, none over 1 MiB | `README_en.md` (199) and `README.md` in Chinese (269), 6.8 KiB at most; `data.yml` has a line in 11 languages | In two READMEs, as links to other hosts |
| Dokploy's templates | The file `meta.json` names beside it: 310 SVG, 285 PNG, a few WebP and JPEG, 16.9 MiB, two over 1 MiB | `meta.json`'s description, a few sentences | None |
| Coolify's templates | `# logo: svgs/<name>` in each template, in `public/svgs`: 236 SVG, 102 PNG, 8.6 MiB | `# slogan:`, one line | None |

Half of Dokploy's logos and two thirds of Coolify's are SVG. Seven of their 567 SVG files hold a `<script>` element, empty in each, left there by an editor.

## Decision

### The stores Dockwell reads are the server's to list

- **`ListCatalogStores` lists them,** for anyone who can read the catalog: each store's ID, name, repository, format, the branch a source of it follows, its licence, its site, and how many of its apps install. Until a source reads it, the counts are what this build's converter measured at a commit it names; once one does, they're that source's, as Dockwell read it, and the store names the source. The CLI has it as `dockwell catalog stores`.
- **A source reads one of these stores only at the store's own repository, in its format, at its top.** `CatalogSourceInfo.store` names it then, so the panel never compares links. A copy of the layout elsewhere is a source of its own, as ADR 0078 says of its words.
- **Adding one is one click, then the plan:** a source named by the store's ID, following its default branch, as Dockwell reads it. A store already added shows as added, with the way to its source, and never disappears from the list.
- **The list is code.** A store joins it with an adapter (ADR 0078), its bounds, and a row here.

### Which stores

| Store | Licence | What it would take | Decision |
| --- | --- | --- | --- |
| 1Panel's app store, Dokploy's templates, Coolify's templates | GPL-3.0, MIT, Apache-2.0 | Read today | Listed |
| Runtipi's app store, runtipi/runtipi-appstore | GPL-3.0 | An adapter: `config.json` with its form, a Compose file with versioned images and Runtipi's variables (`APP_DATA_DIR`, `APP_PORT`, `APP_DOMAIN`), Traefik labels to drop; `metadata/logo.jpg` and `metadata/description.md` in the repository | Next, with Track K's converter |
| CapRover's one-click apps | Apache-2.0 | An adapter for `captainVersion: 4` files and `$$cap_` variables; logos in the repository | After Runtipi |
| CasaOS's app store | Apache-2.0 | An adapter for `x-casaos`; its icons and screenshots are links to other hosts, which Dockwell doesn't fetch | Later, without artwork |
| Umbrel's apps | None stated | Its apps lean on `app_proxy` and `exports.sh` scripts | Not listed |
| Portainer's templates | None stated | Its stacks name files in other repositories | Not listed |

### A store's logo is read for each app that converts

- **The adapter says where the store keeps each app's logo,** by the store's own convention, as a file of the store's folder: 1Panel's `logo.png` beside `data.yml`, the file Dokploy's `meta.json` names beside it, Coolify's `public/svgs/<name>` its header names. A name that leaves its folder, or isn't a WebP, PNG, JPEG or SVG file's, or names nothing there, gives no logo.
- **The logos are read in a third round,** after the app converts, only for those that do, as Snapshot.Read reads the adapter's files: within ADR 0077's limits for a catalog's images, 1,000 at most, 1 MiB each, 32 MiB together, counted apart from the adapter's files. A logo over its limit, or one that isn't what its name says, leaves its app without one; a round that fails leaves every app without one. An app's place never hangs on its logo.
- **Each is kept as `media/<id>.<ext>`** beside the converted entries, through `folder.Prepare`, and named in the entry's icon, so the control plane serves it as any source's image, from the commit kept for a source applied. A second app naming the same file keeps a copy of its own.

### SVG, for icons alone, written anew

- **An icon may be an SVG image; a screenshot never is,** as a screenshot is a picture of the app. This holds for every source, Dockwell's own format too.
- **The control plane keeps and serves an SVG image only as `internal/catalog/svgclean` writes it anew:** a new document from an allowlist of elements that draw (shapes, text, gradients, patterns, clipping, masks and the filter primitives that draw) and of their presentation attributes, with every value checked. Scripts, event handlers, links, animation, foreign content, fonts and editors' markup are left out with what they hold. An `href` names an element of the image, or for `<image>`, a PNG, JPEG or WebP image written into the file whose bytes say so. A `url()` names an element of the image. A style sheet with an at-rule other than `@media`, an escape, or a function that loads a file refuses the image, as do declared entities, an encoding other than UTF-8, over 10,000 elements, 64 levels, or 20,000 elements drawn once each `<use>` stands for what it names. Cleaning what it wrote gives the same bytes; a fuzz test holds it to that, and to the allowlist.
- **It's served so a browser draws it as an image alone,** with `image/svg+xml`, `X-Content-Type-Options: nosniff`, `Content-Disposition: attachment`, `Cross-Origin-Resource-Policy: same-origin`, and `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox`. A browser that draws an SVG as an `<img>` runs no script in it and loads nothing it names. Opened on its own it's a download, and a browser that showed it anyway would make it a sandboxed page that runs nothing. The image is cleaned again as it's served, so a file kept by an older Dockwell, or the snapshot's, is served the same way.
- **543 of the two stores' 546 SVG logos under 1 MiB pass.** Of the three that don't, one declares a Windows encoding and two have style sheets with other at-rules; their apps show a monogram.

### A store's words, in the panel's languages

- **An entry gains `readme`, Markdown,** and **`translations`,** what it says in another language, by its tag: a summary, a description and a README. Only the languages the panel speaks are kept beside English, zh-CN today; `convert.Languages` gains a language as the panel does.
- **1Panel's** English README is `README_en.md`, and its Chinese one `README.md`, where Han characters are a tenth of its letters or more; with no `README_en.md`, a `README.md` without Chinese is the English one. Its Chinese line is `shortDescZh`, or `description.zh`. **Dokploy's** `meta.json` description becomes the entry's description, as plain text. **Coolify's** slogan stays its summary.
- **A README is kept without what the panel would never show:** each block of HTML, to the next blank line, each tag inside a line and each image, outside code spans and fenced code, which keep what they hold. Control characters go, and it's cut where a line ends within 32 KiB. The longest of 1Panel's was 6.8 KiB.
- **The panel shows a README as a subset of Markdown, as text:** headings, paragraphs, lists, emphasis, code and links, made into elements, never into HTML. A link opens only as an HTTPS address, in a new tab, with no referrer. It's shown under the store's name, as its words, as a summary is (ADR 0078).
- **The catalog's list leaves each README out,** and each translation's description and README, which the app's page reads, as 1Panel's READMEs alone come to over a megabyte.
- **`catalog.Check`** holds every source to the same: a README of 32 KiB at most, UTF-8; a summary of 240 characters at most, on one line, in any language; at most 8 languages, each a tag such as `zh-CN`.

### Screenshots

A store's screenshots are read only where the store keeps them in its repository, as its logos are. None of the three does: 1Panel's two READMEs that show screenshots link them from other hosts, which Dockwell doesn't fetch (ADR 0077). Their pages say so by showing none.

## What we turned down

- **Rasterizing SVG on the server.** No Go rasterizer draws the stores' logos faithfully: they use CSS classes, gradients, masks and text. One would be a second parser of the same file in the control plane, beside a rendering engine, where a browser drawing an image runs nothing and loads nothing already.
- **Serving SVG as the store wrote it,** under a strict policy alone. Seven logos carry script elements today; a policy is one line of defence, and a file kept as it came would be served by whatever serves it next.
- **Fetching screenshots and logos from the links a store writes.** Each refresh would have the control plane ask hosts a stranger names, and ADR 0077 keeps the catalog's images to its source's repository.
- **Turning a README into a description and features by its headings.** Stores write READMEs too differently for a rule to tell the overview from the rest, and a wrong guess would put the store's words under Dockwell's headings.
- **Keeping every language a store writes.** 1Panel writes eleven; the panel speaks two.
- **Counting a store's apps live before it's added.** It would fetch and convert each store whenever someone opens the gallery. The measured counts say when they were measured, and a source's own counts take over once it's added.

## Consequences

- A store's apps show their logos, the store's description, its README in English or Chinese, and their links, on the catalog's cards and the app's page.
- A catalog's list stays small; the app's page carries its words.
- Dokploy and Coolify logos come to about 9 MiB of a commit's 32 MiB of images, kept once per commit.
- The converter's version is 2, so every store's commit kept by version 1 is converted again, with its logos, at the next read.
- The measured counts go stale as stores and the converter change; they're measured again when the converter reads more of a store (#570).
- [The threat model](../security/catalog-sources.md) adds SVG images and a store's logos and words.
