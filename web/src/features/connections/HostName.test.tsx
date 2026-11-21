// SPDX-License-Identifier: AGPL-3.0-only

import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { HostName } from "./HostName";

describe("HostName", () => {
  it("shortens an account ID in the middle, breaks only after dots and hyphens, and says it whole on hover", () => {
    const host = "0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com";
    const { container } = render(<HostName host={host} />);
    const code = container.querySelector("code");
    expect(code).toHaveTextContent("012345…cdef.r2.cloudflarestorage.com");
    expect(code).toHaveAttribute("title", host);
    expect(code?.querySelectorAll("wbr")).toHaveLength(3);
  });

  it("keeps a short host as it is, with a break after each hyphen too", () => {
    const { container } = render(<HostName host="s3.us-west-004.backblazeb2.com" />);
    const code = container.querySelector("code");
    expect(code).toHaveTextContent("s3.us-west-004.backblazeb2.com");
    expect(code?.querySelectorAll("wbr")).toHaveLength(5);
  });
});
