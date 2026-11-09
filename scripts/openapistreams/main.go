// SPDX-License-Identifier: AGPL-3.0-only

// Command openapistreams says, in the OpenAPI document, what the API's streaming
// REST routes carry, and writes those routes for the TypeScript client
// (docs/adr/0045-a-file-manager-on-a-server.md). A method whose REST body, or whose
// REST answer, is a google.api.HttpBody streams bytes over REST, as the control
// plane serves it, where protoc-gen-openapi writes JSON. So this finds each such
// method in the schema, and in the document:
//
//   - one that streams its request takes application/octet-stream, as bytes;
//   - one that streams its answer answers application/octet-stream or a zip, as
//     bytes, with Content-Disposition and, where it's known, Content-Length.
//
// Each is marked x-dockwell-stream, request or response. Then it writes each one's
// method, path, query and media types, as the document now has them, to a TypeScript
// file beside the generated client, so a client builds no path by hand.
//
//	go run ./scripts/openapistreams gen/openapi/openapi.yaml packages/api/src/gen/streams.ts
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/dockwell/dockwell/gen/dockwell/v1" // the schema, in the registry
)

// stream is a method that streams bytes over REST, and which way.
type stream struct {
	operationID string
	way         string // "request" or "response"
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/openapistreams <openapi.yaml> <streams.ts>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "openapistreams:", err)
		os.Exit(1)
	}
}

func run(document, out string) error {
	streams := streaming()
	if len(streams) == 0 {
		return errors.New("the schema has no method that streams bytes over REST, so there's nothing to say")
	}
	text, err := os.ReadFile(document) //nolint:gosec // the document make generate names
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(text, &doc); err != nil {
		return fmt.Errorf("can't read %s: %w", document, err)
	}
	routes, err := mark(&doc, streams)
	if err != nil {
		return fmt.Errorf("%s: %w", document, err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := os.WriteFile(document, buf.Bytes(), 0o644); err != nil { //nolint:gosec // a generated document, read by anyone
		return err
	}
	return os.WriteFile(out, []byte(typescript(routes)), 0o644) //nolint:gosec // generated source
}

// streaming finds the methods that stream bytes over REST: a client stream whose
// REST body is an HttpBody, and a server stream whose REST answer is one.
func streaming() map[string]stream {
	out := map[string]stream{}
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				rule, _ := proto.GetExtension(md.Options(), annotations.E_Http).(*annotations.HttpRule)
				if rule == nil {
					continue
				}
				id := string(sd.Name()) + "_" + string(md.Name())
				switch {
				case md.IsStreamingClient() && isBody(md.Input(), rule.GetBody()):
					out[id] = stream{operationID: id, way: "request"}
				case md.IsStreamingServer() && isBody(md.Output(), rule.GetResponseBody()):
					out[id] = stream{operationID: id, way: "response"}
				}
			}
		}
		return true
	})
	return out
}

// isBody reports whether field of msg is an HttpBody, or msg is one where field is
// empty.
func isBody(msg protoreflect.MessageDescriptor, field string) bool {
	if field != "" && field != "*" {
		f := msg.Fields().ByName(protoreflect.Name(field))
		return f != nil && f.Message() != nil && f.Message().FullName() == "google.api.HttpBody"
	}
	return msg.FullName() == "google.api.HttpBody"
}

// route is a streaming route as the document has it.
type route struct {
	operationID, method, path, way string
	query                          []string
	request                        string
	responses                      []string
}

