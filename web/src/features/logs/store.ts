// SPDX-License-Identifier: AGPL-3.0-only

import { type LogLine as ApiLine, type LogChunk, type LogGap, LogLevel, type LogStream } from "@dockwell/api";
import { type LogLine, type LogLines, plainText } from "@dockwell/ui";

/** The most lines a view keeps. Older ones leave, and the view says so. */
export const MAX_LINES = 100_000;
/** The most text a view keeps: a hundred thousand lines of 16 KiB would be far too much. */
export const MAX_BYTES = 64 << 20;

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
  private sources = new Map<string, string>();
  private seen = new Set<string>();
  private replicas = new Map<string, string>();
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
    this.seen.clear();
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
      if (this.entries[i]?.id === id) {
        const absolute = i - this.head + this.base;
        if (!this.kept) return absolute;
        const at = this.kept.indexOf(absolute, this.keptHead);
        return at < 0 ? undefined : at - this.keptHead + this.keptBase;
      }
    }
    return undefined;
  }

  /** Takes in lines from the API, as they came. */
  add(chunks: readonly LogChunk[]): void {
    const batch: Entry[] = [];
    for (const chunk of chunks) this.take(chunk, batch);
    this.insert(batch);
  }

  /** Takes in a page of older lines. */
  addOlder(chunks: readonly LogChunk[]): void {
    this.add(chunks);
  }

  private take(chunk: LogChunk, batch: Entry[]): void {
    const source = chunk.sourceId;
    const firstOfSource = !this.seen.has(source) && chunk.lines.length > 0;
    if (chunk.gap) {
      this.skipped += Number(chunk.gap.lines);
      const at = chunk.lines[0]?.time ?? chunk.gap.endTime ?? chunk.gap.startTime;
      batch.push(this.entry(source, at, { gap: chunk.gap }));
    }
    if (firstOfSource) {
      // Another container of this replica has written here: a deploy. Say where
      // the new one starts, as the source column can't: it is one source.
      const replica = this.replicas.get(source);
      if (replica && [...this.seen].some((id) => this.replicas.get(id) === replica)) {
        batch.push(this.entry(source, chunk.lines[0]?.time, { started: true }));
      }
      this.seen.add(source);
    }
    for (const line of chunk.lines) batch.push(this.lineEntry(source, line));
  }

  private lineEntry(source: string, line: ApiLine): Entry {
    this.bytes += line.text.length;
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
    if (!last || this.entries.length === this.head || compare(last, first) <= 0) {
      for (const e of batch) {
        this.entries.push(e);
        if (this.kept && this.keeps(e)) this.kept.push(this.base + this.entries.length - 1 - this.head);
      }
    } else {
      this.merge(batch);
    }
    this.evict();
    this.bump();
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
      if (!e.gap && !e.started) this.evicted++;
    }
    if (gone === 0) return;
    this.base += gone;
    if (this.kept) {
      while (this.keptHead < this.kept.length && (this.kept[this.keptHead] as number) < this.base) {
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
    if (e.gap) return true;
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
    if (levels.size === 0 && !label && !this.pattern) {
      this.kept = undefined;
      this.keptHead = 0;
      this.keptBase = 0;
      return;
    }
    const kept: number[] = [];
    for (let i = this.head; i < this.entries.length; i++) {
      if (this.keeps(this.entries[i] as Entry)) kept.push(i - this.head + this.base);
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
      if (!e.gap && !e.started) return e;
    }
    return undefined;
  }
}

function timeMs(t: ApiLine["time"]): number {
  return t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : 0;
}
