// SPDX-License-Identifier: AGPL-3.0-only

import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

let file: File;
let digest = sha256.create();
let hashed = 0;

self.addEventListener("message", async (event: MessageEvent<File | number>) => {
  if (typeof event.data !== "number") {
    file = event.data;
    hashed = 0;
    digest = sha256.create();
    return;
  }
  try {
    const end = event.data;
    if (!file || end < hashed || end > file.size) throw new Error();
    while (hashed < end) {
      const next = Math.min(end, hashed + (1 << 20));
      digest.update(new Uint8Array(await file.slice(hashed, next).arrayBuffer()));
      hashed = next;
    }
    self.postMessage(bytesToHex(digest.clone().digest()));
  } catch {
    self.postMessage(null);
  }
});
