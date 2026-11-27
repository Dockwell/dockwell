#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# File loads shared by transfers-load and small-host. Sourcing it lets the former
# use its own fixture and error reporting.

# api calls the REST API as the panel does, signed in.
api() {
  local verb=$1 path=$2
  shift 2
  curl -sS -X "$verb" -H "Cookie: $cookie" -H "Origin: $origin" -H "Sec-Fetch-Site: same-origin" "$@" "$origin$path"
}
# upload sends the source to $1 in 64 MiB parts, each from where the last ended. A
# part the server had no place for goes again, as the panel's does.
upload() {
  local to=$1 began upload offset=0 part=$((64 << 20)) code
  began=$(api POST "/v1/servers/$id/uploads" -H "Content-Type: application/json" -d "{\"path\":\"$to\",\"sizeBytes\":\"$size\"}")
  upload=$(jq -r .upload.uploadId <<<"$began")
  [[ -n $upload && $upload != null ]] || fail "BeginUpload for $to: $began"
  local rate=()
  if [[ -n ${FILE_LOAD_RATE:-} ]]; then
    rate=(--limit-rate "$FILE_LOAD_RATE")
    touch "$to.started"
  fi
  while ((offset < size)); do
    code=$(dd if="$source_file" bs=1M skip=$((offset >> 20)) count=$((part >> 20)) status=none |
      api PUT "/v1/servers/$id/uploads/$upload?offset=$offset" "${rate[@]}" -H "Content-Type: application/octet-stream" --data-binary @- -o "$to.put" -w '%{http_code}')
    case $code in
    200) offset=$((offset + part < size ? offset + part : size)) ;;
    429) sleep 2 ;;
    *) fail "PUT $to at $offset: $code $(cat "$to.put")" ;;
    esac
  done
  api POST "/v1/servers/$id/uploads/$upload:finish" -H "Content-Type: application/json" -d '{}' >"$to.finish"
  jq -e .path "$to.finish" >/dev/null || fail "FinishUpload for $to: $(cat "$to.finish")"
}

# download reads $1 and prints what it holds, as a checksum.
download() {
  local code
  while :; do
    api GET "/v1/servers/$id/files:download?path=$1" -D "$2.headers" | sha256sum | cut -d' ' -f1 >"$2" ||
      fail "GET $1 was cut short"
    code=$(awk 'NR == 1 { print $2 }' "$2.headers")
    [[ $code != 429 ]] && break
    sleep 2
  done
  [[ $code == 200 ]] || fail "GET $1: $code"
}


if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  set -euo pipefail
  origin=$1 cookie=$(<"$2") id=$3 source_file=$4
  size=$(stat -c %s "$source_file")
  fail() { echo "$*" >&2; exit 1; }
  upload "$5"
  cmp -s "$source_file" "$5" || fail "what landed differs from what was sent"
fi
