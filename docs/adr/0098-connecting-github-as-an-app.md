# 0098. Connecting GitHub as an App

- Status: Accepted
- Date: 2026-11-14

## Context

The PRD's Git row (P0) asks for "Git provider connect (GitHub App, GitLab, Gitea/Forgejo)" and push-to-deploy. #253's first slice deploys on push from any of five hosts through a hook a person adds by hand: copy a URL and a secret into the repository's settings ([ADR 0046](0046-builds-from-git.md), "Deploy on push"). It builds only public repositories, and it is one hook per app.

The mocks draw more:

- `Settings-Integrations.dc.html` has a Git section with a GitHub card: "GitHub app on acme · 23 repositories", and Manage.
- `Apps-New.dc.html` picks a repository, `github.com/acme/search-ui`, and a branch.
- `App-Overview.dc.html` says "Deploy on push" as a switch, with nothing to paste.

A GitHub App is how GitHub gives one installation access to chosen repositories, with tokens that last an hour and name what they may do, and one webhook for every repository it's installed on. GitHub's manifest flow makes an App from a description Dockwell posts, so nobody fills in GitHub's form by hand.

What's there to build on:

- `internal/seal` seals a credential under the install's key, bound to a purpose ([ADR 0022](0022-secrets-encrypted-at-rest.md)). Sign-in providers, DNS providers and registry logins keep theirs so ([ADR 0087](0087-private-routes.md), [ADR 0088](0088-certificates-by-dns-01-and-wildcard-domains.md), [ADR 0086](0086-registry-logins-for-private-images.md)).
- Settings → Integrations lists those, as cards with Connect and Manage.
- The hook receiver of slice 1: buckets, a body limit, delivery IDs kept 7 days, a constant-time check, and a deploy with its maker's rights, step by step.
- `BuildService.ListRepositoryBranches`, which the new app sheet uses as its branch picker.

## Decision

### Connecting: the manifest flow, in one click

- **Settings → Integrations → Git → GitHub → Connect** opens a sheet with one choice: your account, or an organization by name. Repositories an organization owns need an App the organization owns, as an App that isn't public installs only where it was made. Then one button, **Create on GitHub**.
- **The button posts a manifest** to `github.com/settings/apps/new`, or `github.com/organizations/<org>/settings/apps/new`, with a `state` of 32 random bytes. The manifest names:
  - the App, "Dockwell" and the panel's host, cut to GitHub's 34 characters, which GitHub lets you change on its page;
  - its webhook, `<panel>/hooks/github/<connection ID>`, active, with the events `push`, `installation` and `installation_repositories`;
  - `redirect_url` and `setup_url`, both on the panel;
  - the least it may do: `contents: read` and `metadata: read`. Later slices ask for more as they need it, as commit statuses, and GitHub asks each installation's owner to accept them.
  - `public: false`.
- **GitHub sends the browser back** to `redirect_url` with a `code`. The control plane checks the `state`: it was made for this signed-in person's session, in the last 10 minutes, and is used once. It then exchanges the code once, `POST /app-manifests/<code>/conversions`, which GitHub allows for an hour. The answer holds the App's ID, slug, client ID, client secret, webhook secret and private key.
- **Then it sends the browser to install the App,** `github.com/apps/<slug>/installations/new`, where you pick the repositories. GitHub sends the browser back to `setup_url` with an installation's ID. That ID is a claim anyone could make in a URL, so the control plane asks GitHub, as the App, whether the installation is the App's, before it records it. An installation made later, on GitHub, arrives by the `installation` event.
- **Who may:** an admin or the owner, as for any integration, with a passkey (step-up 102). The connection, and each installation it gains or loses, is in the audit log.

### What's kept, and where

