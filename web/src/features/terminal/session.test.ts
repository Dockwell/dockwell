// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type AttachTerminalResponse,
  AttachTerminalResponseSchema,
  TerminalEndReason,
  TerminalEndSchema,
  WriteTerminalResponseSchema,
} from "@dockwell/api";
import { afterEach, describe, expect, it, vi } from "vitest";

import { MAX_WRITE, type TerminalClient, TerminalSession, type TerminalStatus } from "./session";

const decoder = new TextDecoder();
const bytes = (text: string) => new TextEncoder().encode(text);

/** A server stream the test pushes to, ending when it's told or aborted. */
class Stream {
  private queue: (AttachTerminalResponse | Error | "end")[] = [];
  private wake: (() => void) | undefined;
  constructor(readonly signal: AbortSignal) {
    signal.addEventListener("abort", () => this.wake?.());
  }
  push(m: AttachTerminalResponse | Error | "end") {
    this.queue.push(m);
    this.wake?.();
  }
  async *[Symbol.asyncIterator]() {
    for (;;) {
      if (this.signal.aborted) throw this.signal.reason;
      const next = this.queue.shift();
      if (next === "end") return;
      if (next instanceof Error) throw next;
      if (next) {
        yield next;
        continue;
      }
      await new Promise<void>((resolve) => {
        this.wake = resolve;
      });
    }
  }
}

/** A terminal on a pretend server: it takes what's typed, by offset, as the agent does. */
function server() {
  const streams: { req: { offset: bigint; columns: number; rows: number }; stream: Stream }[] = [];
  const writes: { offset: bigint; data: string; columns: number; rows: number }[] = [];
  let taken = 0n;
  let room = Number.POSITIVE_INFINITY;
  let failWrites: ConnectError | undefined;
  const client: TerminalClient = {
    attachTerminal: (req, { signal }) => {
      const stream = new Stream(signal);
      streams.push({ req, stream });
      return stream;
    },
    writeTerminal: async (req) => {
      writes.push({ offset: req.offset, data: decoder.decode(req.data), columns: req.columns, rows: req.rows });
      if (failWrites) throw failWrites;
      if (req.offset <= taken) {
        const skip = Number(taken - req.offset);
        const fresh = Math.max(0, Math.min(req.data.length - skip, room));
        taken += BigInt(fresh);
      }
      return create(WriteTerminalResponseSchema, { offset: taken });
    },
  };
  return {
    client,
    streams,
    writes,
    get taken() {
      return taken;
    },
    set taken(n: bigint) {
      taken = n;
    },
    set room(n: number) {
      room = n;
    },
    set failWrites(err: ConnectError | undefined) {
      failWrites = err;
    },
    last: () => streams.at(-1) ?? expect.fail("no attachment"),
  };
}

const attached = (offset: bigint, inputOffset: bigint) =>
  create(AttachTerminalResponseSchema, { attached: true, offset, inputOffset });
const printed = (offset: bigint, text: string) => create(AttachTerminalResponseSchema, { offset, data: bytes(text) });

