# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# openclaw's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_openclaw first, once it's installed,
# and check_openclaw upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[openclaw]="2026.10.6 ghcr.io/openclaw/openclaw:2026.10.6@sha256:7dbff0fe5f5359b1badefe90a23efa9f203cd0d3c36bee095f4896c7a1d3864b"

check_openclaw() {
  local port c token said
  port=$(value PORT)
  c=$(container openclaw)
  token=$(generated openclaw OPENCLAW_GATEWAY_TOKEN)
  answers "http://127.0.0.1:$port/"
  # Use a fresh client identity for the negative check, so a retained paired
  # device's token can't authenticate it in place of the deliberately wrong one.
  if said=$(timeout 35 docker exec -e OPENCLAW_STATE_DIR=/tmp/catalog-wrong-token -e OPENCLAW_GATEWAY_TOKEN=not-the-token \
    "$c" node /app/dist/index.js gateway call health --url ws://127.0.0.1:18789 --token not-the-token --timeout 10000 --json 2>&1); then
    fail "OpenClaw accepts a wrong gateway token"
  fi
  [[ "$said" == *unauthorized* || "$said" == *"token mismatch"* || "$said" == *"AUTH_TOKEN_MISMATCH"* ]] || fail "OpenClaw's wrong-token check failed without an auth refusal: ${said:0:300}"
  said=$(timeout 35 docker exec "$c" node /app/dist/index.js gateway call health --url ws://127.0.0.1:18789 --token "$token" --timeout 10000 --json) || fail "OpenClaw doesn't take its generated gateway token"
  jq -e '.ok' <<<"$said" >/dev/null || fail "OpenClaw's authenticated health isn't ready"
  if [[ "$1" == first ]]; then
    # Agent state is files in its workspace. No paid model or external channel
    # is needed to prove that a real retained workspace survives replacement.
    docker exec "$c" node -e 'const f=require("node:fs"), p=process.env.OPENCLAW_WORKSPACE_DIR; f.mkdirSync(p,{recursive:true}); f.writeFileSync(p+"/catalog-e2e.md","kept-through-upgrade\n");' || fail "OpenClaw can't write its workspace"
    # OpenClaw's config accepts JSON5. A comment retained by its editor must not
    # stop the next image's startup from reading and merging that config.
    docker exec "$c" node -e 'const f=require("node:fs"), p=process.env.OPENCLAW_CONFIG_PATH; f.writeFileSync(p,"// retained through upgrade\n"+f.readFileSync(p,"utf8"));' || fail "OpenClaw can't keep JSON5 config"
  fi
  docker exec "$c" node -e 'const f=require("node:fs"); if(f.readFileSync(process.env.OPENCLAW_WORKSPACE_DIR+"/catalog-e2e.md","utf8")!=="kept-through-upgrade\n")process.exit(1);' || fail "OpenClaw lost its workspace through upgrade"
  said=$(docker exec "$c" node /app/dist/index.js gateway call config.get --url ws://127.0.0.1:18789 --token "$token" --timeout 10000 --json) || fail "OpenClaw can't read its config through the authenticated gateway"
  jq -e --arg port "$port" '.config.gateway.auth.mode == "token" and .config.gateway.mode == "local" and .config.gateway.controlUi.allowedOrigins == ["http://localhost:" + $port, "http://127.0.0.1:" + $port]' <<<"$said" >/dev/null || fail "OpenClaw lost its token gate or explicit control-UI origins"
}
