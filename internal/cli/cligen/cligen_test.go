// SPDX-License-Identifier: AGPL-3.0-only

package cligen

import (
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	_ "github.com/dockwell/dockwell/gen/dockwell/v1" // the schema, compiled in
)

func TestName(t *testing.T) {
	get := func(path string) *annotations.HttpRule {
		return &annotations.HttpRule{Pattern: &annotations.HttpRule_Get{Get: path}}
	}
	post := func(path string) *annotations.HttpRule {
		return &annotations.HttpRule{Pattern: &annotations.HttpRule_Post{Post: path}}
	}
	del := &annotations.HttpRule{Pattern: &annotations.HttpRule_Delete{Delete: "/v1/passkeys/{id}"}}
	tests := []struct {
		method     string
		rule       *annotations.HttpRule
		stream     bool
		noun, verb string
	}{
		{"ListServers", get("/v1/servers"), false, "servers", "list"},
		{"GetServer", get("/v1/servers/{id}"), false, "servers", "get"},
		{"WatchServers", nil, true, "servers", "watch"},
		{"WatchChange", nil, true, "changes", "watch"},
		{"ProposeChange", post("/v1/changes"), false, "changes", "propose"},
		{"ApplyChange", post("/v1/changes/{id}:apply"), false, "changes", "apply"},
		{"ListDrift", get("/v1/drift"), false, "drift", "list"},
		{"RevertDrift", post("/v1/drift:revert"), false, "drift", "revert"},
		{"GetAuthState", get("/v1/authState"), false, "auth-state", "get"},
		{"BeginPasskeySignIn", post("/v1/sessions:beginPasskeySignIn"), false, "sessions", "begin-passkey-sign-in"},
		{"RevokeOtherSessions", post("/v1/sessions:revokeOthers"), false, "sessions", "revoke-others"},
		{"RemovePasskey", del, false, "passkeys", "remove"},
		{"RegenerateRecoveryCodes", post("/v1/totp:regenerateRecoveryCodes"), false, "totp", "regenerate-recovery-codes"},
	}
	for _, tt := range tests {
		noun, verb, err := Name(tt.method, tt.rule, tt.stream)
		if err != nil || noun != tt.noun || verb != tt.verb {
			t.Errorf("Name(%s) = %s %s, %v; want %s %s", tt.method, noun, verb, err, tt.noun, tt.verb)
		}
	}
	if _, _, err := Name("ListThings", nil, false); err == nil || !strings.Contains(err.Error(), "google.api.http") {
		t.Errorf("a unary method without a REST rule = %v, want an error that says to add one", err)
	}
	if _, _, err := Name("Ping", get("/v1/ping"), false); err == nil {
		t.Error("a name with no noun should fail")
	}
}

func TestHelp(t *testing.T) {
	tests := []struct{ name, comment, short, long string }{
		{
			"ListServers", "ListServers returns every server, connected or not, without history.",
			"Return every server", "Return every server, connected or not, without history.",
		},
		{
			"ProposeChange", "ProposeChange plans a set of edits and records them as a planned\nchange. Planning changes nothing.\n\nSecond paragraph.",
			"Plan a set of edits", "Plan a set of edits and records them as a planned change. Planning changes nothing.\n\nSecond paragraph.",
		},
		{"ApplyChange", "ApplyChange carries out a planned change.", "Carry out a planned change", "Carry out a planned change."},
		{"WatchChange", "WatchChange streams a change: first the change.", "Stream a change", "Stream a change: first the change."},
		{"Odd", "Something else entirely.", "Something else entirely", "Something else entirely."},
	}
	for _, tt := range tests {
		short, long := Help(tt.name, tt.comment)
		if short != tt.short || long != tt.long {
			t.Errorf("Help(%s) = %q, %q; want %q, %q", tt.name, short, long, tt.short, tt.long)
		}
	}
	for verb, want := range map[string]string{
		"returns": "return", "carries": "carry", "watches": "watch", "does": "do", "is": "be", "says": "say",
		"passes": "pass", "plans": "plan", "lists": "list", "streams": "stream", "replaces": "replace",
	} {
		if got := Imperative(verb); got != want {
			t.Errorf("Imperative(%s) = %s, want %s", verb, got, want)
		}
	}
}

