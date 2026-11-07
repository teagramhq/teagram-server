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
		if len(os.Args) != 3 {
			fail()
		}
		identity, err := age.GenerateX25519Identity()
		if err != nil {
			fail()
		}
		if err := os.WriteFile(os.Args[2], []byte(identity.String()+"\n"), 0o600); err != nil {
			fail()
		}
		fmt.Println(identity.Recipient())
	case "decrypt":
		if len(os.Args) != 3 {
			fail()
		}
		encoded, err := os.ReadFile(os.Args[2])
		if err != nil {
			fail()
		}
		identity, err := age.ParseX25519Identity(strings.TrimSpace(string(encoded)))
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
