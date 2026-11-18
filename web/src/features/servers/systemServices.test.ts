// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { fileOf } from "./systemServices";

describe("the file a failure names (#800)", () => {
  it("reads a path and its line, as nginx and others write them", () => {
    expect(fileOf("nginx: [emerg] unknown directive in /etc/nginx/sites-enabled/default:12")).toEqual({
      path: "/etc/nginx/sites-enabled/default",
      line: 12,
    });
    expect(fileOf("/srv/app/config.yml:3: mapping values are not allowed here")).toEqual({
      path: "/srv/app/config.yml",
      line: 3,
    });
  });

  it("takes a settings file without a line, and nothing else", () => {
    expect(fileOf("nginx: configuration file /etc/nginx/nginx.conf test failed")).toEqual({
      path: "/etc/nginx/nginx.conf",
    });
    // A program's own path, or none at all, isn't a file to open.
    expect(fileOf("/usr/sbin/nginx exited with status 1")).toBeUndefined();
    expect(fileOf("Failed with result 'exit-code'.")).toBeUndefined();
  });
});
