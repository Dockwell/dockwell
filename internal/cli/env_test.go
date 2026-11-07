// SPDX-License-Identifier: AGPL-3.0-only

package cli

// quiet keeps an agent a test starts from asking the world how it's reached: what a
// real STUN server or metadata service said would depend on where the test ran. A
// test that means to say either sets it in getenv.
func quiet(getenv func(string) string) func(string) string {
	return func(k string) string {
		if v := getenv(k); v != "" {
			return v
		}
		if k == envTest {
			return "1"
		}
		return ""
	}
}

// noEnv is an empty environment, but for quiet.
var noEnv = quiet(func(string) string { return "" })
