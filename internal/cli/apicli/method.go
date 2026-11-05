// SPDX-License-Identifier: AGPL-3.0-only

// Package apicli is the shared machinery of the dockwell CLI's API
// commands. The commands themselves are generated from the schema into
// internal/cli/apicmd, one for each method of dockwell.v1 and three for
// each kind of resource; this package turns those descriptions into cobra
// commands. It binds flags to request fields, calls the control plane with
// an API token, proposes and shows plans and asks before applying them,
// follows a change as it applies, asks which one when the API says there
// is a choice, and prints what comes back for people or, with --json, as
// the API's JSON. See docs/adr/0019-a-cli-generated-from-the-schema.md.
package apicli

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Method is the command for one method of the API, as the generator
// describes it.
type Method struct {
	// Name is the method's full name, such as
	// "dockwell.v1.ServerService.ListServers".
	Name string
	// Procedure is its path, such as
	// "/dockwell.v1.ServerService/ListServers".
	Procedure string
	// HTTP is its REST rule, such as "GET /v1/servers". Empty for a
	// stream, which has none.
	HTTP string
	// Noun and Verb name the command: dockwell <noun> <verb>.
	Noun, Verb string
	// Short and Long are its help, from the schema's comments.
	Short, Long string
	// Effect is how it touches managed state.
	Effect dockwellv1.Effect
	// Fields are its request's fields, each a flag or an argument.
	Fields []Field
	// Call makes the call. Nil for a method the CLI can't call yet, such
	// as a client stream.
	Call Caller
}

// Field is a flag, or an argument, for one field of a request or a spec.
type Field struct {
	// Path is the field's name in the request, such as "page_size", or
	// for a field of a kind's spec the path to it, such as "app.image".
	Path string
	// Flag is the field's flag, from (dockwell.v1.flag), where its name
	// would take one of the CLI's own flags. Empty for most fields, whose
	// flag is FlagName(Path).
	Flag string
	// Help is the field's comment.
	Help string
	// Arg makes the field the command's argument instead of a flag, as a
	// request's id is.
	Arg bool
	// Sensitive marks a field that may hold a secret.
	Sensitive bool
	// FromFile is a text field's (dockwell.v1.from_file): its flag takes the
	// text as given, or reads it from a file with @path, or standard input with -.
	FromFile bool
}

// Kind is the commands for one kind of resource: dockwell <noun> create,
// update and delete.
type Kind struct {
	// Field is the field of dockwell.resource.v1alpha1.Spec that holds the
	// kind's spec, such as "firewall_rule".
	Field string
	// Noun names the commands, such as "firewall-rules".
	Noun string
	// Short and Long describe the kind, from the schema's comments.
	Short, Long string
	// Fields are the spec's fields, flattened.
	Fields []Field
}

// Noun is the help of a noun, the command that holds its verbs.
type Noun struct {
	Name, Short, Long string
}

// errWrongRequest is a bug: a command built a request of another method's.
var errWrongRequest = errors.New("the CLI built a request that isn't the method's own, so it can't call it. Report this as a bug in dockwell")

// Caller calls one method.
type Caller interface {
	// NewRequest returns an empty request.
	NewRequest() proto.Message
	// Response describes what the method returns.
	Response() protoreflect.MessageDescriptor
	// Streams reports a method that streams its responses.
	Streams() bool
	// Invoke calls the method with req and passes each response to each,
	// in order: one for a unary method, any number for a stream.
	Invoke(ctx context.Context, c *Client, req proto.Message, each func(proto.Message) error) error
}

// Unary returns the Caller for a unary method at procedure.
func Unary[Req, Res any, PReq interface {
	*Req
	proto.Message
}, PRes interface {
	*Res
	proto.Message
}](procedure string) Caller {
	return unary[Req, Res, PReq, PRes]{procedure: procedure}
}

type unary[Req, Res any, PReq interface {
	*Req
	proto.Message
}, PRes interface {
	*Res
	proto.Message
}] struct {
	procedure string
}

func (u unary[Req, Res, PReq, PRes]) NewRequest() proto.Message { return PReq(new(Req)) }

func (unary[Req, Res, PReq, PRes]) Streams() bool { return false }

func (unary[Req, Res, PReq, PRes]) Response() protoreflect.MessageDescriptor {
	return PRes(new(Res)).ProtoReflect().Descriptor()
}

func (u unary[Req, Res, PReq, PRes]) Invoke(ctx context.Context, c *Client, req proto.Message, each func(proto.Message) error) error {
	in, ok := req.(PReq)
	if !ok {
		return errWrongRequest
	}
	res, err := connect.NewClient[Req, Res](c.HTTP, c.URL+u.procedure, c.options()...).CallUnary(ctx, connect.NewRequest((*Req)(in)))
	if err != nil {
		return err
	}
	return each(PRes(res.Msg))
}

// ServerStream returns the Caller for a method at procedure that streams
// its responses.
func ServerStream[Req, Res any, PReq interface {
	*Req
	proto.Message
}, PRes interface {
	*Res
	proto.Message
}](procedure string) Caller {
	return serverStream[Req, Res, PReq, PRes]{procedure: procedure}
}

type serverStream[Req, Res any, PReq interface {
	*Req
	proto.Message
}, PRes interface {
	*Res
	proto.Message
}] struct {
	procedure string
}

func (s serverStream[Req, Res, PReq, PRes]) NewRequest() proto.Message { return PReq(new(Req)) }

func (serverStream[Req, Res, PReq, PRes]) Streams() bool { return true }

func (serverStream[Req, Res, PReq, PRes]) Response() protoreflect.MessageDescriptor {
	return PRes(new(Res)).ProtoReflect().Descriptor()
}

func (s serverStream[Req, Res, PReq, PRes]) Invoke(ctx context.Context, c *Client, req proto.Message, each func(proto.Message) error) error {
	in, ok := req.(PReq)
	if !ok {
		return errWrongRequest
	}
	stream, err := connect.NewClient[Req, Res](c.HTTP, c.URL+s.procedure, c.options()...).CallServerStream(ctx, connect.NewRequest((*Req)(in)))
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		if err := each(PRes(stream.Msg())); err != nil {
			return err
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
