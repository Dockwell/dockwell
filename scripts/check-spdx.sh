#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fails when a source file is missing its license header. Generated code and
# the imported design files are exempt. See docs/adr/0001-license-agpl-and-cla.md.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

want="SPDX-License-Identifier: AGPL-3.0-only"
missing=()

while IFS= read -r -d '' file; do
  [[ -f "$file" ]] || continue
  if ! head -n 5 "$file" | grep -qF "$want"; then
    missing+=("$file")
  fi
done < <(
  git ls-files -z --cached --others --exclude-standard -- \
    '*.go' '*.proto' '*.ts' '*.tsx' '*.js' '*.mjs' '*.css' '*.sh' 'Makefile' \
    ':!:gen/**' ':!:packages/api/src/gen/**' ':!:design/**' ':!:**/node_modules/**'
)

if ((${#missing[@]})); then
  printf '%s\n' "${missing[@]}"
  echo "${#missing[@]} file(s) above lack \"$want\" in their first 5 lines. Add it as a comment at the top." >&2
  exit 1
fi
