module github.com/dockwell/dockwell

go 1.27.0

// Builds, CI and releases use this toolchain; bump it for Go security fixes.
toolchain go1.27.3

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
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
