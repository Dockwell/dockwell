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
| Runtipi's app store, runtipi/runtipi-appstore | GPL-3.0 | An adapter (below): `config.json` with its form, a Compose file with versioned images and Runtipi's variables, its `metadata/logo.jpg` and `metadata/description.md` | Listed, with the third pull request |
| CapRover's one-click apps | Apache-2.0 | An adapter for `captainVersion: 4` files and `$$cap_` variables; logos in the repository | After Runtipi |
| CasaOS's app store, IceWhaleTech/CasaOS-AppStore | Apache-2.0 | An adapter for `x-casaos` (below); its logos and screenshots are links through jsDelivr to files in its own repository, which Dockwell reads from the commit | Listed, the fifth |
| Umbrel's apps, getumbrel/umbrel-apps | None stated | An adapter (below); `app_proxy`, and hooks and `exports.sh` scripts, which leave their apps out; its logos and screenshots live in another repository | Read from a pasted link, never in the gallery: a repository with no licence grants no rights to use its files, so Dockwell doesn't promote it |
| Portainer's templates | None stated | Its stacks name files in other repositories | Not listed |

### Runtipi's app store, the fourth format

- **Laid out as `apps/<id>/config.json`** beside its `docker-compose.yml`, which detection reads as Runtipi's (`CATALOG_FORMAT_RUNTIPI`). Its newer `docker-compose.json` says the same as the YAML file, which is read.
- **What Runtipi fills in is said in an entry's own terms:** `${APP_DATA_DIR}/data/<name>` and `${APP_DATA_DIR}/<name>` become the app's folders, which the rewrite makes named volumes; `${APP_PORT}` is the port input, its default `config.json`'s port; `${APP_DOMAIN}` is `localhost:${APP_PORT}` and `${APP_PROTOCOL}` `http`, as for Coolify's address; `${TZ}` reads UTC unless set; `${ROOT_FOLDER_HOST}/media` is `MEDIA_DIR`, a folder a person names, default `/srv/media`, which the plan shows as a path on the server for them to approve. Its network `tipi_main_network` and its `runtipi.` labels go, with Traefik's.
- **Its form is the entry's inputs:** `random` a generated secret of 32 to 64 characters, `password` a secret a person types, `boolean` a choice of true or false, options a choice, the rest text.
- **Left out, saying why:** an app Runtipi marks unavailable or deprecated, one on amd64 alone, one whose data folder Runtipi gives to another user by running chown on the server, one it makes keys for at install, and one that uses any other value of Runtipi's.
- **Bounds:** ADR 0078's, as for every store: each file read on the server within its size and nesting, its aliases expanded within their bounds, 2,000 apps, and the time checked once an app. Its logos are JPEG, read as the others' are.
- **At `e88c4a72`, 141 of its 267 apps convert,** the most of the four by share: 31 use a value only Runtipi fills in, 28 ask for what Dockwell can't run yet, 21 name a moving tag, 16 run on amd64 alone, 10 have their folder given to another user.

### CasaOS's app store, the fifth format

