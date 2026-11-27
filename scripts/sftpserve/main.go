// SPDX-License-Identifier: AGPL-3.0-only

// sftpserve is a local SFTP destination for panel and transfer tests.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/dockwell/dockwell/internal/backup/sftpstore/sftptest"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: sftpserve <listen-address> <authorized-keys-file>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(address, authorizedKeys string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listen := sftptest.Listen
	if os.Getenv("DOCKWELL_TEST_SFTP_DISK") == "1" {
		listen = sftptest.ListenDisk
	}
	server, err := listen(ctx, address, sftptest.Login{
		User: "backups",
		AcceptKey: func(key ssh.PublicKey) bool {
			body, err := os.ReadFile(authorizedKeys) //nolint:gosec // a fixture path provided by the browser test runner
			if err != nil {
				return false
			}
			authorized, _, _, _, err := ssh.ParseAuthorizedKey(body)
			return err == nil && bytes.Equal(key.Marshal(), authorized.Marshal())
		},
	})
	if err != nil {
		return err
	}
	defer server.Close()
	fmt.Println(server.Address(), server.Fingerprint())
	<-ctx.Done()
	return nil
}
