// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { renderHookWithApi } from "../../test/render";
import { useAuthError } from "./messages";

afterEach(async () => {
  await activateLocale("en");
});

// The header the control plane says why in (authapi.ReasonKey).
const refused = (reason: string, message: string) =>
  new ConnectError(message, Code.PermissionDenied, new Headers({ "Dockwell-Error": reason }));

describe("a role's refusal", () => {
  it("says the role doesn't allow it, and who changes it, in the reader's language", async () => {
    const roleRefusal = refused("needs_role", "this needs a developer's role or more, and you hold a viewer's role");
    const say = renderHookWithApi(useAuthError, {}).result.current;
    expect(say(roleRefusal)).toBe("Your role doesn't let you do this here. Ask an admin to change your role.");
    await activateLocale("zh-CN");
    const zh = renderHookWithApi(useAuthError, {}).result.current;
    expect(zh(roleRefusal)).toBe("你的角色不能在这里执行此操作。请让管理员更改你的角色。");
  });
});