func TestReflow(t *testing.T) {
	got := reflow(strings.Repeat("word ", 30) + "\n\nnext")
	for _, line := range strings.Split(got, "\n") {
		if len(line) > wrapWidth {
			t.Errorf("a line is %d long: %q", len(line), line)
		}
	}
	if !strings.HasSuffix(got, "\n\nnext") {
		t.Errorf("paragraphs should stay apart: %q", got)
	}
}

// request is a plugin request for the schema compiled into the binary:
// every file of dockwell.v1, with what it imports. Compiled-in
// descriptors have no comments, so help is empty, but names, flags and
// options are as make generate sees them.
func request(t *testing.T) *pluginpb.CodeGeneratorRequest {
	t.Helper()
	req := &pluginpb.CodeGeneratorRequest{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imports := fd.Imports()
		for i := range imports.Len() {
			add(imports.Get(i).FileDescriptor)
		}
		req.ProtoFile = append(req.ProtoFile, protodesc.ToFileDescriptorProto(fd))
	}
	protoregistry.GlobalFiles.RangeFilesByPackage(Package, func(fd protoreflect.FileDescriptor) bool {
		add(fd)
		req.FileToGenerate = append(req.FileToGenerate, fd.Path())
		return true
	})
	slices.Sort(req.FileToGenerate)
	return req
}

func plugin(t *testing.T, req *pluginpb.CodeGeneratorRequest) *protogen.Plugin {
	t.Helper()
	p, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateDescribesEveryMethod(t *testing.T) {
	req := request(t)
	p := plugin(t, req)
	if err := Generate(p); err != nil {
		t.Fatal(err)
	}
	res := p.Response()
	if res.Error != nil || len(res.GetFile()) != 1 || res.GetFile()[0].GetName() != OutputFile {
		t.Fatalf("Generate wrote %v, error %v", res.GetFile(), res.GetError())
	}
	src := res.GetFile()[0].GetContent()
	if !strings.HasPrefix(src, headerLicense+"\n") || !strings.Contains(src, generatedNotice) {
		t.Errorf("the output doesn't start with the license and the generated notice:\n%.200s", src)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), OutputFile, src, parser.AllErrors); err != nil {
		t.Fatalf("the output isn't Go: %v", err)
	}
	var methods int
	protoregistry.GlobalFiles.RangeFilesByPackage(Package, func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			ms := fd.Services().Get(i).Methods()
			for j := range ms.Len() {
				methods++
				if name := string(ms.Get(j).FullName()); !strings.Contains(src, `"`+name+`"`) {
					t.Errorf("the output has no command for %s", name)
				}
			}
		}
		return true
	})
	if methods == 0 {
		t.Fatal("no methods in the schema")
	}
	for _, want := range []string{
		`Noun:      "system",`, `Verb:      "version",`, // from (dockwell.v1.command)
		`Noun:      "api-tokens",`,
		`Effect:    v1.Effect_EFFECT_DESTRUCTIVE,`,
		`apicli.ServerStream[v1.WatchChangeRequest, v1.WatchChangeResponse](dockwellv1connect.ChangeServiceWatchChangeProcedure)`,
		`{Path: "id", Arg: true}`,
		`{Path: "id", Arg: true, OptionalArg: true}`,
		`Field: "firewall_rule",`, `Noun:  "firewall-rules",`,
		`{Path: "app.image"}`, `{Path: "app.env", Sensitive: true}`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the output doesn't have %s", want)
		}
	}
	// The same schema gives the same code.
	again := plugin(t, request(t))
	if err := Generate(again); err != nil {
		t.Fatal(err)
	}
	if again.Response().GetFile()[0].GetContent() != src {
		t.Error("generating twice gave different code")
	}
}

