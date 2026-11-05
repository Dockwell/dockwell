// SPDX-License-Identifier: AGPL-3.0-only

/**
 * The height of each row of a long list, which grows at the end and shrinks at
 * the start, as a log does: what a row's position is, and which row is at a
 * position, each in O(log n), and however many rows there are. It is a Fenwick
 * tree over the rows' slots: a row that left has height 0 until the slots run
 * out, when what is left moves to the start of a new tree.
 */
export class HeightIndex {
  /** The position of the first row kept, which only grows until reset. */
  first = 0;
  /** How many rows there are after first. */
  length = 0;
  private origin = 0;
  private capacity: number;
  private tree: Float64Array;
  private values: Float32Array;
  private sum = 0;

  constructor(capacity = 1024) {
    this.capacity = capacity;
    this.tree = new Float64Array(capacity + 1);
    this.values = new Float32Array(capacity);
  }

  get total(): number {
    return this.sum;
  }

  /** Empties the index, with rows counted from first. */
  reset(first = 0): void {
    this.first = first;
    this.length = 0;
    this.origin = first;
    this.sum = 0;
    this.tree.fill(0);
    this.values.fill(0);
  }

  push(height: number): void {
    if (this.first + this.length - this.origin >= this.capacity) this.compact();
    const slot = this.first + this.length - this.origin;
    this.length++;
    this.values[slot] = height;
    this.sum += height;
    this.add(slot, height);
  }

  /** Lets go of the rows before position, and says how tall they were. */
  dropTo(position: number): number {
    const end = Math.min(position, this.first + this.length);
    let dropped = 0;
    for (let p = this.first; p < end; p++) {
      const slot = p - this.origin;
      const h = this.values[slot] ?? 0;
      if (h !== 0) {
        this.add(slot, -h);
        this.values[slot] = 0;
        dropped += h;
      }
    }
    this.length -= Math.max(0, end - this.first);
    this.first = Math.max(this.first, end);
    this.sum -= dropped;
    return dropped;
  }

  get(position: number): number {
    if (position < this.first || position >= this.first + this.length) return 0;
    return this.values[position - this.origin] ?? 0;
  }

  set(position: number, height: number): void {
    if (position < this.first || position >= this.first + this.length) return;
    const slot = position - this.origin;
    const was = this.values[slot] ?? 0;
    if (was === height) return;
    this.values[slot] = height;
    this.sum += height - was;
    this.add(slot, height - was);
  }

  /** How far down the list a row starts. */
  offset(position: number): number {
    let i = Math.min(position - this.origin, this.capacity);
    let out = 0;
    while (i > 0) {
      out += this.tree[i] ?? 0;
      i -= i & -i;
    }
    return Math.max(out, 0);
  }

  /** The row at a distance down the list, within those there are. */
  at(y: number): number {
    if (this.length === 0) return this.first;
    let slot = 0;
    let rest = y;
    let bit = 1 << (31 - Math.clz32(this.capacity));
    while (bit > 0) {
      const next = slot + bit;
      if (next <= this.capacity && (this.tree[next] ?? 0) <= rest) {
        slot = next;
        rest -= this.tree[next] ?? 0;
      }
      bit >>= 1;
    }
    return Math.min(Math.max(slot + this.origin, this.first), this.first + this.length - 1);
  }

  private add(slot: number, delta: number): void {
    const tree = this.tree;
    for (let i = slot + 1; i <= this.capacity; i += i & -i) tree[i] = (tree[i] ?? 0) + delta;
  }

  /** Moves the rows kept to the start of a tree with room for as many again. */
  private compact(): void {
    const { first, length, origin, values } = this;
    const capacity = Math.max(1024, length * 2);
    const kept = values.slice(first - origin, first - origin + length);
    this.capacity = capacity;
    this.tree = new Float64Array(capacity + 1);
    this.values = new Float32Array(capacity);
    this.origin = first;
    this.values.set(kept);
    // Built in O(n): each slot passes its sum to the one its range ends in.
    const tree = this.tree;
    for (let i = 1; i <= length; i++) tree[i] = (tree[i] ?? 0) + (kept[i - 1] ?? 0);
    for (let i = 1; i <= capacity; i++) {
      const parent = i + (i & -i);
      if (parent <= capacity) tree[parent] = (tree[parent] ?? 0) + (tree[i] ?? 0);
    }
  }
}
