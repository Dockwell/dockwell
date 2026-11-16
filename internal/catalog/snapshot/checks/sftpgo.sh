# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# sftpgo's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_sftpgo first, once it's installed,
# and check_sftpgo upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[sftpgo]="2.7.5 drakkan/sftpgo:v2.7.5@sha256:9011fe608d336d3daf6ed6224b16fd40443aab8f3335e0b20853d6a539c58738"

check_sftpgo() {
  command -v sshpass >/dev/null || fail "the SFTPGo check needs sshpass, which its CI group installs"
  local port sftp_port base password said token transfer_dir status
  port=$(value PORT)
  sftp_port=$(value SFTP_PORT)
  base="http://127.0.0.1:$port"
  password=$(generated sftpgo SFTPGO_DEFAULT_ADMIN_PASSWORD)
  said=$(curl -fsS --max-time 15 -u "admin:$password" "$base/api/v2/token") || fail "SFTPGo doesn't take its generated admin password"
  token=$(jq -er '.access_token | select(length > 0)' <<<"$said") || fail "SFTPGo's admin sign-in made no token"
  status=$(curl -sS --max-time 15 -u admin:not-the-password -o /dev/null -w '%{http_code}' "$base/api/v2/token") || fail "SFTPGo's sign-in doesn't answer"
  [[ "$status" == 401 ]] || fail "SFTPGo answers $status to a wrong admin password, want 401"
  transfer_dir=$(mktemp -d)
  if [[ "$1" == first ]]; then
    kept[sftpgo-password]="$(openssl rand -hex 24)"
    said=$(curl -fsS --max-time 15 -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg p "${kept[sftpgo-password]}" '{username:"transfer",password:$p,status:1,home_dir:"/srv/sftpgo/data/transfer",permissions:{"/":["*"]}}')" "$base/api/v2/users") || fail "SFTPGo doesn't make a transfer user"
    jq -e '.username == "transfer"' <<<"$said" >/dev/null || fail "SFTPGo made no transfer user"
    kept[sftpgo-hostkey]=$(ssh-keyscan -p "$sftp_port" 127.0.0.1 2>/dev/null) || fail "SFTPGo's SSH listener doesn't answer"
    [[ -n "${kept[sftpgo-hostkey]}" ]] || fail "SFTPGo offered no SSH host key"
    printf '%s\n' 'kept-through-upgrade' >"$transfer_dir/marker"
    printf '%s\n' "${kept[sftpgo-hostkey]}" >"$transfer_dir/known_hosts"
    # -b adds BatchMode=yes, and OpenSSH takes the first value, so the override goes first.
    SSHPASS=${kept[sftpgo-password]} sshpass -e sftp -q -oBatchMode=no -b - -P "$sftp_port" \
      -oStrictHostKeyChecking=yes -oConnectTimeout=10 -oUserKnownHostsFile="$transfer_dir/known_hosts" transfer@127.0.0.1 <<EOF || fail "SFTPGo doesn't accept an SFTP upload"
put $transfer_dir/marker marker
EOF
  fi
  # Keep trusting the original host key after the container and image change too.
  printf '%s\n' "${kept[sftpgo-hostkey]}" >"$transfer_dir/known_hosts"
  SSHPASS=${kept[sftpgo-password]} sshpass -e sftp -q -oBatchMode=no -b - -P "$sftp_port" \
    -oStrictHostKeyChecking=yes -oConnectTimeout=10 -oUserKnownHostsFile="$transfer_dir/known_hosts" transfer@127.0.0.1 <<EOF || fail "SFTPGo doesn't return the uploaded file with its original host key"
get marker $transfer_dir/read-back
EOF
  [[ "$(cat "$transfer_dir/read-back")" == kept-through-upgrade ]] || fail "SFTPGo lost its uploaded file through upgrade"
  rm -rf "$transfer_dir"
}