func TestGenerateRefusesTwoMethodsWithOneCommand(t *testing.T) {
	req := request(t)
	for _, f := range req.GetProtoFile() {
		if f.GetName() != "dockwell/v1/server.proto" {
			continue
		}
		for _, s := range f.GetService() {
			for _, m := range s.GetMethod() {
				if m.GetName() == "GetServer" {
					// GetServer's path, given to ListServers too.
					opts := m.GetOptions()
					for _, other := range s.GetMethod() {
						if other.GetName() == "ListServers" {
							other.Options = opts
							other.Name = func() *string { s := "GetServers"; return &s }()
						}
					}
				}
			}
		}
	}
	err := Generate(plugin(t, req))
	if err == nil || !strings.Contains(err.Error(), "would both be dockwell servers get") || !strings.Contains(err.Error(), "(dockwell.v1.command)") {
		t.Errorf("two methods named alike = %v", err)
	}
}

func TestGenerateRefusesAMethodWithoutAnEffect(t *testing.T) {
	req := request(t)
	for _, f := range req.GetProtoFile() {
		if f.GetName() != "dockwell/v1/system.proto" {
			continue
		}
		f.GetService()[0].GetMethod()[0].Options = &descriptorpb.MethodOptions{}
	}
	err := Generate(plugin(t, req))
	if err == nil || !strings.Contains(err.Error(), "doesn't set (dockwell.v1.effect)") {
		t.Errorf("a method without an effect = %v", err)
	}
}

func TestFirstClause(t *testing.T) {
	for in, want := range map[string]string{
		"Plan a set of edits and records them":           "Plan a set of edits",
		"Check the passkey's signature and signs in":     "Check the passkey's signature",
		"Return one change, with its plan":               "Return one change",
		"Plan keeping what a server has: a new revision": "Plan keeping what a server has",
		"Drop a planned change without applying it":      "Drop a planned change without applying it",
		"Turn code sign-in back on after too many tries": "Turn code sign-in back on after too many tries",
	} {
		if got := firstClause(in); got != want {
			t.Errorf("firstClause(%q) = %q, want %q", in, got, want)
		}
	}
}

// A noun's help names commands where it names methods, and a service whose
// methods' commands have several nouns describes each of its own.
func TestNounHelp(t *testing.T) {
	_, nouns, err := Methods(plugin(t, request(t)))
	if err != nil {
		t.Fatal(err)
	}
	long := map[string]string{}
	for _, n := range nouns {
		long[n.Name] = strings.Join(strings.Fields(n.Long), " ")
	}
	for noun, want := range map[string]string{
		"drift":      "dockwell drift list asks each connected server's agent",
		"auth-state": "dockwell auth-state get needs no sign-in",
		"claim-link": "dockwell claim-link check describes the server a link claims",
		"totp":       "until dockwell totp unlock turns it back on",
		"passkeys":   "Passkeys are how you sign in to the panel",
		"sessions":   "A session is a browser you're signed in on",
		"changes":    "A change is a set of edits to managed state",
	} {
		if !strings.Contains(long[noun], want) {
			t.Errorf("dockwell %s's help doesn't say %q:\n%s", noun, want, long[noun])
		}
	}
	seen := map[string]string{}
	for name, help := range long {
		if other, ok := seen[help]; ok && help != "" {
			t.Errorf("dockwell %s and dockwell %s have the same help", other, name)
		}
		seen[help] = name
		for _, method := range []string{"ListDrift", "RevertDrift", "GetAuthState", "CheckClaim", "UnlockCodeSignIn", "WatchChange"} {
			if strings.Contains(help, method) {
				t.Errorf("dockwell %s's help names the method %s, not its command", name, method)
			}
		}
	}
}

// A service whose methods' commands have several nouns can't let its
// comment describe them all.
func TestGenerateRefusesANounWithoutItsOwnHelp(t *testing.T) {
	req := request(t)
	for _, f := range req.GetProtoFile() {
		if f.GetName() == "dockwell/v1/change.proto" {
			f.GetService()[0].Options = &descriptorpb.ServiceOptions{}
		}
	}
	err := Generate(plugin(t, req))
	if err == nil || !strings.Contains(err.Error(), "have the nouns changes, services, updates and drift") || !strings.Contains(err.Error(), "(dockwell.v1.nouns)") {
		t.Errorf("a service with two nouns and no help for them = %v", err)
	}
}

