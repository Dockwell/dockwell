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

  it("reads no URL's host and port as a file, and offers no path through . or .. (#800's review, L1)", () => {
    expect(fileOf('upstream: "http://127.0.0.1:9000/"')).toBeUndefined();
    expect(fileOf("connect() failed at https://example.com:443/api")).toBeUndefined();
    expect(fileOf("bind to unix:/run/php/php8.3-fpm.sock:1 failed")).toBeUndefined();
    expect(fileOf("open /etc/../root/.ssh/id_rsa:1 failed")).toBeUndefined();
    expect(fileOf("open /etc/./nginx/nginx.conf failed")).toBeUndefined();
    // A path in quotes or after = is still one.
    expect(fileOf('include "/etc/nginx/conf.d/site.conf" failed')).toEqual({ path: "/etc/nginx/conf.d/site.conf" });
    expect(fileOf("in '/etc/nginx/conf.d/site.conf:7'")).toEqual({ path: "/etc/nginx/conf.d/site.conf", line: 7 });
    expect(fileOf("config=/etc/app/app.ini")).toEqual({ path: "/etc/app/app.ini" });
  });
});
