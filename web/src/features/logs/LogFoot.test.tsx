// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { LogChunkSchema, LogLevel } from "@dockwell/api";
import { i18n } from "@lingui/core";
import { I18nProvider } from "@lingui/react";
import { act, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { LogFoot, SearchCount } from "./LogFoot";
import { LogStore, NO_FILTER } from "./store";

function filled(): LogStore {
  const store = new LogStore();
  const lines = ["one", "two", "three"].map((text, i) => ({
    time: { seconds: 1_793_832_000n + BigInt(i), nanos: 0 },
    text,
    level: LogLevel.UNSPECIFIED,
  }));
  store.add([create(LogChunkSchema, { sourceId: "a", lines })]);
  return store;
}

const words = (store: LogStore, query: string): ReactNode => (
  <I18nProvider i18n={i18n}>
    <LogFoot
      store={store}
      deep={undefined}
      query={query}
      stale={false}
      live={false}
      following={false}
      rangeLabel=""
      rangeEnd={0}
      levelFiltered={false}
      phone={false}
      onPause={() => {}}
      onLastSkip={() => {}}
    />
    <SearchCount store={store} query={query} step={undefined} />
  </I18nProvider>
);

describe("what a pause says", () => {
  beforeEach(() => activateLocale("en"));
  afterEach(() => vi.useRealTimers());

  // The page stops following a render before the store holds still, and the foot
  // drawn in between said a pause at 00:00:00, with nothing since.
  it("says when it paused as soon as the store holds still", () => {
    const store = filled();
    const paused = (
      <I18nProvider i18n={i18n}>
        <LogFoot
          store={store}
          deep={undefined}
          query=""
          stale={false}
          live={true}
          following={false}
          rangeLabel=""
          rangeEnd={0}
          levelFiltered={false}
          phone={false}
          onPause={() => {}}
          onLastSkip={() => {}}
        />
      </I18nProvider>
    );
    const { container } = render(paused);
    const at = new Date(2026, 10, 8, 9, 41, 7);
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(at);
    act(() => store.setHolding(true));
    expect(container).toHaveTextContent("Paused at 09:41:07. 0 lines since.");
  });
});

describe("what a search says of what it found", () => {
  beforeEach(() => activateLocale("en"));

  // The page knows what was asked a moment before the store has searched for it,
  // which in a long log is long enough to read as every line matching.
  it("waits for the store to have searched, then says at once", () => {
    const store = filled();
    const { container } = render(words(store, "t"));
    expect(container).not.toHaveTextContent("lines loaded");
    expect(container.querySelector(".logs-search-count")).toBeEmptyDOMElement();
    act(() => store.setFilter({ ...NO_FILTER, query: "t" }));
    expect(screen.getByText("2 of the 3 lines loaded match.")).toBeVisible();
    expect(container.querySelector(".logs-search-count")).toHaveTextContent("2");
  });

  it("goes by the search asked for, and not the one before", () => {
    const store = filled();
    store.setFilter({ ...NO_FILTER, query: "t" });
    const { container, rerender } = render(words(store, "t"));
    expect(screen.getByText("2 of the 3 lines loaded match.")).toBeVisible();
    rerender(words(store, "th"));
    expect(container).not.toHaveTextContent("lines loaded");
    act(() => store.setFilter({ ...NO_FILTER, query: "th" }));
    expect(screen.getByText("1 of the 3 lines loaded matches.")).toBeVisible();
  });
});
