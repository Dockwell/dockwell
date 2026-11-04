// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { describeUserAgent } from "./userAgent";

describe("describeUserAgent", () => {
  it.each([
    [
      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
      "Chrome",
      "macOS",
      false,
    ],
    [
      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1",
      "Safari",
      "iOS",
      true,
    ],
    [
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0",
      "Edge",
      "Windows",
      false,
    ],
    ["Mozilla/5.0 (X11; Linux x86_64; rv:144.0) Gecko/20100101 Firefox/144.0", "Firefox", "Linux", false],
    [
      "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36",
      "Chrome",
      "Android",
      true,
    ],
    ["curl/8.11.0", "", "", false],
  ])("reads %s", (ua, browser, os, mobile) => {
    expect(describeUserAgent(ua)).toEqual({ browser, os, mobile });
  });
});