- **Sealed under the install's key, each bound to the connection's ID as its purpose:** the private key and the webhook secret. Nothing else of the App's is secret to Dockwell.
- **Not kept:** the client secret. It's for signing people in through GitHub, which this doesn't do. A later slice that signs people in would ask for one then.
- **Kept plainly:** the App's ID, slug, owner, and client ID, which are public, and each installation's ID, account and repositories.
- **Only the control plane opens the key.** It signs a JWT with it, which lasts 9 minutes, GitHub's limit being 10, and is never written down. No agent, plan, change, event, log or error holds the key, the webhook secret, a JWT or a token. An error from GitHub is said by its status and message, never by a request's headers.

### Installation tokens: an hour at most, and as narrow as each use

- **The control plane asks GitHub for a token when it needs one,** `POST /app/installations/<id>/access_tokens`, signed with a JWT. GitHub's tokens last an hour. Dockwell asks for each one narrowed:
  - to the one repository it's for, by `repository_ids`;
  - to the permissions that use needs: `contents: read` to read a branch or fetch a commit, `metadata: read` to list repositories.
- **The control plane keeps a token in memory only,** for the repository and permissions it was asked for, until 10 minutes before it ends, and asks again after. A restart starts with none.
- **A build of a private repository takes one token,** for that repository and `contents: read`. The control plane sends it to the build's agent with the build, over the agent's connection, in a field of its own (agent protocol 360). The agent hands it to the builder's fetch as an `Authorization` header in Git's environment, from a file in the build's memory-backed secrets folder: never in the URL, the command line, the build's log or BuildKit's cache. It's gone with the build. A build that runs past the hour has fetched its commit long before.
- **Which projects an installation reaches:** every project by default, as a registry login does ([ADR 0086](0086-registry-logins-for-private-images.md)).
  - Manage shows which projects use it, and narrows them, as a plan, with a passkey.
  - A developer picks among the repositories their project's installations reach.
  - A plan that builds through an installation names it: "Builds acme/web at 8be41d2 (main), through the GitHub App on acme". A plan of a build from a repository no installation of the project's reaches is refused, saying which installations there are and that the project isn't among theirs.

### Pushes through the App

- **One webhook for the App,** `POST /hooks/github/<connection ID>`, held to slice 1's receiver: the same buckets and limits, `X-Hub-Signature-256` under the App's webhook secret in constant time, `X-GitHub-Delivery` kept 7 days, and the same answer for a connection there isn't.
- **A push names a repository.** Each app that builds from it, through one of the App's installations, with Deploy on push on, deploys as slice 1 does: the branch read from Git, never the payload, one deploy at a time for each app, with the rights of whoever turned it on, step by step.
- **Deploy on push for such an app is a switch and nothing more:** no URL, no secret, no sheet. Its line says so, "Pushes come through the GitHub App", or, as below, that GitHub can't reach the panel. An app whose repository isn't on GitHub, or that no installation reaches, keeps slice 1's hook.

### When GitHub can't reach the panel

- **The panel knows, rather than guesses.** After connecting, and every 10 minutes while it's connected, the control plane reads the App's recent deliveries as the App (`GET /app/hook/deliveries`): GitHub says whether each reached the panel. The ping GitHub sends as the App is made is the first.
- **When they don't arrive, Dockwell polls,** and says so. The GitHub card, and each app's line, read "GitHub can't reach this panel, so Dockwell checks for pushes every minute", with **Give the panel a domain**.
  - Each app with Deploy on push on is checked once a minute: `GET /repos/<owner>/<repo>/commits/<branch>` asking for the SHA alone, with the ETag of the last answer, which GitHub answers with 304 when nothing moved, and doesn't count against the installation's limit.
  - A commit that moved is a push, with no delivery ID, and goes through the same queue.
  - Polling stops once a delivery arrives.