// A field whose name would take one of the CLI's own flags gets the flag
// its (dockwell.v1.flag) names.
func TestGenerateNamesFlagsFromTheOption(t *testing.T) {
	methods, _, err := Methods(plugin(t, request(t)))
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]string{}
	for _, m := range methods {
		for _, f := range m.Fields {
			if f.Path == "token" {
				flags[m.Noun+" "+m.Verb] = f.Flag
			}
		}
	}
	want := map[string]string{
		"claim-link check": "claim-token", "claim-link begin": "claim-token", "claim-link finish": "claim-token",
		"sessions sign-in-with-link": "link-token",
	}
	for command, flag := range want {
		if flags[command] != flag {
			t.Errorf("dockwell %s takes its token as --%s, want --%s", command, flags[command], flag)
		}
	}
	if len(flags) != len(want) {
		t.Errorf("fields named token: %v, want %v", flags, want)
	}
}

func TestGenerateNamesNamesEveryCommand(t *testing.T) {
	p := plugin(t, request(t))
	if err := GenerateNames(p); err != nil {
		t.Fatal(err)
	}
	res := p.Response()
	if res.Error != nil || len(res.GetFile()) != 1 || res.GetFile()[0].GetName() != NamesFile {
		t.Fatalf("GenerateNames wrote %v, error %v", res.GetFile(), res.GetError())
	}
	src := res.GetFile()[0].GetContent()
	if !strings.HasPrefix(src, headerLicense+"\n") || !strings.Contains(src, generatedNotice) {
		t.Errorf("the output doesn't start with the license and the generated notice:\n%.200s", src)
	}
	f, err := parser.ParseFile(token.NewFileSet(), NamesFile, src, parser.AllErrors)
	if err != nil {
		t.Fatalf("the output isn't Go: %v", err)
	}
	// The agent build links it, so it must bring nothing with it.
	if len(f.Imports) > 0 {
		t.Errorf("the output imports %d packages, want none", len(f.Imports))
	}
	methods, nouns, err := Methods(plugin(t, request(t)))
	if err != nil || len(methods) == 0 {
		t.Fatalf("Methods = %d methods, error %v", len(methods), err)
	}
	kinds, err := Kinds(plugin(t, request(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(nouns)+len(kinds))
	for _, n := range nouns {
		want = append(want, n.Name)
	}
	for _, k := range kinds {
		want = append(want, k.Noun)
	}
	for _, name := range want {
		if strings.Count(src, strconv.Quote(name)+",") != 1 {
			t.Errorf("the output doesn't name dockwell %s once", name)
		}
	}
	for _, name := range []string{"servers", "api-tokens", "firewall-rules"} {
		if !slices.Contains(want, name) {
			t.Errorf("the schema has no dockwell %s", name)
		}
	}
}

// A kind's verbs join a noun a service has, unless a method has one of them:
// the two would be one command, and the help would name it twice.
func TestAKindsVerbsJoinANounOnce(t *testing.T) {
	kinds := []Kind{{Field: "secret", Noun: "secrets"}}
	nouns, err := joinKinds([]Noun{{Name: "secrets", Short: "Reveal a secret", verbs: []string{"reveal"}}}, kinds)
	if err != nil || nouns[0].Short != "Reveal, create, update or delete secrets" {
		t.Errorf("joinKinds = %v, %v", nouns, err)
	}
	_, err = joinKinds([]Noun{{Name: "secrets", verbs: []string{"list", "create"}}}, kinds)
	if err == nil || !strings.Contains(err.Error(), "dockwell secrets create is both a method's and the kind secret's") {
		t.Errorf("a method with a kind's verb = %v, want it refused", err)
	}
}

// A noun's line lists its verbs, the one-word ones alone when all of them are too
// long, and counts the rest; when every verb is one word, there's no rest to count.
func TestANounsLineCountsOnlyWhatItLeavesOut(t *testing.T) {
	long := []string{"list", "get", "preview", "create", "delete", "policies", "destinations", "fetch", "restore"}
	if got := nounShort("backups", long, ""); got != "List, get, preview, create, delete, policies, destinations, fetch or restore backups" {
		t.Errorf("every verb one word: %q", got)
	}
	if got := nounShort("backups", append(slices.Clone(long), "list-copies", "take-now"), ""); !strings.HasSuffix(got, ", and 2 more") {
		t.Errorf("two verbs of two words: %q", got)
	}
}
