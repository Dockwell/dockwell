#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fails when a source file is missing its license header, or carries the wrong
# one. The API schema in proto/ and the clients generated from it (gen/ and
# packages/api/) are Apache-2.0; everything else is AGPL-3.0-only. Generated
# code carries the header of the .proto file it comes from. The imported
# design files are exempt. See docs/adr/0001-license-agpl-and-cla.md and
# docs/adr/0012-apache-for-api-schema-and-clients.md.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

agpl="AGPL-3.0-only"
apache="Apache-2.0"
# The Apache-2.0 directories, each with its own copy of the license text.
apache_dirs=(proto gen packages/api)

license_for() {
  local dir
  for dir in "${apache_dirs[@]}"; do
    if [[ "$1" == "$dir"/* ]]; then
      echo "$apache"
      return
    fi
  done
  echo "$agpl"
}

wrong=()

while IFS= read -r -d '' file; do
  [[ -f "$file" ]] || continue
  want="$(license_for "$file")"
  if ! head -n 5 "$file" | grep -qF "SPDX-License-Identifier: $want"; then
    wrong+=("$file: needs $want")
  fi
done < <(
  git ls-files -z --cached --others --exclude-standard -- \
    '*.go' '*.proto' '*.ts' '*.tsx' '*.js' '*.mjs' '*.css' '*.sh' 'Makefile' \
    ':!:**/*.gen.ts' ':!:design/**' ':!:**/node_modules/**'
)

# Package manifests declare the same license as their files.
while IFS= read -r -d '' manifest; do
  want="$(license_for "$manifest")"
  if ! grep -qF "\"license\": \"$want\"" "$manifest"; then
    wrong+=("$manifest: needs \"license\": \"$want\"")
  fi
done < <(git ls-files -z -- 'package.json' '**/package.json' ':!:**/node_modules/**')

# Each Apache-2.0 directory carries the same, unmodified license text.
for dir in "${apache_dirs[@]}"; do
  if ! cmp -s proto/LICENSE "$dir/LICENSE" || ! grep -qF "Apache License" "$dir/LICENSE"; then
    wrong+=("$dir/LICENSE: needs the Apache-2.0 text, the same as proto/LICENSE")
  fi
done

if ((${#wrong[@]})); then
  printf '%s\n' "${wrong[@]}"
  cat >&2 <<EOF
${#wrong[@]} file(s) above have the wrong license, or none. Files under proto/, gen/ and packages/api/ are $apache, and everything else is $agpl.
Put "SPDX-License-Identifier: <license>" in a comment in a file's first 5 lines. For generated code, fix the .proto file, then run make generate.
EOF
  exit 1
fi
