// SPDX-License-Identifier: AGPL-3.0-only

import {
  type LogLine as ApiLine,
  type LogChunk,
  type LogGap,
  LogGapReason,
  LogLevel,
  type LogStream,
} from "@dockwell/api";
import { type LogLine, type LogLines, plainText } from "@dockwell/ui";

/** The most lines a view keeps. Older ones leave, and the view says so. */
export const MAX_LINES = 100_000;
/** The most text a view keeps: a hundred thousand lines of 16 KiB would be far too much. */
export const MAX_BYTES = 64 << 20;
/**
 * The most a paused view sets aside of what comes, past which only the count goes
 * on. A reader who follows again wants the newest lines, and what shows stays as
 * it was until then, however long they read.
 */
export const MAX_HELD = 20_000;
export const MAX_HELD_BYTES = 16 << 20;
/** How far apart lines of one level can be, in milliseconds, and still read as one thing, as a stack trace does. */
const GROUP_MS = 100;

/** One thing in a log's order: a line, or a note about a place in it. */
export interface Entry {
  /** Unique in the store, and stays with the entry as others come and go. */
  id: number;
  /** When, in milliseconds, and the nanoseconds within its second for the order. */
  ms: number;
  nanos: number;
  source: string;
  level: LogLevel;
  stream: LogStream;
  text: string;
  cut: number;
  /** Lines left out here. */
  gap?: LogGap;
  /** A new container of a replica starts here, as a deploy does. */
  started?: true;
  /** How many lines that came while this view was paused left it unseen. */
  left?: number;
  /** How it was last drawn, with the words of drawnAt. */
  drawn?: LogLine;
  drawnAt?: number;
}

/** What to keep of what is in the store. An empty set keeps everything. */
export interface LogFilter {
  /** Which levels show. A line that names none counts as info, and debug shows with info. */
  levels: ReadonlySet<LogLevel>;
  /** Which sources show, by label. */
  label: string;
  /** Text to find, in any case. */
  query: string;
}

export const NO_FILTER: LogFilter = { levels: new Set(), label: "", query: "" };

const ESC = "\u001b";

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** The level a line shows under, in the filters. */
export function shownLevel(level: LogLevel): LogLevel {
  return level === LogLevel.WARN || level === LogLevel.ERROR ? level : LogLevel.INFO;
}

function compare(a: Entry, b: Entry): number {
  return a.ms - b.ms || a.nanos - b.nanos || a.id - b.id;
}

/**
 * The lines a view has loaded, in time order, within its limits, and which of
 * them its filters keep. Lines come in batches, almost always newer than all
 * that are here; those of a server a little behind another are merged in. Only
 * what shows is ever drawn, so this holds plain data, and a filter or a search
 * goes over it in a few milliseconds at the limit.
 */
export class LogStore implements LogLines {
  version = 0;
  epoch = 0;
  /** How many left, at the start: the position of the first entry here. */
  private base = 0;
  private entries: Entry[] = [];
  /** Entries before this index have left; they are cut off now and then. */
  private head = 0;
  private bytes = 0;
  private nextId = 1;

  private filter: LogFilter = NO_FILTER;
  private pattern: RegExp | undefined;
  /** The absolute positions of the entries the filter keeps, or undefined when it keeps all. */
  private kept: number[] | undefined;
  private keptHead = 0;
  private keptBase = 0;

  /** How many lines have left this view since it started, to say so. */
  evicted = 0;
  /** How many lines never reached this view, as markers in it said, to go on saying so. */
  skipped = 0;
  /** How many lines have ever come, for how fast they are coming. */
  received = 0;
  /** The newest marker of lines left out, to go to. */
  lastSkip: Entry | undefined;

  /** Whether what shows holds still, with what comes set aside. */
  private holding = false;
  /** When it started to, in milliseconds. */
  heldSince = 0;
  private held: Entry[] = [];
  private heldHead = 0;
  private heldBytes = 0;
  private heldCount = 0;
  private heldLost = 0;

