module github.com/dockwell/dockwell

go 1.27.0

// The panel and the TypeScript packages live alongside the Go code; keep their
// node_modules (and any stray .go files inside them) out of ./... patterns.
ignore (
	./node_modules
	./packages
	./web
)

require (
	connectrpc.com/connect v1.21.1
	connectrpc.com/grpchealth v1.5.0
	connectrpc.com/grpcreflect v1.3.1
	github.com/spf13/cobra v1.10.2
	golang.org/x/term v0.47.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.49.0 // indirect
)
