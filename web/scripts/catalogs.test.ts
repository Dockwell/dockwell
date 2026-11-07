// SPDX-License-Identifier: AGPL-3.0-only

import { formatter } from "@lingui/format-po";
import { afterEach, describe, expect, it, vi } from "vitest";

import { mergeCatalog, refill } from "./catalogs.ts";

const ctx = { locale: "zh-CN", sourceLocale: "en", filename: "web/src/locales/zh-CN/messages.po" };
const DATE = "2026-11-03 23:10+0000";

/** A zh-CN catalog with these messages and translations, in this order. */
function catalog(...entries: [string, string][]): string {
  const header = [
    'msgid ""',
    'msgstr ""',
    `"POT-Creation-Date: ${DATE}\\n"`,
    '"MIME-Version: 1.0\\n"',
    '"Content-Type: text/plain; charset=utf-8\\n"',
    '"Content-Transfer-Encoding: 8bit\\n"',
    '"X-Generator: @lingui/cli\\n"',
    '"Language: zh-CN\\n"',
  ].join("\n");
  return `${[header, ...entries.map(([id, str]) => `msgid "${id}"\nmsgstr "${str}"`)].join("\n\n")}\n`;
}

/** A catalog with one more header field, after the language. */
function withField(text: string, field: string): string {
  return text.replace('"Language: zh-CN\\n"', `"Language: zh-CN\\n"\n"${field}\\n"`);
}

/** Each message's translation in a catalog, by its English. */
async function translations(text: string): Promise<Record<string, string | undefined>> {
  const parsed = await formatter({ origins: false }).parse(text, ctx);
  return Object.fromEntries(Object.values(parsed).map((m) => [m.message ?? "", m.translation]));
}

describe("mergeCatalog", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("keeps the header's date, so two branches merged at different times agree", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-12-25T10:00:00Z"));
    const base = catalog(["Cancel", "取消"]);
    const { text } = await mergeCatalog(
      {
        base,
        ours: catalog(["Cancel", "取消"], ["Disk", "磁盘"]),
        theirs: catalog(["Address", "地址"], ["Cancel", "取消"]),
      },
      ctx,
    );
    expect(text).toContain(`"POT-Creation-Date: ${DATE}\\n"`);
    expect(text).not.toContain("2026-12-25");
  });

  it("keeps every message either side adds, with its translation", async () => {
    const base = catalog(["Cancel", "取消"]);
    const merged = await mergeCatalog(
      {
        base,
        ours: catalog(["Cancel", "取消"], ["Disk", "磁盘"]),
        theirs: catalog(["Address", "地址"], ["Cancel", "取消"]),
      },
      ctx,
    );
    expect(merged.disagree).toEqual([]);
    expect(await translations(merged.text)).toEqual({ Cancel: "取消", Disk: "磁盘", Address: "地址" });
  });

  it("refuses a message both sides add with different translations", async () => {
    const merged = await mergeCatalog(
      { base: catalog(), ours: catalog(["Name", "名称"]), theirs: catalog(["Name", "名字"]) },
      ctx,
    );
    expect(merged.disagree).toEqual(['"Name": "名称" on one side, "名字" on the other']);
  });

  it("refuses a message one side already holds twice with different translations, as a finished rebase leaves it", async () => {
    const ours = catalog(["Name", "名称"], ["Disk", "磁盘"], ["Name", "名字"]);
    const merged = await mergeCatalog({ base: catalog(), ours, theirs: catalog(["Disk", "磁盘"]) }, ctx);
    expect(merged.disagree).toEqual(['"Name": "名称" and "名字", both on one side']);
  });

  it("takes a message one side holds twice with one translation", async () => {
    const ours = catalog(["Name", "名称"], ["Disk", "磁盘"], ["Name", "名称"]);
    const merged = await mergeCatalog({ base: catalog(), ours, theirs: catalog() }, ctx);
    expect(merged.disagree).toEqual([]);
    expect(await translations(merged.text)).toEqual({ Name: "名称", Disk: "磁盘" });
  });

  it("merges the header a field at a time, keeping the base's date", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-12-25T10:00:00Z"));
    const plural = "Plural-Forms: nplurals=1; plural=0;";
    const merged = await mergeCatalog(
      { base: catalog(), ours: catalog(), theirs: withField(catalog(), plural).replace(DATE, "2026-11-20 08:00+0000") },
      ctx,
    );
    expect(merged.text).toContain(`"${plural}\\n"`);
    expect(merged.text).toContain(`"POT-Creation-Date: ${DATE}\\n"`);
  });

  it("takes a message both sides add with one translation", async () => {
    const merged = await mergeCatalog(
      { base: catalog(), ours: catalog(["Name", "名称"]), theirs: catalog(["Name", "名称"]) },
      ctx,
    );
    expect(merged.disagree).toEqual([]);
    expect(await translations(merged.text)).toEqual({ Name: "名称" });
  });

  it("keeps a message one side stopped using, with its translation, for the other side's use", async () => {
    const base = catalog(["Memory", "内存"]);
    const merged = await mergeCatalog({ base, ours: catalog(), theirs: catalog(["Memory", "内存"]) }, ctx);
    expect(await translations(merged.text)).toEqual({ Memory: "内存" });
  });

  it("takes the translation one side changed", async () => {
    const base = catalog(["Memory", "内存"]);
    const merged = await mergeCatalog({ base, ours: base, theirs: catalog(["Memory", "存储器"]) }, ctx);
    expect(await translations(merged.text)).toEqual({ Memory: "存储器" });
  });

  it("keeps a translation one side cleared, so it's translated again", async () => {
    const base = catalog(["Memory", "内存"]);
    const merged = await mergeCatalog({ base, ours: base, theirs: catalog(["Memory", ""]) }, ctx);
    expect(merged.chosen.get(Object.keys(await formatter().parse(base, ctx))[0] ?? "")).toBe("");
    expect(await translations(merged.text)).toEqual({ Memory: "" });
  });
});

describe("refill", () => {
  it("fills what extract left untranslated from either side, and keeps the header", async () => {
    const sides = { base: catalog(), ours: catalog(["Memory", "内存"]), theirs: catalog() };
    const merged = await mergeCatalog(sides, ctx);
    const extracted = catalog(["Disk", ""], ["Memory", ""]);
    const { text, filled } = await refill(extracted, merged, sides, ctx);
    expect(filled).toBe(1);
    expect(await translations(text)).toEqual({ Disk: "", Memory: "内存" });
    expect(text).toContain(`"POT-Creation-Date: ${DATE}\\n"`);
  });

  it("leaves a translation the merge cleared", async () => {
    const base = catalog(["Memory", "内存"]);
    const sides = { base, ours: base, theirs: catalog(["Memory", ""]) };
    const merged = await mergeCatalog(sides, ctx);
    const { filled } = await refill(catalog(["Memory", ""]), merged, sides, ctx);
    expect(filled).toBe(0);
  });
});
