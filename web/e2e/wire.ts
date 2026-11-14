// SPDX-License-Identifier: AGPL-3.0-only

// Just enough of the protobuf wire format to answer the built panel by hand, for what
// the fake Docker Engine can't make happen.

const varint = (n: number) => {
  const out: number[] = [];
  let left = n;
  for (; left > 127; left = Math.floor(left / 128)) out.push((left % 128) | 128);
  out.push(left);
  return Buffer.from(out);
};

/** A field of bytes, a string or a message. */
export function bytes(field: number, value: Buffer | string): Buffer {
  const body = Buffer.from(value);
  // A tag is a varint too: one byte up to field 15, more past it.
  return Buffer.concat([varint((field << 3) | 2), varint(body.length), body]);
}

/** A field of an integer, an enum or a boolean. */
export function number(field: number, value: number): Buffer {
  return Buffer.concat([varint(field << 3), varint(value)]);
}