  /** The first line of each error, oldest first, for going through them. */
  private errorEntries: Entry[] = [];
  private errorsHead = 0;
  /** How many of what the filter keeps are notes, not lines. */
  private notesKept = 0;
  private sources = new Map<string, string>();
  private seen = new Set<string>();
  private replicas = new Map<string, string>();
  /**
   * Sources whose first lines came before it was known which replica they or those
   * before them are: when those lines began, and which sources had lines then. The
   * page says which replica a source is once it has drawn it, which can be after
   * its first lines are here, so whether a new container starts there waits for it.
   */
  private undecided = new Map<string, { time: ApiLine["time"]; before: string[] }>();
  private listeners = new Set<() => void>();
  private drawLine: (entry: Entry) => LogLine = (e) => ({ id: e.id, time: "", source: e.source, text: e.text });
  private words = 0;

  /** Says how to draw an entry: its time, its source's name and the words of a note. */
  setDraw(draw: (entry: Entry) => LogLine): void {
    this.drawLine = draw;
    this.words++;
    this.bump();
  }

  /** Says what a source is called, and which replica it is, for filters and for notes. */
  setSource(id: string, label: string, replica: string): void {
    this.sources.set(id, label);
    this.replicas.set(id, replica);
    if (this.undecided.size > 0) this.decide();
  }

  /**
   * Says where a new container starts, for each source that waited to know, at the
   * first line it waited with, as soon as the replicas involved are known.
   */
  private decide(): void {
    const batch: Entry[] = [];
    for (const [source, first] of this.undecided) {
      const starts = this.startsAnother(source, first.before);
      if (starts === undefined) continue;
      this.undecided.delete(source);
      if (!starts) continue;
      // Just before that line, which has been here since and came first.
      const marker = this.entry(source, first.time, { started: true });
      marker.nanos -= 1;
      batch.push(marker);
    }
    if (batch.length === 0) return;
    if (this.holding) this.setAside(batch);
    else this.insert(batch);
  }

  /**
   * Whether a source's first lines are another container of a replica that had lines
   * here before, as a deploy makes: undefined until the replicas it takes are known.
   */
  private startsAnother(source: string, before: readonly string[]): boolean | undefined {
    const replica = this.replicas.get(source);
    if (replica === undefined) return undefined;
    if (before.some((id) => this.replicas.get(id) === replica)) return true;
    return before.every((id) => this.replicas.has(id)) ? false : undefined;
  }

  labelOf(id: string): string {
    return this.sources.get(id) ?? "";
  }

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  snapshot = (): number => this.version;

  private bump(): void {
    this.version++;
    for (const fn of this.listeners) fn();
  }

  /** Empties it, for what it shows to start over. */
  clear(): void {
    this.entries = [];
    this.head = 0;
    this.base += 1_000_000; // positions never mean what they did
    this.bytes = 0;
    this.evicted = 0;
    this.skipped = 0;
    this.lastSkip = undefined;
    this.received = 0;
    this.held = [];
    this.heldHead = 0;
    this.heldBytes = 0;
    this.heldCount = 0;
    this.heldLost = 0;
    this.errorEntries = [];
    this.errorsHead = 0;
    this.notesKept = 0;
    this.seen.clear();
    this.undecided.clear();
    this.kept = this.pattern || this.filter.levels.size > 0 || this.filter.label ? [] : undefined;
    this.keptHead = 0;
    this.keptBase = 0;
    this.epoch++;
    this.bump();
  }

  /** How many entries show: what the filter keeps. */
  get length(): number {
    return this.kept ? this.kept.length - this.keptHead : this.entries.length - this.head;
  }

  get start(): number {
    return this.kept ? this.keptBase : this.base;
  }

  /** How many entries there are, whatever the filter keeps. */
  get total(): number {
    return this.entries.length - this.head;
  }

  /** How many have come that don't show yet, while what shows holds still. */
  get arrived(): number {
    return this.holding ? this.heldCount : 0;
  }

  /** How many of what shows are lines, not notes. */
  get matches(): number {
    return this.kept ? this.length - this.notesKept : this.total;
  }

  /** How many errors there are, a stack trace counting as one. */
  get errors(): number {
    return this.errorEntries.length - this.errorsHead;
  }

  /** The first line of an error, by its number from the oldest. */
  errorAt(index: number): Entry | undefined {
    return this.errorEntries[this.errorsHead + index];
  }