- **Polling has a cap for each installation, and backs off when GitHub asks.**
  - At most 30 checks a minute for one installation, a third of the least GitHub gives one (5,000 an hour), so builds and listing repositories keep the rest. Past 30 apps, the apps take turns, and each line says how often its app is checked: "checks for pushes every 2 minutes".
  - Every answer's `x-ratelimit-remaining` and `x-ratelimit-reset` are read. Under a tenth of the hour's limit left, checks wait for the reset. A secondary limit, a 403 or 429 with `retry-after`, waits that long, doubling each time it comes again, to 15 minutes at most, and starting over once one passes.
  - While it backs off, the card and each app's line say so, with when it ends: "GitHub asked Dockwell to slow down, so pushes aren't checked until 9:40 PM", with **Give the panel a domain**, as a webhook needs none of this. A push in that time deploys at the next check, once.
- **Refusing to connect** would leave a panel behind a tunnel or on a private network with no way to deploy on push. Polling costs a minute's delay, which the line says.
- **A panel that gets a domain** sets the App's webhook to it, `PATCH /app/hook/config`, as the App, and says so in the audit log.

### Uninstalling, suspending and removing

- **An installation removed on GitHub** arrives as `installation` with `deleted`; suspended, as `suspend`; repositories taken away, as `installation_repositories`. Dockwell records it at once, and each app it reached says, in its Source line, "The GitHub App can't reach acme/web any more", with **Add it on GitHub**, a link to the installation's settings. Its builds fail at planning with the same words. Deploy on push stays on, and works again once the repository is back.
- **An App deleted on GitHub** answers its JWT with 401. The card says "The GitHub App was deleted on GitHub", with **Connect again**, and every app it reached says so too.
- **Removing it in Dockwell,** Manage → Remove, with a passkey (step-up 104): Dockwell uninstalls each installation as the App (`DELETE /app/installations/<id>`), then forgets the key and the secret. GitHub has no way for an App to delete itself, so the card's last line is **Delete it on GitHub**, a link to the App's advanced settings. Apps that built through it say they can't reach their repository, as above.

### Rotating

- **The webhook secret,** Manage → New webhook secret, with a passkey (step-up 103): Dockwell makes one, sets it on GitHub as the App (`PATCH /app/hook/config`), and keeps the old one for 10 minutes, for deliveries already sent. Nothing to paste.
- **The private key,** which GitHub makes only on its own page and lets no App make by its API: Manage → New private key, with a passkey (step-up 103).
  - It links to the App's page on GitHub, where Generate a private key downloads one.
  - You drop the file on the sheet. Dockwell checks it at once, signing a JWT with it and asking GitHub for the App, and seals it only if it's this App's key.
  - The sheet then says which key to delete on GitHub, by the SHA-256 fingerprint GitHub shows beside each, with the link. A key left there still works, so the card says "2 keys on GitHub" until GitHub says one.
- **Installation tokens** aren't rotated: each ends within the hour.

### Tokens for CI

The fourth slice deploys from CI: a workflow that deploys a commit or an image. Any API token Dockwell makes for it is narrowed as [ADR 0071](0071-api-tokens-narrowed-to-projects-and-a-role.md) narrows a token, with the narrow end as the default:

- **Its projects:** the one project whose app the CI deploys, not every project its maker holds.
- **Its role:** capped at developer. So in a protected environment its change plans and waits for an admin or the owner to approve it, as a developer's does (ADR 0049). Its plan, and the line where the CI is set up, say so: "Deploys to production wait for an admin's yes".
- **The person making it can widen it, one step away:** under Advanced, more of the projects they hold, or no cap, which is their own role. An admin who lifts the cap lets CI apply in that project's protected environments, as ADR 0071 allows an uncapped token.
- **A token's reach is fixed for its life,** as ADR 0071 has it. Widening or narrowing it later makes a new token in its place, and Dockwell revokes the old one once the new one is where CI reads it.
- **It's made by a person, signed in, with a passkey,** as any token is (ADR 0016), and the audit log says it's for CI and for which app.

### Numbers

