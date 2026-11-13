// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { CatalogFormat, CatalogSourceInfoSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { sourcesWords, sourceWords } from "./SourceFormats";

const source = (name: string, repository: string, format = CatalogFormat.UNSPECIFIED, path = "", store = "") =>
  create(CatalogSourceInfoSchema, { name, source: { repository, format, path }, store });

describe("A source as a person says it", () => {
  it("is the store's name where the control plane says it reads one, and owner/name anywhere else, never its ID", () => {
    expect(
      sourceWords(source("onepanel", "https://github.com/1Panel-dev/appstore", CatalogFormat.ONEPANEL, "", "onepanel")),
    ).toBe("1Panel");
    expect(
      sourceWords(source("dok", "https://github.com/Dokploy/templates.git/", CatalogFormat.DOKPLOY, "", "dokploy")),
    ).toBe("Dokploy");
    expect(
      sourceWords(source("cool", "https://github.com/coollabsio/coolify", CatalogFormat.COOLIFY, "", "coolify")),
    ).toBe("Coolify");
    // The panel never decides it from the link: the control plane says it, at the store's own repository alone.
    expect(sourceWords(source("onepanel", "https://github.com/1Panel-dev/appstore", CatalogFormat.ONEPANEL))).toBe(
      "1panel-dev/appstore",
    );
    // A fork in a store's format is no store of its own.
    expect(sourceWords(source("onepanel", "https://github.com/acme/appstore", CatalogFormat.ONEPANEL))).toBe(
      "acme/appstore",
    );
    // A store's repository read in another format is that repository.
    expect(sourceWords(source("cool", "https://github.com/coollabsio/coolify"))).toBe("coollabsio/coolify");
    expect(sourceWords(source("community", "https://git.example.com/catalog"))).toBe("git.example.com/catalog");
  });

  it("says the folder, or the ID last, where two would read alike", () => {
    const words = sourcesWords([
      source("apps", "https://github.com/acme/catalog", CatalogFormat.DOCKWELL, "apps"),
      source("more", "https://github.com/acme/catalog", CatalogFormat.DOCKWELL, "more"),
      source("one", "https://github.com/acme/one"),
    ]);
    expect([...words.values()]).toEqual(["acme/catalog (apps)", "acme/catalog (more)", "acme/one"]);
  });
});
