#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fails when a file carries the wrong license, or none:
# - each source file has exactly one SPDX-License-Identifier in its first 5
#   lines, and it names the license of the file's directory;
# - each package.json declares that same license;
# - each Apache-2.0 directory has a LICENSE file with the canonical text.
# The API schema in proto/ and the clients generated from it (gen/ and
# packages/api/) are Apache-2.0; everything else is AGPL-3.0-only. Generated
# code carries the header of the .proto file it comes from. The TypeScript
# generated from googleapis (packages/api/src/gen/google/) also keeps
# Google's own Apache-2.0 notice, below the SPDX line make generate adds,
# which is checked for. The imported design files are exempt. See
# docs/adr/0001-license-agpl-and-cla.md and
# docs/adr/0012-apache-for-api-schema-and-clients.md.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

agpl="AGPL-3.0-only"
apache="Apache-2.0"
# The Apache-2.0 directories. license_for matches the same list.
apache_dirs=(proto gen packages/api)
# SHA-256 of the canonical Apache License 2.0 text, the file published at
# https://www.apache.org/licenses/LICENSE-2.0.txt (202 lines). Each LICENSE in
# apache_dirs must be that file, byte for byte.
apache_url="https://www.apache.org/licenses/LICENSE-2.0.txt"
apache_sha256="cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"

# Sets want to the license of the path in $1.
license_for() {
  case $1 in
    proto/* | gen/* | packages/api/*) want=$apache ;;
    *) want=$agpl ;;
  esac
}

# Lists files git tracks, and untracked files it doesn't ignore.
list_files() {
  git ls-files -z --cached --others --exclude-standard -- "$@" ':!:**/node_modules/**'
}

headers=()
while IFS= read -r -d '' file; do
  [[ -f "$file" ]] || continue
  license_for "$file"
  found=()
  mapfile -t -n 5 lines <"$file"
  for line in "${lines[@]}"; do
    [[ $line =~ SPDX-License-Identifier:[[:space:]]*(.*[^[:space:]]) ]] || continue
    id=${BASH_REMATCH[1]}
    # The end of a /* */ comment on the same line isn't part of the value.
    [[ $id =~ ^(.*[^[:space:]])[[:space:]]*\*/$ ]] && id=${BASH_REMATCH[1]}
    found+=("$id")
  done
  if ((${#found[@]} == 0)); then
    headers+=("$file: no SPDX-License-Identifier, needs $want")
  elif ((${#found[@]} > 1)); then
    headers+=("$file: ${#found[@]} SPDX-License-Identifier lines, needs one, $want")
  elif [[ ${found[0]} != "$want" ]]; then
    headers+=("$file: ${found[0]}, needs $want")
  fi
done < <(
  list_files '*.go' '*.proto' '*.ts' '*.tsx' '*.js' '*.mjs' '*.css' '*.sh' 'Makefile' \
    ':!:**/*.gen.ts' ':!:design/**'
)

# Code generated from googleapis, which the TypeScript client imports, also
# keeps Google's Apache-2.0 notice from the .proto file, below its SPDX line.
# The files are picked out here: git can list none for a pathspec naming
# their directory beside the exclusion in list_files.
googleapis=packages/api/src/gen/google/
googleapis_files=0
while IFS= read -r -d '' file; do
  [[ -f "$file" && $file == "$googleapis"* ]] || continue
  googleapis_files=$((googleapis_files + 1))
  mapfile -t -n 15 lines <"$file"
  notice=no
  for line in "${lines[@]}"; do
    [[ $line == "// Copyright "*" Google LLC" ]] && notice=yes
  done
  if ((${#lines[@]} == 0)); then
    headers+=("$file: empty, needs the code make generate writes from googleapis, with Google's Apache-2.0 notice")
  elif [[ $notice == no || "${lines[*]}" != *"Licensed under the Apache License, Version 2.0"* ]]; then
    headers+=("$file: no Google Apache-2.0 notice in its first 15 lines, needs one, as generated from googleapis")
  fi
done < <(list_files '*.ts')
if ((googleapis_files == 0)); then
  headers+=("$googleapis: no files, needs the TypeScript make generate writes from googleapis")
fi

# Package manifests declare the license of their directory. Node reads them as
# JSON, so the field's spelling and position don't matter.
manifest_args=()
while IFS= read -r -d '' manifest; do
  [[ -f "$manifest" ]] || continue
  license_for "$manifest"
  manifest_args+=("$manifest" "$want")
done < <(list_files 'package.json' '**/package.json')
manifests=()
if ((${#manifest_args[@]})); then
  if ! command -v node >/dev/null; then
    echo "Node.js isn't installed, so the package.json licenses can't be checked. Install Node.js 24, as CONTRIBUTING.md says." >&2
    exit 1
  fi
  # The ${…} below are JavaScript template literals, which the shell must leave alone.
  # shellcheck disable=SC2016
  if ! out=$(node -e '
    const fs = require("node:fs");
    const args = process.argv.slice(1);
    for (let i = 0; i < args.length; i += 2) {
      const [file, want] = [args[i], args[i + 1]];
      let license;
      try {
        license = JSON.parse(fs.readFileSync(file, "utf8")).license;
      } catch (err) {
        console.log(`${file}: not valid JSON (${err.message}), needs "license": "${want}"`);
        continue;
      }
      if (license === undefined) {
        console.log(`${file}: no "license" field, needs "license": "${want}"`);
      } else if (license !== want) {
        console.log(`${file}: "license": ${JSON.stringify(license)}, needs "license": "${want}"`);
      }
    }
  ' -- "${manifest_args[@]}"); then
    echo "Node.js stopped with the error above, so the package.json licenses weren't checked. Fix what it names, then run the script again." >&2
    exit 1
  fi
  [[ -z $out ]] || mapfile -t manifests <<<"$out"
fi

# Each Apache-2.0 directory carries the canonical license text, unmodified.
sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi
}
licenses=()
for dir in "${apache_dirs[@]}"; do
  file=$dir/LICENSE
  if [[ ! -f $file ]]; then
    if [[ $dir == gen ]]; then
      licenses+=("$file: missing. buf generate empties gen/, and only make generate puts the license back.")
    else
      licenses+=("$file: missing")
    fi
    continue
  fi
  sum=$(sha256 "$file")
  if [[ ${sum%% *} != "$apache_sha256" ]]; then
    licenses+=("$file: differs from the Apache License 2.0 text")
  fi
done

if ((${#headers[@]})); then
  printf '%s\n' "${headers[@]}"
  cat >&2 <<EOF
Each source file above needs exactly one license header in its first 5 lines, naming its directory's license: $apache under proto/, gen/ and packages/api/, and $agpl everywhere else. Put "SPDX-License-Identifier: <license>" in a comment there. For generated code, fix the .proto file, then run make generate.
EOF
fi
if ((${#manifests[@]})); then
  printf '%s\n' "${manifests[@]}"
  cat >&2 <<EOF
Each package.json above needs a "license" field that names its directory's license, or npm publishes the package under the wrong one. Set the field to the license its line names.
EOF
fi
if ((${#licenses[@]})); then
  printf '%s\n' "${licenses[@]}"
  cat >&2 <<EOF
Each LICENSE above must be the Apache License 2.0 text, unmodified, or GitHub, npm and Go tooling can't tell that its directory is $apache. Download $apache_url over it, or run make generate for gen/LICENSE.
EOF
fi
if ((${#headers[@]} + ${#manifests[@]} + ${#licenses[@]})); then
  exit 1
fi
