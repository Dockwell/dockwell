# SPDX-License-Identifier: AGPL-3.0-only
#
# Prints how many servers are connected now, from a control plane's log in
# JSON (--log-format json): each server's last "server connected" or "server
# disconnected" line says whether it is.
#
#   awk -f scripts/connected.awk control.log
#
# The API would say so too, but it needs a session, and a session needs a
# passkey, which a script doesn't have. Connections live only in the running
# control plane's memory, so there is nothing else local to read. The
# scripts that wait for agents (budgets.sh and install-test/run.sh) use this,
# and TestConnectionsCanBeCountedFromTheLog in internal/control/agentapi runs
# it on a real log, so renaming either message or its "server" attribute
# fails that test.
function server(s) {
	s = $0
	sub(/.*"server":"/, "", s)
	sub(/".*/, "", s)
	return s
}
/"msg":"server connected"/ { up[server()] = 1 }
/"msg":"server disconnected"/ { up[server()] = 0 }
END {
	n = 0
	for (s in up) n += up[s]
	print n
}