  /**
   * Holds what shows still, or lets it move again with what came meanwhile. Held,
   * nothing leaves, so the line a reader is on stays theirs at any rate of lines.
   */
  setHolding(on: boolean): void {
    if (on === this.holding) return;
    this.holding = on;
    if (on) {
      this.heldSince = Date.now();
      this.heldCount = 0;
      this.bump();
      return;
    }
    const batch = this.held.slice(this.heldHead);
    if (this.heldLost > 0) {
      // Where what left was: before the first of what stayed.
      const left = this.entry("", this.lastHeldTime(batch), { left: this.heldLost });
      left.nanos -= 1;
      batch.unshift(left);
    }
    this.held = [];
    this.heldHead = 0;
    this.heldBytes = 0;
    this.heldCount = 0;
    this.heldLost = 0;
    if (batch.length > 0) this.insert(batch);
    else this.bump();
  }

  private lastHeldTime(batch: readonly Entry[]): ApiLine["time"] {
    const first = batch[0];
    if (!first) return undefined;
    return { $typeName: "google.protobuf.Timestamp", seconds: BigInt(Math.floor(first.ms / 1000)), nanos: first.nanos };
  }

  /** The labels of the sources with lines here. */
  labels(): string[] {
    return [...new Set([...this.seen].map((id) => this.labelOf(id)))].filter(Boolean).sort();
  }

  at(position: number): LogLine | undefined {
    const entry = this.entryAt(position);
    if (!entry) return undefined;
    if (entry.drawnAt !== this.words || !entry.drawn) {
      entry.drawn = this.drawLine(entry);
      entry.drawnAt = this.words;
    }
    return entry.drawn;
  }

  entryAt(position: number): Entry | undefined {
    if (position < this.start || position >= this.start + this.length) return undefined;
    if (this.kept) {
      const absolute = this.kept[position - this.keptBase + this.keptHead];
      return absolute === undefined ? undefined : this.entries[absolute - this.base + this.head];
    }
    return this.entries[position - this.base + this.head];
  }

  positionOf(id: string | number): number | undefined {
    // Near the end is where a reader mostly is, and it is ordered by time, so a
    // search would need the entry's time; going through a screenful is rare.
    for (let i = this.entries.length - 1; i >= this.head; i--) {
      if (this.entries[i]?.id === id) return this.positionAt(i);
    }
    return undefined;
  }

  /** Where an entry is, in what shows, if it does, in O(log n) as its time says. */
  positionOfEntry(entry: Entry): number | undefined {
    let low = this.head;
    let high = this.entries.length;
    while (low < high) {
      const mid = (low + high) >> 1;
      if (compare(this.entries[mid] as Entry, entry) < 0) low = mid + 1;
      else high = mid;
    }
    return this.entries[low] === entry ? this.positionAt(low) : undefined;
  }

  private positionAt(index: number): number | undefined {
    const absolute = index - this.head + this.base;
    if (!this.kept) return absolute;
    let low = this.keptHead;
    let high = this.kept.length;
    while (low < high) {
      const mid = (low + high) >> 1;
      if ((this.kept[mid] as number) < absolute) low = mid + 1;
      else high = mid;
    }
    return this.kept[low] === absolute ? low - this.keptHead + this.keptBase : undefined;
  }

  /** The next line that shows after a position, going by, or undefined if there is none. */
  lineFrom(position: number, by: 1 | -1): number | undefined {
    for (let p = position; p >= this.start && p < this.start + this.length; p += by) {
      const e = this.entryAt(p);
      if (e && !e.gap && !e.started && e.left === undefined) return p;
    }
    return undefined;
  }

  /** Takes in lines from the API, as they came. */
  add(chunks: readonly LogChunk[]): void {
    const batch: Entry[] = [];
    for (const chunk of chunks) this.take(chunk, batch);
    if (this.holding) this.setAside(batch);
    else this.insert(batch);
  }

  /** Keeps the newest of what comes while what shows holds still, and counts it all. */
  private setAside(batch: readonly Entry[]): void {
    if (batch.length === 0) return;
    for (const e of batch) {
      this.held.push(e);
      this.heldBytes += e.text.length;
      if (!e.gap && !e.started) this.heldCount++;
    }
    while (this.held.length - this.heldHead > MAX_HELD || this.heldBytes > MAX_HELD_BYTES) {
      const e = this.held[this.heldHead] as Entry;
      this.heldHead++;
      this.heldBytes -= e.text.length;
      if (!e.gap && !e.started) this.heldLost++;
    }
    if (this.heldHead > 16_384) {
      this.held = this.held.slice(this.heldHead);
      this.heldHead = 0;
    }
    this.bump();
  }