- **Step-ups:** 102 connecting a GitHub App, 103 a new webhook secret or private key, 104 removing it.
- **Agent protocol:** 360 to 369. A build's Git token is 360.
- **The build's source:** `ImageBuild.git_login = 101`, naming an installation, or slice 3's logins for other hosts.
- **Migration:** the next after main's when it lands.
- **The API:** a `GitProviderService` of its own, its messages numbered from 1.

## Alternatives considered

- **A GitHub App that Dockwell's project publishes,** one App every install shares, as hosted platforms have: one click less, but its private key would sit with whoever publishes it, and every install's tokens would come from one key. A self-hosted control plane keeps its own.
- **An OAuth App, or a personal access token:** tokens as wide as the person, that don't end, and no webhook of their own. Slice 3's logins take a token for hosts without Apps, narrowed as each host allows.
- **Refusing to connect a panel GitHub can't reach:** see above. Polling keeps a tunnel's panel deploying.
- **Keeping installation tokens in the store,** to survive a restart: a token on disk for an hour, for a saving of one call after a restart.
- **Sending agents the private key, or a JWT,** to ask for their own tokens: any agent could then act as the App on every installation.

## Consequences

- Settings → Integrations gains its Git section, with GitHub first. GitLab and Gitea or Forgejo come with slice 3, as logins.
- The new app sheet offers the installation's repositories as a picker, with a search, then the branch picker, and a private repository builds.
- The fourth slice, CI, builds on ADR 0071's narrowed tokens, which Track V is building, and lands after them.
- An app that builds through the App needs no hook of its own. Slice 1's hooks stay for every other host and for public repositories without an App.
- The control plane calls GitHub's API on its own: the deliveries check every 10 minutes, and, only while GitHub can't reach the panel, a check a minute for each app with Deploy on push on, which costs nothing of the installation's limit while nothing moved.

## As built: slice 2

What slice 2 built, where it differs from the decision above, or settles what the decision left open.

