package hash_test

import (
	"strings"
	"testing"

	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

// FuzzVerifyEncodedHash feeds arbitrary strings to the encoded-hash parser.
//
// Verify reads a string that arrives from the database, and a database row is a
// less trusted input than it looks: a hash written by a future version, a row
// truncated by a botched restore, or a column somebody edited by hand all reach
// this function. It must return an error for anything it cannot parse and must
// never panic — a panic here is an unauthenticated denial of service, because
// the parse happens before the password is checked.
//
// The one property that matters beyond not panicking: no input may verify true
// against a password the caller does not know. Verify returning (true, nil) for
// a hash the fuzzer invented would be an authentication bypass.
func FuzzVerifyEncodedHash(f *testing.F) {
	real, err := hash.New(secret.New("a correct horse battery staple"))
	if err != nil {
		f.Fatal(err)
	}

	f.Add(real)
	f.Add("")
	f.Add("$argon2id$")
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$a2V5")
	f.Add("$argon2id$v=19$m=0,t=0,p=0$$")
	f.Add("$argon2id$v=19$m=4294967295,t=3,p=2$c2FsdHNhbHQ$a2V5")
	f.Add("$argon2id$v=19$m=65536,t=4294967295,p=2$c2FsdHNhbHQ$a2V5")
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$") // empty key
	f.Add("$argon2id$v=19$m=65536,t=0,p=2$c2FsdHNhbHQ$a2V5")
	f.Add("$argon2id$v=19$m=65536,t=3,p=0$c2FsdHNhbHQ$a2V5")
	f.Add("$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$a2V5")
	f.Add("$argon2id$v=99$m=65536,t=3,p=2$c2FsdA$a2V5")
	f.Add("$argon2id$v=19$m=x,t=y,p=z$c2FsdA$a2V5")
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$!!!!$a2V5")
	f.Add(strings.Repeat("$", 64))

	f.Fuzz(func(t *testing.T, encoded string) {
		// Very large memory parameters would have the fuzzer allocating
		// gigabytes rather than finding parse bugs, which is the thing being
		// looked for here.
		if len(encoded) > 512 {
			t.Skip()
		}

		ok, err := hash.Verify(secret.New("a password the fuzzer does not know"), encoded)
		if err != nil {
			return
		}
		if ok {
			t.Fatalf("a fuzzer-supplied hash verified against an unrelated password: %q", encoded)
		}
	})
}
