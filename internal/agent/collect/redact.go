// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/lazyre"
)

// Hidden stands in for a secret a command line held, as the process list shows it.
const Hidden = "…"

// secretFlags are flags whose value is a credential, as cloudflared's connector
// token, which holds its tunnel's secret: "cloudflared service install <token>"
// writes a unit that runs "cloudflared tunnel run --token <token>" (ADR 0104).
var secretFlags = []string{"--token", "--credentials-contents"}

// secretInArg finds such a flag's value inside one argument, as a shell's -c
// string holds a whole command line.
var secretInArg = lazyre.Regexp(`((?:^|\s)--?(?:token|credentials-contents)(?:=|\s+))(?:"[^"]*"|'[^']*'|\S+)`)

// installInArg finds the token "cloudflared service install" takes as an
// argument of its own, inside a shell's -c string.
var installInArg = lazyre.Regexp(`(cloudflared\S*\s+service\s+install\s+)(?:"[^"]*"|'[^']*'|\S+)`)

// redact hides the value of every secret flag in a command line's arguments, apart
// or joined with "=", or inside an argument of its own, before the line leaves the
// server. Only its start is read, so a value cut off there is hidden too.
func redact(args []string) []string {
	out := make([]string, len(args))
	hideNext := false
	for i, arg := range args {
		switch {
		case hideNext:
			out[i], hideNext = Hidden, false
			continue
		case isSecretFlag(arg):
			out[i], hideNext = arg, true
			continue
		}
		if flag, _, ok := strings.Cut(arg, "="); ok && isSecretFlag(flag) {
			out[i] = flag + "=" + Hidden
			continue
		}
		out[i] = installInArg().ReplaceAllString(secretInArg().ReplaceAllString(arg, "${1}"+Hidden), "${1}"+Hidden)
	}
	// "cloudflared service install <token>" takes the token with no flag.
	for i := 2; i+1 < len(out); i++ {
		if out[i] == "install" && out[i-1] == "service" && slices.ContainsFunc(out[:i-1], isCloudflared) {
			out[i+1] = Hidden
		}
	}
	return out
}

func isCloudflared(arg string) bool { return strings.Contains(arg, "cloudflared") }

func isSecretFlag(arg string) bool {
	for _, f := range secretFlags {
		// cloudflared takes a single dash as well as two.
		if arg == f || arg == f[1:] {
			return true
		}
	}
	return false
}
