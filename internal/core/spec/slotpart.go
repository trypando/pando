package spec

import (
	"net/url"
	"strings"
)

// SlotPart is one piece of a slot's connection string, for a variable that
// holds only that piece (EnvEntry.SlotPart).
//
// The connection string stays the slot's one value — what is provisioned,
// bound or pasted — and each part is read out of it at deploy. Nothing about a
// part is stored, so a password rotated in the URL is rotated in PGPASSWORD
// too.
type SlotPart string

const (
	// SlotPartHost is the host name alone: "svc-01h…".
	SlotPartHost SlotPart = "host"
	// SlotPartPort is the port, or the type's default when the URL has none.
	SlotPartPort SlotPart = "port"
	// SlotPartHostPort is "host:port", what Joomla's JOOMLA_DB_HOST and
	// most "-e X_HOST=hostname:port" instructions take.
	SlotPartHostPort SlotPart = "hostport"
	SlotPartUser     SlotPart = "user"
	SlotPartPassword SlotPart = "password"
	// SlotPartDatabase is the path without its slash: the database's name.
	SlotPartDatabase SlotPart = "database"
)

// Known reports whether this is a part Pando can read out of a connection
// string. Empty — the whole string — is.
func (p SlotPart) Known() bool {
	switch p {
	case "", SlotPartHost, SlotPartPort, SlotPartHostPort, SlotPartUser, SlotPartPassword, SlotPartDatabase:
		return true
	}
	return false
}

// defaultPorts are the ports a connection string of each scheme means when it
// names none.
var defaultPorts = map[string]string{
	"postgres": "5432", "postgresql": "5432",
	"mysql": "3306", "mariadb": "3306",
	"redis": "6379", "rediss": "6379",
	"mongodb": "27017",
}

// SlotPartOf reads one part out of a connection string. The whole string for
// an empty part; for anything else the URL must parse, and ok is false when it
// does not — a value somebody pasted that is not a URL has no host to take.
func SlotPartOf(value string, part SlotPart) (string, bool) {
	if part == "" {
		return value, true
	}
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" {
		return "", false
	}
	switch part {
	case SlotPartHost:
		return u.Hostname(), true
	case SlotPartPort:
		if p := u.Port(); p != "" {
			return p, true
		}
		p, ok := defaultPorts[strings.ToLower(u.Scheme)]
		return p, ok
	case SlotPartHostPort:
		port, ok := SlotPartOf(value, SlotPartPort)
		if !ok {
			return u.Host, true
		}
		return u.Hostname() + ":" + port, true
	case SlotPartUser:
		if u.User == nil {
			return "", true
		}
		return u.User.Username(), true
	case SlotPartPassword:
		if u.User == nil {
			return "", true
		}
		pw, _ := u.User.Password()
		return pw, true
	case SlotPartDatabase:
		return strings.TrimPrefix(u.Path, "/"), true
	}
	return "", false
}
