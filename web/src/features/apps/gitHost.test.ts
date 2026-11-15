// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import { gitHost, hookOutOfReach } from "./gitHost";

describe("gitHost", () => {
  it("names each host and the page where its hooks are added", () => {
    expect(gitHost("https://github.com/acme/web.git")).toEqual({
      kind: "github",
      name: "GitHub",
      path: "acme/web",
      hooksPage: "https://github.com/acme/web/settings/hooks/new",
    });
    expect(gitHost("git@gitlab.com:acme/tools/web.git")?.hooksPage).toBe("https://gitlab.com/acme/tools/web/-/hooks");
    expect(gitHost("https://gitlab.example.com/acme/web")?.kind).toBe("gitlab");
    expect(gitHost("https://bitbucket.org/acme/web")?.hooksPage).toBe("https://bitbucket.org/acme/web/admin/webhooks");
    expect(gitHost("https://codeberg.org/acme/web.git")).toMatchObject({ kind: "gitea", name: "Codeberg" });
    expect(gitHost("github.com/acme/web")?.path).toBe("acme/web");
  });

  it("takes a host it can't tell for a Gitea or Forgejo, served as the repository is", () => {
    expect(gitHost("http://192.0.2.10:3000/acme/web.git")).toEqual({
      kind: "gitea",
      name: "",
      path: "acme/web",
      hooksPage: "http://192.0.2.10:3000/acme/web/settings/hooks",
    });
  });

  it("never carries a login the URL had into the page it links", () => {
    const host = gitHost("https://leo:s3cret@github.com/acme/web.git");
    expect(host?.hooksPage).toBe("https://github.com/acme/web/settings/hooks/new");
    expect(JSON.stringify(host)).not.toContain("s3cret");
  });

  it("reads nothing from what isn't a repository's URL", () => {
    expect(gitHost("")).toBeUndefined();
    expect(gitHost("https://github.com/")).toBeUndefined();
  });
});

describe("hookOutOfReach", () => {
  it("is a hook on this machine alone, for any host but one on this machine too", () => {
    expect(hookOutOfReach("http://127.0.0.1:7600/hooks/git/ghk_1", "https://github.com/acme/web")).toBe(true);
    expect(hookOutOfReach("http://localhost:7600/hooks/git/ghk_1", "http://192.0.2.10:3000/acme/web")).toBe(true);
    expect(hookOutOfReach("http://[::1]:7600/hooks/git/ghk_1", "https://github.com/acme/web")).toBe(true);
    expect(hookOutOfReach("http://127.0.0.1:7600/hooks/git/ghk_1", "http://127.0.0.1:3000/acme/web.git")).toBe(false);
  });

  it("is a private address when the host is on the internet, and not when the host is beside it", () => {
    expect(hookOutOfReach("http://192.168.1.20:7600/hooks/git/ghk_1", "https://github.com/acme/web")).toBe(true);
    expect(hookOutOfReach("http://dockwell.lan:7600/hooks/git/ghk_1", "https://gitlab.com/acme/web")).toBe(true);
    expect(hookOutOfReach("http://192.168.1.20:7600/hooks/git/ghk_1", "http://192.168.1.30:3000/acme/web")).toBe(false);
    expect(hookOutOfReach("http://192.168.1.20:7600/hooks/git/ghk_1", "http://git.lan/acme/web")).toBe(false);
  });

  it("isn't a public address or a domain", () => {
    expect(hookOutOfReach("https://panel.example.com/hooks/git/ghk_1", "https://github.com/acme/web")).toBe(false);
    expect(hookOutOfReach("http://203.0.113.7:7600/hooks/git/ghk_1", "https://github.com/acme/web")).toBe(false);
  });
});
