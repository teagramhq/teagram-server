package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

func main() {
	if len(os.Args) < 2 {
		fail()
	}
	switch os.Args[1] {
	case "keygen":
		if len(os.Args) != 2 {
			fail()
		}
		identity, err := age.GenerateX25519Identity()
		if err != nil {
			fail()
		}
		fmt.Printf("%s\n%s\n", identity.Recipient(), identity)
	case "decrypt":
		if len(os.Args) != 2 {
			fail()
		}
		identity, err := age.ParseX25519Identity(strings.TrimSpace(os.Getenv("TEST_AGE_IDENTITY")))
		if err != nil {
			fail()
		}
		reader, err := age.Decrypt(armor.NewReader(os.Stdin), identity)
		if err != nil {
			fail()
		}
		if _, err := io.Copy(os.Stdout, reader); err != nil {
			fail()
		}
	default:
		fail()
	}
}

func fail() {
	fmt.Fprintln(os.Stderr, "age fixture helper failed")
	os.Exit(1)
}
