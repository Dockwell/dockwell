// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it, vi } from "vitest";

import { attributeValue, evaluate, HoleError, pieces, Scope, toText, wholeHole } from "./holes";

const scope = new Scope({ user: { name: "Mira", tags: ["a", "b"] }, n: 0, fn: () => 1 }).child({
  $index: 2,
  item: "x",
});

describe("evaluate", () => {
  it("reads literals", () => {
    expect([
      evaluate("16", scope),
      evaluate("-0.5", scope),
      evaluate("true", scope),
      evaluate("null", scope),
      evaluate("'hi'", scope),
    ]).toEqual([16, -0.5, true, null, "hi"]);
    expect(evaluate("undefined", scope)).toBeUndefined();
  });

  it("follows dotted paths through loop variables and renderVals", () => {
    expect(evaluate(" user.name ", scope)).toBe("Mira");
    expect(evaluate("user.tags.1", scope)).toBe("b");
    expect(evaluate("user.tags.length", scope)).toBe(2);
    expect(evaluate("$index", scope)).toBe(2);
    expect(evaluate("item", scope)).toBe("x");
    expect(evaluate("n", scope)).toBe(0);
  });

  it("gives undefined for a path that runs out", () => {
    expect(evaluate("missing.deeper.still", scope)).toBeUndefined();
  });

  it("rejects expressions", () => {
    expect(() => evaluate("a + b", scope)).toThrow(HoleError);
    expect(() => evaluate("!x", scope)).toThrow(HoleError);
    expect(() => evaluate("fn()", scope)).toThrow(HoleError);
  });
});

describe("attributes and text", () => {
  it("passes a whole hole through as the raw value", () => {
    const onError = vi.fn();
    expect(attributeValue("{{fn}}", scope, onError)).toBe(scope.get("fn"));
    expect(attributeValue("{{16}}", scope, onError)).toBe(16);
    expect(onError).not.toHaveBeenCalled();
  });

  it("interpolates mixed text into a string, with nothing for null and booleans", () => {
    expect(attributeValue("Hi {{user.name}}, {{missing}}{{true}}#{{$index}}", scope, vi.fn())).toBe("Hi Mira, #2");
  });

  it("keeps literal text as it is", () => {
    expect(attributeValue("plain", scope, vi.fn())).toBe("plain");
  });

  it("reports a bad hole and goes on", () => {
    const onError = vi.fn();
    expect(pieces("a {{x + 1}} b", scope, onError)).toEqual([{ text: "a " }, { value: undefined }, { text: " b" }]);
    expect(onError).toHaveBeenCalledWith(expect.any(HoleError));
  });

  it("knows a whole hole from two holes", () => {
    expect(wholeHole("{{ a.b }}")).toBe(" a.b ");
    expect(wholeHole("{{a}} {{b}}")).toBeNull();
    expect(wholeHole("{{a}}{{b}}")).toBeNull();
    expect(wholeHole("x{{a}}")).toBeNull();
  });

  it("renders values as text", () => {
    expect([toText(0), toText("s"), toText(null), toText(undefined), toText(false)]).toEqual(["0", "s", "", "", ""]);
  });
});
