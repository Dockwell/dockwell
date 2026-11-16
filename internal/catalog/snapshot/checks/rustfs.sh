# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# rustfs's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_rustfs first, once it's installed,
# and check_rustfs upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[rustfs]="1.0.2 rustfs/rustfs:1.0.2@sha256:7e948dad4748bda48f8985fda7195f8cf2e4259427011f6120acc2c9acf51587"

check_rustfs() {
  local port console key secret said
  port=$(value S3_PORT)
  console=$(value CONSOLE_PORT)
  key=$(value ACCESS_KEY)
  secret=$(generated rustfs RUSTFS_SECRET_KEY)
  curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:$console/rustfs/console/health" || fail "RustFS's console doesn't answer on port $console"
  # Named outright, as S3 asks for it and some versions of curl don't send it.
  s3() { curl -fsS --max-time 15 --aws-sigv4 aws:amz:us-east-1:s3 --user "$key:$1" -H 'x-amz-content-sha256: UNSIGNED-PAYLOAD' "${@:2}"; }
  if [[ "$1" == first ]]; then
    s3 "$secret" -X PUT "http://127.0.0.1:$port/e2e-kept" >/dev/null || fail "RustFS doesn't make a bucket with the key Dockwell generated"
    s3 "$secret" -X PUT --data-binary kept "http://127.0.0.1:$port/e2e-kept/marker" >/dev/null || fail "RustFS doesn't take an object"
  fi
  [[ "$(s3 "$secret" "http://127.0.0.1:$port/e2e-kept/marker")" == kept ]] || fail "RustFS doesn't give back the object it took"
  said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' --aws-sigv4 aws:amz:us-east-1:s3 --user "$key:not-the-secret-key-at-all" \
    -H 'x-amz-content-sha256: UNSIGNED-PAYLOAD' "http://127.0.0.1:$port/e2e-kept/marker") || fail "RustFS got no answer"
  [[ "$said" == 403 ]] || fail "RustFS answered $said to a wrong secret key, want 403"
}
