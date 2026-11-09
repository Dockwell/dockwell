// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import type { AttachTerminalResponse, TerminalEnd, WriteTerminalResponse } from "@dockwell/api";

import { reasonOf } from "../../lib/errors";
import { delay, keepTrying } from "../../lib/retry";

/** The most one write carries, as the control plane takes it. */
export const MAX_WRITE = 64 * 1024;

/** What a stream that hears nothing for this long, not even a heartbeat, is: gone. The control plane beats every 10 s. */
export const SILENT_FOR_MS = 30_000;

/** How long to wait before sending again what the terminal hadn't room for. */
const FULL_WAIT_MS = 100;

/** How a terminal's attachment stands, as the screen shows it. */
export type TerminalStatus =
  | { kind: "connecting" }
  | { kind: "attached" }
  /** The stream dropped, and it's attaching again by itself. What's typed meanwhile waits. */
  | { kind: "reconnecting"; error: unknown }
  /** It's attached somewhere else now: another tab or window. */
  | { kind: "moved" }
  | { kind: "ended"; end: TerminalEnd | undefined }
  /** Attaching again won't help, as for a session that ended. */
  | { kind: "failed"; error: unknown };

/** What a session needs of TerminalService. */
export interface TerminalClient {
  attachTerminal(
    req: { terminalId: string; offset: bigint; columns: number; rows: number },
    opts: { signal: AbortSignal },
  ): AsyncIterable<AttachTerminalResponse>;
  writeTerminal(
    req: { terminalId: string; offset: bigint; data: Uint8Array; columns: number; rows: number },
    opts: { signal: AbortSignal },
  ): Promise<WriteTerminalResponse>;
}

export interface SessionHandlers {
  /** What the terminal printed, in order, with nothing twice. */
  onData: (data: Uint8Array) => void;
  /** Some of what it printed was lost while the panel was away, as the server keeps only the newest. */
  onLost?: (bytes: bigint) => void;
  onStatus: (status: TerminalStatus) => void;
}

const encoder = new TextEncoder();

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
  const out = new Uint8Array(a.length + b.length);
  out.set(a);
  out.set(b, a.length);
  return out;
}

/** Whether attaching again could work. One that can't says so for good. */
function lasting(error: unknown): boolean {
  const code = ConnectError.from(error).code;
  return (
    code === Code.NotFound ||
    code === Code.Unauthenticated ||
    code === Code.PermissionDenied ||
    code === Code.InvalidArgument
  );
}

/**
 * One terminal as the panel holds it: what it printed, from where, and what's typed
 * that it hasn't taken. It attaches, and attaches again after a dropped stream from
 * the byte it had, so nothing is missed or shown twice. What's typed goes one write
 * at a time, gathered meanwhile, each with its offset, so a write sent again types
 * nothing twice.
 */
export class TerminalSession {
  /** How much of what the terminal printed the panel has. */
  received = 0n;
  private status: TerminalStatus = { kind: "connecting" };
  private stopped = new AbortController();
  private attachment: AbortController | undefined;
  /** What's typed from typedFrom on that the terminal hasn't taken. */
  private queue: Uint8Array = new Uint8Array(0);
  private typedFrom = 0n;
  private writing = false;
  private size: { columns: number; rows: number };
  private sizeSent = true;
  private writes = new AbortController();
  private running = false;

  constructor(
    private readonly client: TerminalClient,
    readonly terminalId: string,
    private readonly handlers: SessionHandlers,
    size: { columns: number; rows: number },
  ) {
    this.size = size;
  }

  get current(): TerminalStatus {
    return this.status;
  }

  private set(status: TerminalStatus) {
    this.status = status;
    this.handlers.onStatus(status);
  }

  /** Attaches, and keeps attaching until the terminal ends, it moves, or stop. */
  start(): void {
    // A terminal that moved stays where it went until someone brings it back.
    if (this.running || this.status.kind === "ended" || this.status.kind === "moved") return;
    if (this.stopped.signal.aborted) this.stopped = new AbortController();
    if (this.writes.signal.aborted) this.writes = new AbortController();
    if (this.status.kind !== "connecting") this.set({ kind: "connecting" });
    this.running = true;
    const signal = this.stopped.signal;
    void keepTrying(
      signal,
      { minMs: 500, maxMs: 5_000 },
      async (connected, attempt) => {
        const each = new AbortController();
        this.attachment = each;
        const onAbort = () => each.abort(attempt.reason);
        attempt.addEventListener("abort", onAbort, { once: true });
        let silent: ReturnType<typeof setTimeout> | undefined;
        const listen = () => {
          clearTimeout(silent);
          silent = setTimeout(
            () => each.abort(new ConnectError("the terminal's stream went quiet", Code.Unavailable)),
            SILENT_FOR_MS,
          );
        };
        try {
          listen();
          const stream = this.client.attachTerminal(
            { terminalId: this.terminalId, offset: this.received, columns: this.size.columns, rows: this.size.rows },
            { signal: each.signal },
          );
          for await (const msg of stream) {
            listen();
            if (this.take(msg, connected) === "done") return "done";
          }
          if (each.signal.aborted) throw each.signal.reason;
          throw new ConnectError("the terminal's stream ended", Code.Unavailable);
        } finally {
          clearTimeout(silent);
          attempt.removeEventListener("abort", onAbort);
          if (this.attachment === each) this.attachment = undefined;
        }
      },
      (err) => {
        if (lasting(err)) {
          // A terminal the control plane no longer has ended while nobody watched.
          this.set(
            ConnectError.from(err).code === Code.NotFound
              ? { kind: "ended", end: undefined }
              : { kind: "failed", error: err },
          );
          return "done";
        }
        if (this.status.kind !== "connecting") this.set({ kind: "reconnecting", error: err });
        return undefined;
      },
    ).finally(() => {
      if (this.stopped.signal === signal) this.running = false;
    });
  }

