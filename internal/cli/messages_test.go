// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// verbs are a message's format verbs, which stand for words.
var verbs = regexp.MustCompile(`%[-+# 0]*\d*[a-zA-Z]`)

// TestTheCLIsErrorsStateTheFactFirst reads every error the API commands
// write themselves, in api.go and internal/cli/apicli, and those each build
// writes about what it has, in the edition files, and checks that it
// follows the content rules (design/system/README.md): a whole sentence
// that starts with the fact, lowercase as Go errors are, with no
// exclamation, and with its consequence or its fix after it. A message
// may leave those to what wraps it only when it wraps a cause (%w), is a
// value one flag refused, which the flag's error completes with the flag
// and where its help is, or is for whoever builds dockwell, as "run make
// generate" is.
func TestTheCLIsErrorsStateTheFactFirst(t *testing.T) {
	files, err := filepath.Glob("apicli/*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "api.go", "edition_full.go", "edition_agent.go")
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 || !isErrorCall(call) {
				return true
			}
			msg, ok := literal(call.Args[0])
			if !ok {
				return true // a message made elsewhere, such as the API's
			}
			checked++
			if problem := messageProblem(msg); problem != "" {
				t.Errorf("%s: %q %s", fset.Position(call.Pos()), msg, problem)
			}
			return true
		})
	}
	if checked < 50 {
		t.Fatalf("checked %d messages; the test no longer finds them", checked)
	}
}

func isErrorCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && (pkg.Name == "errors" && sel.Sel.Name == "New" || pkg.Name == "fmt" && sel.Sel.Name == "Errorf")
}

// literal is a string constant, joined where it is split with +.
func literal(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		x, ok := literal(v.X)
		if !ok {
			return "", false
		}
		y, ok := literal(v.Y)
		return x + y, ok
	}
	return "", false
}

// messageProblem says what breaks the content rules in msg, if anything.
func messageProblem(msg string) string {
	first, _, _ := strings.Cut(msg, ". ")
	whole := verbs.MatchString(first) && verbs.FindString(first) == first || // a sentence made elsewhere
		strings.Contains(strings.TrimSpace(verbs.ReplaceAllString(first, "x")), " ")
	switch {
	case strings.Contains(msg, "!"):
		return "has an exclamation mark"
	case strings.HasSuffix(msg, "."):
		return "ends with a period, which Go errors don't"
	case strings.ToUpper(msg[:1]) == msg[:1] && msg[0] != '%' && msg[0] != '-':
		return "starts with a capital, which Go errors don't"
	case !whole:
		return "starts with a fragment, not a whole sentence"
	case msg == "%s: %w" || strings.HasSuffix(msg, ": %w"), // wraps its cause
		strings.HasPrefix(msg, "%q isn't"),     // a flag's value
		strings.Contains(msg, "make generate"): // for whoever builds dockwell
		return ""
	case strings.Contains(msg, ". "), regexp.MustCompile(`%s [A-Z]`).MatchString(msg):
		return "" // the fix, after the fact
	case strings.Contains(msg, ", so "):
		return "" // the consequence, when there's nothing to fix
	}
	return "states a fact with no consequence or fix after it"
}

// The check itself catches what it is for.
func TestMessageProblem(t *testing.T) {
	for msg, bad := range map[string]bool{
		"stopped":                               true,
		"standard input ended before an answer": true,
		"the control plane returned a change that isn't planned": true,
		"Something went wrong": true,
		"%s is planned. Apply it with dockwell changes apply %s":     false,
		"someone discarded %s before it applied, so nothing changed": false,
		"can't read %s: %w":       false,
		"%q isn't a whole number": false,
	} {
		if got := messageProblem(msg) != ""; got != bad {
			t.Errorf("messageProblem(%q) = %q, want a problem: %v", msg, messageProblem(msg), bad)
		}
	}
}
