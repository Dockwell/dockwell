# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# lobechat's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_lobechat first, once it's installed,
# and check_lobechat upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[lobechat]="1.143.2 lobehub/lobe-chat:1.143.2@sha256:65793dfe2378eecea89c335c01cb1094c240eb67047902859140e0f0b1a15069"

# What a person gives its install, as dockwell catalog install takes it.
given[lobechat]="OPENAI_API_KEY"
given_env[lobechat]="OPENAI_API_KEY=catalog-test-provider-key-never-sent-outside-this-container"

# LobeChat's browser edition stores no chats on the server. Try both versions
# against a local model-list fixture, with no real model key or external call.
check_lobechat() {
  local port c code payload said status
  port=$(value PORT)
  c=$(container lobechat)
  code=$(generated lobechat ACCESS_CODE)
  answers "http://127.0.0.1:$port/"
  docker exec -d "$c" /bin/node -e 'require("node:http").createServer((q,r)=>{r.setHeader("content-type","application/json");r.end(JSON.stringify({object:"list",data:[{id:"gpt-4o-mini",object:"model",created:0,owned_by:"catalog-e2e"}]}));}).listen(19001,"127.0.0.1");' || fail "can't start LobeChat's local provider fixture"
  docker exec "$c" /bin/node -e 'fetch("http://127.0.0.1:19001/v1/models").then(r=>process.exit(r.ok?0:1),()=>process.exit(1));' || fail "LobeChat's local provider fixture doesn't answer"
  payload=$(docker exec -e CATALOG_CODE=not-the-access-code "$c" /bin/node -e 'const b=Buffer.from(JSON.stringify({accessCode:process.env.CATALOG_CODE,baseURL:"http://127.0.0.1:19001/v1"})),k=Buffer.from("LobeHub · LobeHub"); for(let i=0;i<b.length;i++)b[i]^=k[i%k.length]; console.log(b.toString("base64"));') || fail "can't build LobeChat's browser auth header"
  status=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code}' -H "X-lobe-chat-auth: $payload" "http://127.0.0.1:$port/webapi/models/openai") || fail "LobeChat's model route doesn't answer"
  [[ "$status" == 401 ]] || fail "LobeChat answers $status to a wrong access code, want 401"
  payload=$(docker exec -e CATALOG_CODE="$code" "$c" /bin/node -e 'const b=Buffer.from(JSON.stringify({accessCode:process.env.CATALOG_CODE,baseURL:"http://127.0.0.1:19001/v1"})),k=Buffer.from("LobeHub · LobeHub"); for(let i=0;i<b.length;i++)b[i]^=k[i%k.length]; console.log(b.toString("base64"));') || fail "can't build LobeChat's browser auth header"
  said=$(curl -fsS --max-time 20 -H "X-lobe-chat-auth: $payload" "http://127.0.0.1:$port/webapi/models/openai") || fail "LobeChat doesn't take its generated access code"
  jq -e 'any(.id == "gpt-4o-mini")' <<<"$said" >/dev/null || fail "LobeChat can't list models from its local provider"
}