function session(client: TerminalClient) {
  const data: string[] = [];
  const statuses: TerminalStatus[] = [];
  const lost: bigint[] = [];
  const s = new TerminalSession(
    client,
    "trm_1",
    { onData: (d) => data.push(decoder.decode(d)), onStatus: (st) => statuses.push(st), onLost: (n) => lost.push(n) },
    { columns: 80, rows: 24 },
  );
  return { s, data, statuses, lost, kind: () => s.current.kind };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("a terminal's session", () => {
  it("shows what it prints, once, in order, and types what's typed with its offset", async () => {
    const srv = server();
    const { s, data, kind } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    expect(srv.last().req).toMatchObject({ offset: 0n, columns: 80, rows: 24 });
    srv.last().stream.push(attached(0n, 0n));
    srv.last().stream.push(printed(0n, "$ "));
    // Sent again, as after a reattach: only what's new shows.
    srv.last().stream.push(printed(0n, "$ ls"));
    await vi.waitFor(() => expect(data.join("")).toBe("$ ls"));
    expect(kind()).toBe("attached");
    s.type("echo hi\r");
    await vi.waitFor(() => expect(srv.taken).toBe(8n));
    expect(srv.writes.map((w) => [w.offset, w.data])).toContainEqual([0n, "echo hi\r"]);
    s.stop();
  });

  it("gathers what's typed while a write is out into the next", async () => {
    const srv = server();
    const { s } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    srv.last().stream.push(attached(0n, 0n));
    await vi.waitFor(() => expect(s.current.kind).toBe("attached"));
    for (const key of "abcdef") s.type(key);
    await vi.waitFor(() => expect(srv.taken).toBe(6n));
    // The first went alone, and the rest together after it.
    expect(srv.writes.filter((w) => w.data).length).toBeLessThanOrEqual(3);
    expect(
      srv.writes
        .filter((w) => w.data)
        .map((w) => w.data)
        .join(""),
    ).toBe("abcdef");
    s.stop();
  });

  it("sends in parts what's longer than a write carries, and the rest of what the terminal hadn't room for", async () => {
    const srv = server();
    const { s } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    srv.last().stream.push(attached(0n, 0n));
    await vi.waitFor(() => expect(s.current.kind).toBe("attached"));
    srv.room = 1000;
    s.type("x".repeat(MAX_WRITE + 10));
    await vi.waitFor(() => expect(srv.taken).toBe(1000n));
    srv.room = Number.POSITIVE_INFINITY;
    await vi.waitFor(() => expect(srv.taken).toBe(BigInt(MAX_WRITE + 10)), { timeout: 3000 });
    expect(Math.max(...srv.writes.map((w) => w.data.length))).toBeLessThanOrEqual(MAX_WRITE);
    s.stop();
  });

  it("attaches again from the byte it had after a dropped stream, and sends what was typed meanwhile", async () => {
    const srv = server();
    const { s, data, kind } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    srv.last().stream.push(attached(0n, 0n));
    srv.last().stream.push(printed(0n, "hello\r\n"));
    await vi.waitFor(() => expect(data.join("")).toBe("hello\r\n"));
    srv.last().stream.push(new ConnectError("gone", Code.Unavailable));
    await vi.waitFor(() => expect(kind()).toBe("reconnecting"));
    s.type("typed while away");
    expect(srv.writes.some((w) => w.data.includes("away"))).toBe(false);
    await vi.waitFor(() => expect(srv.streams).toHaveLength(2), { timeout: 3000 });
    expect(srv.last().req.offset).toBe(7n);
    srv.last().stream.push(attached(7n, 0n));
    srv.last().stream.push(printed(7n, "more"));
    await vi.waitFor(() => expect(srv.taken).toBe(16n));
    await vi.waitFor(() => expect(data.join("")).toBe("hello\r\nmore"));
    s.stop();
  });

  it("drops what the terminal says it has taken already, and says what was lost", async () => {
    const srv = server();
    const { s, lost } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    srv.last().stream.push(attached(0n, 0n));
    srv.last().stream.push(printed(0n, "abc"));
    await vi.waitFor(() => expect(s.received).toBe(3n));
    srv.failWrites = new ConnectError("gone", Code.Unavailable);
    s.type("12345");
    await vi.waitFor(() => expect(srv.writes.length).toBeGreaterThan(0));
    srv.last().stream.push(new ConnectError("gone", Code.Unavailable));
    await vi.waitFor(() => expect(srv.streams).toHaveLength(2), { timeout: 3000 });
    srv.failWrites = undefined;
    // The terminal took 3 of the 5 before the stream dropped, and kept only what came after byte 100.
    srv.taken = 3n;
    srv.last().stream.push(attached(100n, 3n));
    await vi.waitFor(() => expect(s.unsent).toBe(0));
    expect(srv.writes.at(-1)).toMatchObject({ offset: 3n, data: "45" });
    expect(lost).toEqual([97n]);
    s.stop();
  });

  it("stays where it moved to until it's brought back", async () => {
    const srv = server();
    const { s, kind } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    srv.last().stream.push(attached(0n, 0n));
    srv.last().stream.push(create(AttachTerminalResponseSchema, { moved: true }));
    await vi.waitFor(() => expect(kind()).toBe("moved"));
    s.stop();
    s.start();
    await new Promise((r) => setTimeout(r, 50));
    expect(srv.streams).toHaveLength(1);
    s.bringBack();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(2));
    s.stop();
  });

  it("ends with the terminal, and ends for good when the control plane no longer has it", async () => {
    const srv = server();
    const first = session(srv.client);
    first.s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    const end = create(TerminalEndSchema, { reason: TerminalEndReason.EXITED, exitCode: 3 });
    srv.last().stream.push(create(AttachTerminalResponseSchema, { end }));
    await vi.waitFor(() => expect(first.kind()).toBe("ended"));
    expect(first.s.current).toEqual({ kind: "ended", end });

    const second = session(srv.client);
    second.s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(2));
    srv.last().stream.push(new ConnectError("you have no open terminal", Code.NotFound));
    await vi.waitFor(() => expect(second.kind()).toBe("ended"));
    await new Promise((r) => setTimeout(r, 50));
    expect(srv.streams).toHaveLength(2);
  });

  it("changes the terminal's size with a write of its own", async () => {
    const srv = server();
    const { s } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    srv.last().stream.push(attached(0n, 0n));
    await vi.waitFor(() => expect(s.current.kind).toBe("attached"));
    s.resize(120, 40);
    await vi.waitFor(() => expect(srv.writes.some((w) => w.columns === 120 && w.rows === 40)).toBe(true));
    s.stop();
  });

  it("attaches again when its stream goes quiet, with no heartbeat", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const srv = server();
    const { s } = session(srv.client);
    s.start();
    await vi.waitFor(() => expect(srv.streams).toHaveLength(1));
    srv.last().stream.push(attached(0n, 0n));
    await vi.waitFor(() => expect(s.current.kind).toBe("attached"));
    await vi.advanceTimersByTimeAsync(31_000);
    await vi.waitFor(() => expect(srv.streams.length).toBeGreaterThan(1));
    s.stop();
  });
});
