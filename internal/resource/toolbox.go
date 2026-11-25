// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// ToolboxPlan uses protobuf's JSON mapping for the operation's oneof when
// revision stores a plan as JSON.
type ToolboxPlan v1.HostToolboxPlan

// Proto returns the wire form of the recorded plan.
func (p *ToolboxPlan) Proto() *v1.HostToolboxPlan { return (*v1.HostToolboxPlan)(p) }

// MarshalJSON encodes the selected operation for the revision store.
func (p *ToolboxPlan) MarshalJSON() ([]byte, error) { return protojson.Marshal(p.Proto()) }

// UnmarshalJSON restores the selected operation from an existing plan.
func (p *ToolboxPlan) UnmarshalJSON(raw []byte) error { return protojson.Unmarshal(raw, p.Proto()) }

// ToolboxNeedsKeep reports host settings that could block the agent's next
// connection to the control server.
func ToolboxNeedsKeep(op *v1.HostToolboxOperation) bool {
	switch op.GetOperation().(type) {
	case *v1.HostToolboxOperation_Hostname, *v1.HostToolboxOperation_Dns, *v1.HostToolboxOperation_Hosts:
		return true
	default:
		return false
	}
}

// ToolboxSensitiveEnv reports names interpreted by loaders, shells and runtimes.
func ToolboxSensitiveEnv(name string) bool {
	switch name {
	case "HOME", "PATH", "SHELL", "ENV", "BASH_ENV", "SHELLOPTS", "BASHOPTS", "GCONV_PATH", "GLIBC_TUNABLES",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "TMPDIR", "XDG_CONFIG_HOME",
		"LOCPATH", "NLSPATH", "IFS", "CDPATH", "RUBYOPT", "RUBYLIB", "GEM_HOME", "GEM_PATH",
		"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS", "CLASSPATH", "LUA_PATH", "LUA_CPATH",
		"PHPRC", "PHP_INI_SCAN_DIR", "DOTNET_STARTUP_HOOKS", "ERL_FLAGS", "ERL_AFLAGS", "ERL_ZFLAGS":
		return true
	default:
	}
	for _, prefix := range []string{"LD_", "DYLD_", "GIT_", "MALLOC_", "NODE_", "PYTHON", "PERL", "BASH_FUNC_", "LUA_INIT", "LUA_PATH_", "LUA_CPATH_", "BUNDLE_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return strings.HasSuffix(name, "_OPTIONS") || strings.HasSuffix(name, "_CA_BUNDLE") ||
		strings.HasSuffix(name, "_PROXY") || strings.HasSuffix(name, "_proxy")
}

// ToolboxLiteralKey names the verification key in the sealed apply payload.
// The colon keeps it outside the namespace of service environment names.
func ToolboxLiteralKey(name string) string { return "toolbox-key:" + name }

// ToolboxLiteralDigest pins a literal without publishing a password verifier.
// The random per-value key travels separately from the reviewed plan.
func ToolboxLiteralDigest(key, value string) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}
