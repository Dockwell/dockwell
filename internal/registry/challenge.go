// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"errors"
	"strings"
)

// challenge is one of what a registry's WWW-Authenticate headers ask of a client:
// its scheme, lowercase, and its parameters, their names lowercase.
type challenge struct {
	scheme string
	params map[string]string
}

// errUnreadChallenge is a challenge that can't be read as RFC 9110 has them, as a
// client that reads it another way could send a login where Dockwell can't see.
var errUnreadChallenge = errors.New("a challenge that can't be read")

// parseChallenges reads every challenge in every value of a WWW-Authenticate
// header, as Docker does: a value may hold several, separated by commas, and a
// scheme is a token followed by any whitespace. What can't be read as that is an
// error, so nothing is decided on what was read of it.
func parseChallenges(values []string) ([]challenge, error) {
	var out []challenge
	for _, v := range values {
		got, err := parseChallengeValue(v)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func parseChallengeValue(s string) ([]challenge, error) {
	var out []challenge
	i := 0
	skip := func(set string) {
		for i < len(s) && strings.IndexByte(set, s[i]) >= 0 {
			i++
		}
	}
	const space = " \t"
	for {
		skip(space + ",")
		if i >= len(s) {
			return out, nil
		}
		name := readToken(s, &i)
		if name == "" {
			return nil, errUnreadChallenge
		}
		skip(space)
		if i < len(s) && s[i] == '=' {
			// A parameter of the challenge before it.
			if len(out) == 0 {
				return nil, errUnreadChallenge
			}
			i++
			skip(space)
			value, ok := readValue(s, &i)
			if !ok {
				return nil, errUnreadChallenge
			}
			key := strings.ToLower(name)
			if _, twice := out[len(out)-1].params[key]; twice {
				// Clients differ in which of two they take.
				return nil, errUnreadChallenge
			}
			out[len(out)-1].params[key] = value
			skip(space)
			if i < len(s) && s[i] != ',' {
				return nil, errUnreadChallenge
			}
			continue
		}
		c := challenge{scheme: strings.ToLower(name), params: map[string]string{}}
		// A token68, as Negotiate's, in place of parameters: kept as it is, under "".
		if j := i; j < len(s) && s[j] != ',' {
			if t := readToken68(s, &j); t != "" && (j >= len(s) || s[j] == ',' || strings.IndexByte(space, s[j]) >= 0) {
				rest := strings.TrimLeft(s[j:], space)
				if rest == "" || rest[0] == ',' {
					c.params[""] = t
					i = j
				}
			}
		}
		out = append(out, c)
	}
}

// readToken reads an RFC 9110 token.
func readToken(s string, i *int) string {
	start := *i
	for *i < len(s) && isTokenChar(s[*i]) {
		*i++
	}
	return s[start:*i]
}

// readToken68 reads a token68: what a scheme carries in place of parameters.
func readToken68(s string, i *int) string {
	start := *i
	for *i < len(s) && (isAlnum(s[*i]) || strings.IndexByte("-._~+/", s[*i]) >= 0) {
		*i++
	}
	for *i < len(s) && s[*i] == '=' {
		*i++
	}
	return s[start:*i]
}

// readValue reads a parameter's value: a token, or a quoted string with its escapes.
func readValue(s string, i *int) (string, bool) {
	if *i < len(s) && s[*i] == '"' {
		*i++
		var b strings.Builder
		for *i < len(s) {
			switch c := s[*i]; c {
			case '\\':
				if *i+1 >= len(s) {
					return "", false
				}
				b.WriteByte(s[*i+1])
				*i += 2
			case '"':
				*i++
				return b.String(), true
			default:
				b.WriteByte(c)
				*i++
			}
		}
		return "", false
	}
	t := readToken(s, i)
	return t, t != ""
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isTokenChar(c byte) bool {
	return isAlnum(c) || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