  /** Takes in a page of older lines, which show whatever else holds still. */
  addOlder(chunks: readonly LogChunk[]): void {
    const batch: Entry[] = [];
    for (const chunk of chunks) this.take(chunk, batch);
    this.insert(batch);
  }

  private take(chunk: LogChunk, batch: Entry[]): void {
    const source = chunk.sourceId;
    const firstOfSource = !this.seen.has(source) && chunk.lines.length > 0;
    if (chunk.gap) {
      // Where the lines it stands for were, which its chunk's first line may be
      // well after.
      const at = chunk.gap.endTime ?? chunk.lines[0]?.time ?? chunk.gap.startTime;
      const marker = this.entry(source, at, { gap: chunk.gap });
      batch.push(marker);
      if (chunk.gap.reason !== LogGapReason.MATCH_LIMIT) {
        this.skipped += Number(chunk.gap.lines);
        if (chunk.gap.lines > 0n) this.lastSkip = marker;
      }
    }
    if (firstOfSource) {
      // Another container of this replica has written here: a deploy. Say where
      // the new one starts, as the source column can't: it is one source.
      const before = [...this.seen];
      this.seen.add(source);
      const time = chunk.lines[0]?.time;
      const starts = this.startsAnother(source, before);
      if (starts) batch.push(this.entry(source, time, { started: true }));
      else if (starts === undefined) this.undecided.set(source, { time, before });
    }
    this.received += chunk.lines.length;
    for (const line of chunk.lines) batch.push(this.lineEntry(source, line));
  }

  private lineEntry(source: string, line: ApiLine): Entry {
    return {
      id: this.nextId++,
      ms: timeMs(line.time),
      nanos: line.time?.nanos ?? 0,
      source,
      level: line.level,
      stream: line.stream,
      text: line.text,
      cut: line.cutBytes,
    };
  }

  private entry(source: string, time: ApiLine["time"], extra: Partial<Entry>): Entry {
    return {
      id: this.nextId++,
      ms: timeMs(time),
      nanos: time?.nanos ?? 0,
      source,
      level: LogLevel.UNSPECIFIED,
      stream: 0 as LogStream,
      text: "",
      cut: 0,
      ...extra,
    };
  }

  private insert(batch: Entry[]): void {
    if (batch.length === 0) return;
    batch.sort(compare);
    const last = this.entries[this.entries.length - 1];
    const first = batch[0] as Entry;
    for (const e of batch) this.bytes += e.text.length;
    if (!last || this.entries.length === this.head || compare(last, first) <= 0) {
      for (const e of batch) {
        this.noteError(e, this.entries[this.entries.length - 1]);
        this.entries.push(e);
        if (this.kept && this.keeps(e)) {
          this.kept.push(this.base + this.entries.length - 1 - this.head);
          if (e.gap || e.started || e.left !== undefined) this.notesKept++;
        }
      }
    } else {
      this.merge(batch);
    }
    this.evict();
    this.bump();
  }

  /** Notes the first line of an error, which what carries it on, as a stack trace's frames do, isn't. */
  private noteError(e: Entry, before: Entry | undefined): void {
    if (e.level !== LogLevel.ERROR || e.gap || e.started) return;
    const carries =
      before !== undefined &&
      before.level === LogLevel.ERROR &&
      before.source === e.source &&
      e.ms - before.ms <= GROUP_MS;
    if (!carries) this.errorEntries.push(e);
  }

  /** Works out the errors again from a place in the log, as lines merged in may change them. */
  private redoErrors(from: number): void {
    const start = this.entries[from];
    if (!start) return;
    while (this.errorEntries.length > this.errorsHead) {
      const last = this.errorEntries[this.errorEntries.length - 1] as Entry;
      if (compare(last, start) < 0) break;
      this.errorEntries.pop();
    }
    for (let i = from; i < this.entries.length; i++) {
      this.noteError(this.entries[i] as Entry, i > this.head ? this.entries[i - 1] : undefined);
    }
  }