- **The kind is `github.app`,** with IDs `gha_…`, at the organization level as every integration is. Its spec holds the App's ID, slug, owner, client ID, the private key and webhook secret (sensitive, sealed), the previous webhook secret and when it stops, the key's fingerprint, and `reaches`, each installation's projects. An installation with no entry reaches every project. `GitHubInstallationReach` and the Spec's `github_app` are 130 in specs.proto.
- **The webhook's ID comes before the App.** GitHub's manifest names the webhook's URL before the App exists, and GitHub's API has no ID to name it by after, so Start makes a `ghw_…` ID, the manifest's URL names it, and the spec keeps it as `hook_id`. `POST /hooks/github/<hook ID>` finds the App by it.
- **The manifest subscribes to `push` alone.** GitHub sends every App its `installation` and `installation_repositories` events without a subscription.
- **An App-backed hook is narrowed as ADR 0071 narrows slice 1's:** to its app's project, as a developer at most, so a push into a protected environment plans its deploy and waits for a person, until its maker, an admin there, turns on Deploy without waiting with a passkey (step-up 109). The switch sits under Deploy on push, as such a hook has no sheet. A push queued before its hook narrows leaves its plan for a person, as slice 1's does.
- **Installations are facts,** in the store's `github_installations` table (ID, App, account, its repositories as GitHub lists them, suspended, removed), with the migration that also adds `git_hooks.via`. An App-backed hook is a slice 1 hook with `via = github` and no secret of its own; its URL is the App's webhook.
- **A build's login is `<App name>/<installation ID>`,** in `ImageBuild.git_login` (101). A plan pins it where an installation of the project reaches and lists the repository and the edit names none, and refuses one that doesn't reach the project, naming the project's installations. The binding's label keeps it, so the read-back check compares it.
- **Agent protocol, in the 360 block:** `DesiredResource.build_git_auth` (360) carries a build's Authorization, sealed for `resource.BuildGitRef`; `Hello.build_git_auth` (361) says the agent hands it on, for builds and readings both; `DetectBuildCall.git_auth` (362) carries a reading's, sealed for `resource.DetectGitRef`. The agent writes it to the build's in-memory secrets folder, mounted read-only at `/run/dockwell-git`, and Git reads it through `GIT_CONFIG_*` as `http.<url>.extraHeader`. A step to an agent that doesn't say 361 fails, saying to update it.
- **The new app sheet reads as the plan will.** `BuildService.ListRepositoryBranches` takes the project (`project`, 2) and says which installation it read through (`git_login`, 4); `DetectBuild` uses the project or environment it names, or the service's. Both pin the login as a plan does, and refuse a login named for no project. The scope check keeps a developer to their own projects.
- **Polling, as built:** the deliveries check every 10 minutes, as the App, from `GET /app/hook/deliveries`; while the newest didn't reach the panel, each App-backed hook's branch is checked once a minute with its ETag, up to 30 hooks a minute for each installation, past which they take turns and the line says how often each is checked. A secondary limit waits as long as `Retry-After` asks, or twice the last wait from a minute, to 15 minutes at most; fewer than a tenth of the hour's calls left waits for the hour to start over. A branch that moved past what the app runs deploys once, as delivery `poll-<commit>`. An installation narrowed away from a hook's project isn't asked about that hook. A delivery that reaches the panel ends polling at once. Only a delivery the panel answered with a 2xx reached it. One it refused with a 401 means GitHub signs with a secret Dockwell doesn't keep: `GitHubDelivery.secret_refused` (5) says so, the card offers **New webhook secret** beside it, and Dockwell polls meanwhile. Any other answer, as a proxy's 502, delivered nothing either, and starts polling.
- **A new webhook secret is Dockwell's before it's GitHub's.** Dockwell keeps it first, as a change, with the old one counting 10 minutes, then sets it on GitHub, so nothing GitHub sends between is refused. If GitHub doesn't take it, the old secret is Dockwell's again, with the new one counting as long, as a call that failed may have reached GitHub. An App's secret and key change one at a time, each from the App as it is then.
- **A change that acts on GitHub offers no undo:** connecting, a new webhook secret, a new key and removal. An undo would put Dockwell back while GitHub keeps what it did, so every delivery or JWT would be refused. Its plan keeps why (`ResourcePlan.NoUndo`, from the kind's `ActsElsewhere`), the change has no undo deadline, and `UndoChange` refuses with what to do instead. Which projects an installation reaches is Dockwell's alone, and undoes.
- **Every change to an App names the revision it read** (`Edit.base_revision`), so one made from a read another change has moved past is stale, never a silent undo of a narrowing. Manage's reach editor names the App's revision, which `GitHubAppInfo.revision` (12) gives an admin. A new secret or key starts from the App as it is under the lock that keeps those one at a time, and reads it once more if a change landed in between.
- **The API's view is `GitHubAppInfo`,** as registry logins and DNS providers have theirs. Below an admin, its answer holds the App's name, owner, page, delivery line and each installation's account, state and settings link: not the key's fingerprint, the App's settings, or which projects an installation reaches.
- **The panel posts to GitHub.** Its policy keeps forms to its own origin, so `form-action` names GitHub's web origin too, and nothing else widens. GitHub sends the browser back to `/settings/integrations/github/created` and `/settings/integrations/github/<App ID>/installed`.
- **A test reaches no GitHub:** a test's control plane talks only to the stand-in `DOCKWELL_TEST_GITHUB` names, its API and its pages at one address, or to `github.invalid`.

Not built yet:

- Setting the App's webhook to a panel's new domain, `PATCH /app/hook/config`, as the panel gets one. Until then, deliveries go to the old address, the deliveries check sees they don't arrive, and polling covers the gap.
- "2 keys on GitHub" on the card: GitHub has no API to list an App's keys, so the sheet says which to delete, once.
- Refused deliveries to the App's webhook aren't in the audit log; slice 1's are.
