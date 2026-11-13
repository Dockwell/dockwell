// SPDX-License-Identifier: AGPL-3.0-only

// Command htpasswd writes one line of an htpasswd file, with bcrypt, as the
// registry image's htpasswd sign-in reads: a username, then the password from
// standard input, so it stays out of the process list. make docker-e2e runs a
// registry that asks for a login with it (ADR 0086).
//
//	htpasswd ci <password-file >htpasswd
package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" || strings.Contains(os.Args[1], ":") {
		log.Fatal("htpasswd takes the username, with no colon in it, and reads the password from standard input")
	}
	password, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && password == "" {
		log.Fatalf("htpasswd read no password from standard input: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(strings.TrimSuffix(password, "\n")), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("htpasswd can't hash the password: %v", err)
	}
	fmt.Printf("%s:%s\n", os.Args[1], hash)
}