  /** Puts lines where their times say, which moves what comes after them. */
  private merge(batch: Entry[]): void {
    let low = this.head;
    let high = this.entries.length;
    const first = batch[0] as Entry;
    while (low < high) {
      const mid = (low + high) >> 1;
      if (compare(this.entries[mid] as Entry, first) <= 0) low = mid + 1;
      else high = mid;
    }
    const tail = this.entries.slice(low);
    this.entries.length = low;
    let t = 0;
    let b = 0;
    while (t < tail.length || b < batch.length) {
      const from = tail[t];
      const by = batch[b];
      if (from && (!by || compare(from, by) <= 0)) {
        this.entries.push(from);
        t++;
      } else if (by) {
        this.entries.push(by);
        b++;
      }
    }
    this.redoErrors(low);
    this.epoch++;
    if (this.kept) this.refilter();
  }

  private evict(): void {
    let gone = 0;
    while (
      this.entries.length - this.head > MAX_LINES ||
      (this.bytes > MAX_BYTES && this.entries.length - this.head > 1)
    ) {
      const e = this.entries[this.head] as Entry;
      this.bytes -= e.text.length;
      this.head++;
      gone++;
      if (!e.gap && !e.started && e.left === undefined) this.evicted++;
      if (this.errorEntries[this.errorsHead] === e) this.errorsHead++;
    }
    if (gone === 0) return;
    this.base += gone;
    if (this.errorsHead > 16_384) {
      this.errorEntries = this.errorEntries.slice(this.errorsHead);
      this.errorsHead = 0;
    }
    if (this.kept) {
      while (this.keptHead < this.kept.length && (this.kept[this.keptHead] as number) < this.base) {
        const e = this.entries[(this.kept[this.keptHead] as number) - this.base + this.head];
        if (e && (e.gap || e.started || e.left !== undefined)) this.notesKept--;
        this.keptHead++;
        this.keptBase++;
      }
      if (this.keptHead > 16_384) {
        this.kept = this.kept.slice(this.keptHead);
        this.keptHead = 0;
      }
    }
    if (this.head > 16_384) {
      this.entries = this.entries.slice(this.head);
      this.head = 0;
    }
  }

  private keeps(e: Entry): boolean {
    // What is missing shows whatever the filters, so a reader always knows.
    if (e.gap || e.left !== undefined) return true;
    const { levels, label } = this.filter;
    if (e.started) return levels.size === 0 && !this.pattern;
    if (levels.size > 0 && !levels.has(shownLevel(e.level))) return false;
    if (label && this.labelOf(e.source) !== label) return false;
    if (this.pattern) {
      this.pattern.lastIndex = 0;
      return this.pattern.test(e.text.includes(ESC) ? plainText(e.text) : e.text);
    }
    return true;
  }

  /** Keeps only what passes, of what is here and what comes next. */
  setFilter(filter: LogFilter): void {
    this.filter = filter;
    this.pattern = filter.query ? new RegExp(escapeRegExp(filter.query), "iu") : undefined;
    this.refilter();
    this.epoch++;
    this.bump();
  }

  private refilter(): void {
    const { levels, label } = this.filter;
    this.notesKept = 0;
    if (levels.size === 0 && !label && !this.pattern) {
      this.kept = undefined;
      this.keptHead = 0;
      this.keptBase = 0;
      return;
    }
    const kept: number[] = [];
    for (let i = this.head; i < this.entries.length; i++) {
      const e = this.entries[i] as Entry;
      if (this.keeps(e)) {
        kept.push(i - this.head + this.base);
        if (e.gap || e.started || e.left !== undefined) this.notesKept++;
      }
    }
    this.kept = kept;
    this.keptHead = 0;
    this.keptBase = 0;
  }

  /** What shows, as text, for copying and downloading. */
  text(draw: (entry: Entry) => string): string {
    const out: string[] = [];
    for (let p = this.start; p < this.start + this.length; p++) {
      const e = this.entryAt(p);
      if (e) out.push(draw(e));
    }
    return out.length > 0 ? `${out.join("\n")}\n` : "";
  }

  /** The oldest line here, for reading what comes before it. */
  oldest(): Entry | undefined {
    for (let i = this.head; i < this.entries.length; i++) {
      const e = this.entries[i] as Entry;
      if (!e.gap && !e.started && e.left === undefined) return e;
    }
    return undefined;
  }
}

function timeMs(t: ApiLine["time"]): number {
  return t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : 0;
}