- **Laid out as `Apps/<name>/docker-compose.yml`,** whose `x-casaos` says the rest: its title, tagline and description by language (`en_US`, and `zh_CN` as the panel's zh-CN), the description, which CasaOS writes in Markdown, kept as the entry's README; category, architectures, the service a person opens and the port it's published on, version and links.
- **What CasaOS fills in is said in an entry's own terms:** `/DATA/AppData/$AppID/<folder>` is the app's own folder, which becomes a named volume, where `$AppID` or the folder names the app itself; `/DATA/Media` and `/DATA/Downloads` are `MEDIA_DIR` and `DOWNLOADS_DIR`, folders a person names, which the plan shows as paths on the server to approve; `$TZ`, `$PUID` and `$PGID` read UTC, 1000 and 1000 unless set; the published `port_map` is the port input. `x-casaos`, the project's `name` and `network_mode: bridge`, Docker's default, go.
- **Left out, saying why:** an app on amd64 alone, one that binds another app's data folder, one that binds CasaOS's own `/DATA` beyond those, and what the rewrite leaves out for every store.
- **Its logo and screenshots** are written as addresses, `https://cdn.jsdelivr.net/gh/<owner>/<repo>@<ref>/Apps/<name>/<file>` or GitHub's raw files of the same. Dockwell takes the path from such an address, reads the file at that path in the commit it fetched, only inside the app's own folder, and never fetches the address. Any other address is left out.
- **At `07178f6d`, 95 of its 183 apps convert:** 27 run on amd64 alone, 25 ask for what Dockwell can't run yet, 12 name a moving tag.

### Umbrel's app store, readable but not listed

- **Laid out as `<id>/umbrel-app.yml`** beside its Compose file, at the repository's top. Its proxy, `app_proxy`, goes, and the service and port it reached (`APP_HOST`, `APP_PORT`) are published on a port input; `${APP_DATA_DIR}` becomes named volumes and `${UMBREL_ROOT}/data/storage/downloads` `DOWNLOADS_DIR`; `${APP_SEED}` and `${APP_PASSWORD}`, which Umbrel makes for each app, are generated secrets of 64 hex and 32 characters; `${DEVICE_DOMAIN_NAME}` is `localhost`; a container its services reach by Umbrel's name for it, as `planka_db_1`, they reach by its service's name. Its images are pinned by digest already.
- **Left out:** an app with hooks or an `exports.sh`, which are scripts Umbrel runs on the server, one that needs another app installed, and one that uses any other value of Umbrel's. At `1d9cd0b7`, 155 of its 403 apps convert; 147 have scripts.
- **Never in the gallery (the owner's decision, 13 Nov 2026):** its repository has no licence file and its README states none, and a repository with no licence grants no rights to use its files, so Dockwell doesn't promote it. It stays readable from a link a person pastes, as any public repository laid out as a store's is, and says its state as it is: the source shows its licence as "None stated" (`CatalogSourceInfo.license` is `NONE`), its plan says its repository states no licence, and each entry's note says it's from a repository that states no licence. Its logos and screenshots are in another repository, getumbrel/umbrel-apps-gallery, which a source of the store doesn't read.
- **Any store a repository states no licence for reads the same way:** "None stated" on its source and in the gallery, never an identifier. A repository with a licence file Dockwell doesn't recognise is another state, `NOASSERTION`, which reads "Not recognised", and its plan and notes say the licence is one Dockwell doesn't recognise.

### A store's logo is read for each app that converts

- **The adapter says where the store keeps each app's logo,** by the store's own convention, as a file of the store's folder: 1Panel's `logo.png` beside `data.yml`, the file Dokploy's `meta.json` names beside it, Coolify's `public/svgs/<name>` its header names. A name that leaves its folder, or isn't a WebP, PNG, JPEG or SVG file's, or names nothing there, gives no logo.
- **The logos are read in a third round,** after the app converts, only for those that do, as Snapshot.Read reads the adapter's files: within ADR 0077's limits for a catalog's images, 1,000 at most, 1 MiB each, 32 MiB together, counted apart from the adapter's files. A logo over its limit, or one that isn't what its name says, leaves its app without one; a round that fails leaves every app without one. An app's place never hangs on its logo.
- **Each is kept as `media/<id>.<ext>`** beside the converted entries, through `folder.Prepare`, and named in the entry's icon, so the control plane serves it as any source's image, from the commit kept for a source applied. A second app naming the same file keeps a copy of its own.
- **A raster logo is 1024 pixels a side at most,** as its header says, as the panel draws one a few dozen pixels wide; a larger one leaves its app without one. Every raster image a catalog keeps, Dockwell's own included, is 4096 a side and 16 MP at most (`folder.Image`), as a browser decodes an image whole and a file of a few hundred KiB can name 30000 by 30000 pixels.

### SVG, for icons alone, written anew

- **An icon may be an SVG image; a screenshot never is,** as a screenshot is a picture of the app. This holds for every source, Dockwell's own format too.
- **The control plane keeps and serves an SVG image only as `internal/catalog/svgclean` writes it anew:** a new document from an allowlist of elements that draw (shapes, text, gradients, clipping, masks and the filter primitives that draw) and of their presentation attributes, with every value checked, the first of each name kept. Scripts, event handlers, links, animation, foreign content, fonts and editors' markup are left out with what they hold, and so are markers, drawn at every point of a path, and patterns, drawn in every tile they fill, which multiply in ways no count of elements bounds. An `href` names an element of the image, or for `<image>`, a PNG, JPEG or WebP image written into the file whose bytes say so. A `url()` names an element of the image, and in a style sheet, a gradient alone, as what a sheet's selectors reach can't be counted. A style sheet with an at-rule other than `@media`, an escape, or a function that loads a file refuses the image, as do declared entities, an encoding other than UTF-8, over 10,000 elements, 64 levels, or 4,000 elements drawn once each reference stands for what it names, each time it's named: a `<use>`, and a mask, clip path, filter or paint named by `url(#id)`. Cleaning what it wrote gives the same bytes; a fuzz test holds it to that, to the allowlist, and to one of each attribute.
- **Why 4,000:** the security review of #574 measured masks of masks, clip paths of clip paths and markers of markers, a few KiB each, blocking Chromium's main thread for seconds or crashing its renderer when only `<use>` was counted. The most any store logo draws, counted so, is 1,110; the heaviest document 4,000 allows, masks or clip paths four levels deep, draws twelve times at 48 px in under 40 ms.
- **It's served so a browser draws it as an image alone,** with `image/svg+xml`, `X-Content-Type-Options: nosniff`, `Content-Disposition: attachment`, `Cross-Origin-Resource-Policy: same-origin`, and `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox`. A browser that draws an SVG as an `<img>` runs no script in it and loads nothing it names. Opened on its own it's a download, and a browser that showed it anyway would make it a sandboxed page that runs nothing. The image is cleaned again as it's served, so a file kept by an older Dockwell, or the snapshot's, is served the same way.
- **538 of the two stores' 546 SVG logos under 1 MiB pass.** Of the eight that don't, five have style sheets that name a clip path by `url()`, two have style sheets with other at-rules, and one declares a Windows encoding; their apps show a monogram.
- **The media route cleans each image once,** keyed by what its file holds, and serves it from there, so a page of logos costs one cleaning each.

### A store's words, in the panel's languages

- **An entry gains `readme`, Markdown,** and **`translations`,** what it says in another language, by its tag: a summary, a description and a README. Only the languages the panel speaks are kept beside English, zh-CN today; `convert.Languages` gains a language as the panel does.
- **1Panel's** English README is `README_en.md`, and its Chinese one `README.md`, where Han characters are a tenth of its letters or more; with no `README_en.md`, a `README.md` without Chinese is the English one. Its Chinese line is `shortDescZh`, or `description.zh`. **Dokploy's** `meta.json` description becomes the entry's description, as plain text. **Coolify's** slogan stays its summary.
- **A README is kept without what the panel would never show:** each block of HTML, to the next blank line, each tag inside a line and each image, outside code spans and fenced code, which keep what they hold. Control characters go, and it's cut where a line ends within 32 KiB. The longest of 1Panel's was 6.8 KiB.
- **The panel shows a README as a subset of Markdown, as text:** headings, paragraphs, lists, emphasis, code and links, made into elements, never into HTML. A link opens only as an HTTPS address, in a new tab, with no referrer. It's shown under the store's name, as its words, as a summary is (ADR 0078).
- **The catalog's list leaves each README out,** and each translation's description and README, which the app's page reads, as 1Panel's READMEs alone come to over a megabyte.
- **`catalog.Check`** holds every source to the same: a README of 32 KiB at most, UTF-8; a summary of 240 characters at most, on one line, in any language; at most 8 languages, each a tag such as `zh-CN`.

### Screenshots

- **A store's screenshots are read only where the store keeps them in its own repository,** as its logos are, and never from another host (ADR 0077). Of the six stores, CasaOS does: its `screenshot_link` names files beside each app's Compose file. 1Panel's two READMEs that show screenshots link them from other hosts; Runtipi keeps none in its repository, and Umbrel keeps them in another.
- **They're read after the logos, in what's left of a commit's image limits** (1,000 images, 1 MiB each, 32 MiB together): three an app at most, each app's first before any app's second, so a store of many apps gives each a picture before the budget ends. A commit's objects come with no sizes, so they're asked for 16 at a time, in 12 rounds at most. The logo round and the screenshot rounds share one budget of what their packs take from the host, 64 MiB, a snapshot's bound for one pack, each round's pack held to what's left, so a store of large screenshots costs no more than one fetch, hourly or not. The rounds stop after one that keeps nothing, or whose files are mostly over 1 MiB. They come whether or not the store keeps logos. CasaOS's screenshots come to 210 MiB, so most of its apps show one or two.
- **A screenshot is a raster image,** never an SVG one, kept as `media/<id>_shot-<n>.<ext>`, a name no logo has, as an ID has no underscore, with "<app>, screen <n>" as its caption, where the store gives no words. Its source, where a person sees the original, is the repository's own page of the file at the commit read, on GitHub, GitLab or Codeberg, or the repository's page elsewhere; never the address the store gives, which could name another repository.

### A folder a person names is no way out

- **A store's folder that becomes one a person names,** such as CasaOS's `/DATA/Media` as `MEDIA_DIR`, matches only the folder itself or one in it, ending at a slash, the colon before a volume's target, or the value's end, so `/DATA/Media-x` is no folder of theirs and leaves the app out as any other of `/DATA` does.
- **A path that climbs out with `..`,** after a store's folder or a person's, leaves the app out: in each adapter, and in the rewrite every store's file goes through, which refuses `..` after a `$` source. A path to Docker's socket reads as control of Docker however it's written, as `ControlsDocker` cleans it first.

### A store's apps are checked once for every reader

Reads of the same commit and the IDs before it share one check of its entries, as they share its fetch. The check is every reader's, so a person who leaves the page doesn't cut it short, within three minutes of its own. A check cut short, or one where any entry's Compose file wasn't read as the reader was busy, is kept for nobody, and the next read checks again: a store never reads as "too many Compose files are waiting" for longer than one look.

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
