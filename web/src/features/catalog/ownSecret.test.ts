// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Characters, GenerateAs, InputSchema, InputType } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { nextFree } from "./freeName";
import { OWN_FLOOR, ownFault, ownRule, strongValue } from "./ownSecret";

const secret = (more: Parameters<typeof create<typeof InputSchema>>[1] = {}) =>
  create(InputSchema, { name: "KEY", label: "Key", type: InputType.SECRET, generate: 32, ...more });

describe("A person's own value for a generated secret", () => {
  it("takes what the control plane takes, as the input says", () => {
    expect(ownRule(secret())).toEqual({ form: "text", min: OWN_FLOOR, urlSafe: false });
    expect(ownRule(secret({ generate: 64, minLength: 64 }))).toEqual({ form: "text", min: 64, urlSafe: false });
    expect(ownRule(secret({ characters: Characters.URL_SAFE }))).toEqual({ form: "text", min: 8, urlSafe: true });
    expect(ownRule(secret({ generate: 64, generateAs: GenerateAs.HEX }))).toEqual({ form: "hex", size: 64 });
    expect(ownRule(secret({ generateAs: GenerateAs.BASE64 }))).toEqual({ form: "base64", size: 32 });
  });

  it("says what's wrong, as the control plane would refuse it", () => {
    const text = ownRule(secret());
    expect(ownFault(text, "eight ch")).toBeUndefined();
    expect(ownFault(text, "correct horse $battery 'staple\"")).toBeUndefined();
    expect(ownFault(text, "seven c")).toBe("short");
    expect(ownFault(text, " padded value ")).toBe("ends");
    expect(ownFault(text, "tab\there!x")).toBe("unprintable");
    const url = ownRule(secret({ characters: Characters.URL_SAFE }));
    expect(ownFault(url, "Abc-12.3_~xyz")).toBeUndefined();
    expect(ownFault(url, "p@ss/w0rd#1")).toBe("url");
    const hex = ownRule(secret({ generate: 8, generateAs: GenerateAs.HEX }));
    expect(ownFault(hex, "0aF19bC2")).toBeUndefined();
    expect(ownFault(hex, "0aF19bCz")).toBe("form");
    expect(ownFault(hex, "dead")).toBe("form");
    const b64 = ownRule(secret({ generate: 4, generateAs: GenerateAs.BASE64 }));
    expect(ownFault(b64, "AAECAw==")).toBeUndefined();
    expect(ownFault(b64, "AAEC")).toBe("form");
    expect(ownFault(b64, "not base64!")).toBe("form");
  });

  it("generates a strong value that its own check takes, different each time", () => {
    for (const input of [
      secret(),
      secret({ generate: 64, minLength: 64 }),
      secret({ characters: Characters.URL_SAFE }),
      secret({ generate: 33, generateAs: GenerateAs.HEX }),
      secret({ generateAs: GenerateAs.BASE64 }),
    ]) {
      const rule = ownRule(input);
      const one = strongValue(rule, input.generate);
      expect(ownFault(rule, one)).toBeUndefined();
      expect(one).not.toBe(strongValue(rule, input.generate));
    }
    expect(strongValue(ownRule(secret()), 32)).toMatch(/^[A-Za-z0-9]{32}$/);
  });
});

describe("A taken name", () => {
  it("moves on to the next name the projects here leave", () => {
    expect(nextFree("mysql", ["mysql"])).toBe("mysql-2");
    expect(nextFree("mysql", ["mysql", "mysql-2"])).toBe("mysql-3");
    expect(nextFree("mysql-2", ["mysql", "mysql-2"])).toBe("mysql-3");
    expect(nextFree("my-db", ["my-db"])).toBe("my-db-2");
  });
});
