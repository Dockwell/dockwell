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
	github.com/go-webauthn/webauthn v0.18.2
	github.com/jackc/pgx/v5 v5.11.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/crypto v0.58.0
	golang.org/x/term v0.47.0
	google.golang.org/protobuf v1.36.12
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fxamacker/cbor/v2 v2.9.4 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.3.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
