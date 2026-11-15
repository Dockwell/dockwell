#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
ENV_FILE="./.env"
if [[ -f "$ENV_FILE" ]]; then
  if grep -q "^PANEL_DB_TYPE=['\"]postgresql['\"]" "$ENV_FILE"; then
    sed -i -E "s/^PANEL_DB_TYPE=['\"]?postgresql['\"]?/PANEL_DB_TYPE='pgsql'/" "$ENV_FILE"
    echo "PANEL_DB_TYPE is pgsql now"
  fi
else
  echo ".env isn't there"
fi