  private take(msg: AttachTerminalResponse, connected: () => void): "done" | undefined {
    if (msg.attached) {
      connected();
      if (msg.offset > this.received) {
        // What came between went while nobody was attached, past what the server keeps.
        if (this.received > 0n) this.handlers.onLost?.(msg.offset - this.received);
        this.received = msg.offset;
      }
      this.taken(msg.inputOffset);
      this.sizeSent = false; // the attachment carried the size; a resize meanwhile goes again
      this.set({ kind: "attached" });
      this.flush();
      return undefined;
    }
    if (msg.data.length > 0) {
      const end = msg.offset + BigInt(msg.data.length);
      if (end <= this.received) return undefined; // had it already
      let data = msg.data;
      if (msg.offset > this.received) {
        this.handlers.onLost?.(msg.offset - this.received);
      } else if (msg.offset < this.received) {
        data = data.subarray(Number(this.received - msg.offset));
      }
      this.received = end;
      this.handlers.onData(data);
    }
    if (msg.moved) {
      this.set({ kind: "moved" });
      return "done";
    }
    if (msg.end) {
      this.set({ kind: "ended", end: msg.end });
      return "done";
    }
    return undefined;
  }

  /** The terminal has taken what's typed up to offset: it needn't go again. */
  private taken(offset: bigint) {
    if (offset <= this.typedFrom) return;
    const drop = offset - this.typedFrom;
    this.queue = drop >= BigInt(this.queue.length) ? new Uint8Array(0) : this.queue.subarray(Number(drop));
    this.typedFrom = offset;
  }

  /** Types text, or bytes as they are. */
  type(input: string | Uint8Array): void {
    const bytes = typeof input === "string" ? encoder.encode(input) : input;
    if (bytes.length === 0) return;
    this.queue = concat(this.queue, bytes);
    this.flush();
  }

  /** Changes the terminal's size, with the next write or one of its own. */
  resize(columns: number, rows: number): void {
    if (columns === this.size.columns && rows === this.size.rows) return;
    this.size = { columns, rows };
    this.sizeSent = false;
    this.flush();
  }

  /** Has the terminal back here, after it moved somewhere else. */
  bringBack(): void {
    if (this.status.kind !== "moved" && this.status.kind !== "failed") return;
    this.stop();
    this.set({ kind: "connecting" });
    this.start();
  }

  /** Lets the terminal go on without this page. start attaches again from where it was. */
  stop(): void {
    this.stopped.abort();
    this.writes.abort();
    this.running = false;
  }

  /** Whether anything typed hasn't been taken yet. */
  get unsent(): number {
    return this.queue.length;
  }

  private flush(): void {
    if (this.writing || this.status.kind !== "attached" || (this.queue.length === 0 && this.sizeSent)) return;
    this.writing = true;
    void this.write().finally(() => {
      this.writing = false;
      if (!this.stopped.signal.aborted) this.flush();
    });
  }

  private async write(): Promise<void> {
    const signal = this.writes.signal;
    const data = this.queue.subarray(0, MAX_WRITE);
    const from = this.typedFrom;
    const size = this.sizeSent ? { columns: 0, rows: 0 } : this.size;
    try {
      const res = await this.client.writeTerminal(
        { terminalId: this.terminalId, offset: from, data, columns: size.columns, rows: size.rows },
        { signal },
      );
      if (size.columns > 0 && size === this.size) this.sizeSent = true;
      this.taken(res.offset);
      // The terminal hadn't room for all of it: what's left goes in a moment.
      if (data.length > 0 && res.offset < from + BigInt(data.length)) await delay(FULL_WAIT_MS, signal);
    } catch (err) {
      if (signal.aborted) return;
      const reason = reasonOf(err);
      if (reason === "not_attached" || ConnectError.from(err).code === Code.NotFound) {
        // What's typed waits for the attachment that's coming, and goes then.
        this.attachment?.abort(err);
        return;
      }
      await delay(1_000, signal);
    }
  }
}