// mark rewrites what each streaming operation carries, and reads back its route.
func mark(doc *yaml.Node, streams map[string]stream) ([]route, error) {
	paths := get(get(doc.Content[0], "paths"), "")
	if paths == nil {
		return nil, errors.New("it has no paths")
	}
	var routes []route
	for i := 0; i+1 < len(paths.Content); i += 2 {
		path, item := paths.Content[i].Value, paths.Content[i+1]
		for k := 0; k+1 < len(item.Content); k += 2 {
			method, op := item.Content[k].Value, item.Content[k+1]
			s, ok := streams[get(op, "operationId").Value]
			if !ok {
				continue
			}
			r := route{operationID: s.operationID, method: strings.ToUpper(method), path: path, way: s.way}
			for _, p := range get(op, "parameters").Content {
				if get(p, "in").Value == "query" {
					r.query = append(r.query, get(p, "name").Value)
				}
			}
			switch s.way {
			case "request":
				r.request = "application/octet-stream"
				set(op, "requestBody", mapping(
					"description", scalar("The bytes, from offset, as they come: not JSON. Send a large file as several requests, each from where the last ended."),
					"required", scalar("true"),
					"content", mapping(r.request, mapping("schema", binary())),
				))
				r.responses = []string{"application/json"}
			case "response":
				ok := mapping(
					"description", scalar("The bytes, as they're read: a file as it is, or a folder as a zip archive made as it goes. One that stops partway cuts the connection, so it can't pass for the whole."),
					"headers", mapping(
						"Content-Disposition", mapping("description", scalar("attachment, with the file's name, or the folder's with .zip"), "schema", mapping("type", scalar("string"))),
						"Content-Length", mapping("description", scalar("How large a file is. A folder's archive has none, as it's made as it goes."), "schema", mapping("type", scalar("integer"))),
					),
					"content", mapping(
						"application/octet-stream", mapping("schema", binary()),
						"application/zip", mapping("schema", binary()),
					),
				)
				set(get(op, "responses"), "200", ok)
				r.responses = []string{"application/octet-stream", "application/zip"}
			}
			set(op, "x-dockwell-stream", scalar(s.way))
			routes = append(routes, r)
			delete(streams, s.operationID)
		}
	}
	if len(streams) > 0 {
		var missing []string
		for id := range streams {
			missing = append(missing, id)
		}
		slices.Sort(missing)
		return nil, fmt.Errorf("it has no operation %s, which the schema streams over REST", strings.Join(missing, ", "))
	}
	slices.SortFunc(routes, func(a, b route) int { return strings.Compare(a.operationID, b.operationID) })
	return routes, nil
}

// get is a mapping's value for key, or an empty node. With key "", it's n itself.
func get(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return &yaml.Node{}
	}
	if key == "" {
		return n
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return &yaml.Node{}
}

// set puts value under key in a mapping, in place of what's there.
func set(n *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1] = value
			return
		}
	}
	n.Content = append(n.Content, scalar(key), value)
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

func mapping(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Content = append(n.Content, scalar(kv[i].(string)), kv[i+1].(*yaml.Node)) //nolint:forcetypeassert // pairs this file writes
	}
	return n
}

func binary() *yaml.Node { return mapping("type", scalar("string"), "format", scalar("binary")) }

// typescript writes the routes for the TypeScript client.
func typescript(routes []route) string {
	var b strings.Builder
	b.WriteString(`// SPDX-License-Identifier: Apache-2.0

// Generated by scripts/openapistreams from gen/openapi/openapi.yaml: the REST routes
// that stream bytes, as the OpenAPI document has them. Do not edit.

/** A REST route that streams its request's body or its answer's, as bytes. */
export interface StreamRoute {
  /** Its operation in the OpenAPI document. */
  readonly operationId: string;
  /** Which way it streams. */
  readonly stream: "request" | "response";
  /** Its HTTP method. */
  readonly method: string;
  /** Its path, with {name} for each path parameter. */
  readonly path: string;
  /** Its query parameters, by name. */
  readonly query: readonly string[];
  /** What its request's body is, for one that streams it. */
  readonly requestType?: string;
  /** What it answers. */
  readonly responseTypes: readonly string[];
}

/** The routes, by operation. */
export const streamRoutes = {
`)
	for i := range routes {
		r := &routes[i]
		fmt.Fprintf(&b, "  %s: {\n", r.operationID)
		fmt.Fprintf(&b, "    operationId: %q,\n    stream: %q,\n    method: %q,\n    path: %q,\n", r.operationID, r.way, r.method, r.path)
		fmt.Fprintf(&b, "    query: [%s],\n", quoted(r.query))
		if r.request != "" {
			fmt.Fprintf(&b, "    requestType: %q,\n", r.request)
		}
		fmt.Fprintf(&b, "    responseTypes: [%s],\n  },\n", quoted(r.responses))
	}
	b.WriteString("} as const satisfies Record<string, StreamRoute>;\n")
	return b.String()
}

func quoted(list []string) string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, ", ")
}
