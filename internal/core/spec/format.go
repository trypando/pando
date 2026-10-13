package spec

import (
	"encoding/base64"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// SecretFormat is the shape a slot's value must take, for a slot that holds a
// key the app generates nothing for and cannot start without: Rails'
// SECRET_KEY_BASE, Laravel's APP_KEY, Django's SECRET_KEY.
//
// It exists so that "generate a value" produces one the app accepts. A random
// string is the right answer for most secrets and the wrong one for these —
// Laravel refuses a key that is not 32 bytes ("Unsupported cipher or incorrect
// key length"), Phoenix one shorter than 64, and Django's deploy check one
// shorter than 50 — and a person who pressed Generate has no way to know which
// (R-130: a hole with a type).
//
// Empty means the value has no shape Pando knows of, and anything goes.
type SecretFormat string

const (
	// FormatRandom is any non-empty string. Generated as 32 random bytes.
	FormatRandom SecretFormat = "random"

	// FormatRandomLong is at least 64 characters: Rails' and Phoenix's
	// SECRET_KEY_BASE. Generated as 64 random bytes.
	FormatRandomLong SecretFormat = "random_long"

	// FormatDjangoKey is what Django's deploy check accepts as SECRET_KEY: at
	// least 50 characters, at least 5 of them different, and not the
	// "django-insecure-" prefix startproject writes.
	FormatDjangoKey SecretFormat = "django_key"

	// FormatLaravelKey is a Laravel application key: "base64:" and 32 bytes
	// in base64, or 32 characters used as they are.
	FormatLaravelKey SecretFormat = "laravel_key"
)

// Known reports whether this is a format Pando can generate and check.
func (f SecretFormat) Known() bool {
	switch f {
	case "", FormatRandom, FormatRandomLong, FormatDjangoKey, FormatLaravelKey:
		return true
	}
	return false
}

// Check refuses a value the app named by key would refuse, saying what a valid
// one looks like (R-105). A value is checked when somebody sets it, so the
// refusal arrives while they are still looking at it rather than as a crash
// loop after the deploy.
func (f SecretFormat) Check(key, value string) error {
	switch f {
	case FormatRandomLong:
		if n := len(value); n < 64 {
			return errs.Newf(errs.ValidInvalid,
				"%s must be at least 64 characters long, and the value given has %d. The app signs its "+
					"sessions and cookies with it and refuses to start with a shorter one.", key, n).
				WithDetail("slot_key", key).
				WithRemedy("Use Generate beside the value, or paste the output of `openssl rand -hex 64`.")
		}
	case FormatDjangoKey:
		unique := map[rune]bool{}
		for _, r := range value {
			unique[r] = true
		}
		if len(value) < 50 || len(unique) < 5 || strings.HasPrefix(value, "django-insecure-") {
			return errs.Newf(errs.ValidInvalid,
				"%s must be at least 50 characters long, use at least 5 different characters, and not start "+
					"with \"django-insecure-\". Django's deployment check refuses any other key, and the app "+
					"does not start.", key).
				WithDetail("slot_key", key).
				WithRemedy("Use Generate beside the value, or paste the output of `openssl rand -base64 64`.")
		}
	case FormatLaravelKey:
		if !laravelKey(value) {
			return errs.Newf(errs.ValidInvalid,
				"%s must be a Laravel application key: \"base64:\" followed by 32 random bytes in base64. "+
					"Laravel encrypts sessions and cookies with it, and with any other value it answers every "+
					"page with \"Unsupported cipher or incorrect key length\".", key).
				WithDetail("slot_key", key).
				WithRemedy("Use Generate beside the value, or paste what " +
					"`echo \"base64:$(openssl rand -base64 32)\"` prints.")
		}
	}
	return nil
}

func laravelKey(value string) bool {
	encoded, ok := strings.CutPrefix(value, "base64:")
	if !ok {
		// A raw key is used byte for byte, and AES-256 wants 32 of them.
		return len(value) == 32
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && len(raw) == 32
}

// CheckSlotValue refuses a value for a slot whose format it does not fit. A
// slot with no format, or no slot by that key, accepts anything: the value is
// the person's answer, and Pando has nothing to check it against.
func CheckSlotValue(s *AppSpec, slot, value string) error {
	for _, sl := range s.Slots {
		if sl.Key == slot {
			return sl.Format.Check(sl.Key, value)
		}
	}
	return nil
}
