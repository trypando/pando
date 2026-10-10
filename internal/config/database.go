package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// withPasswordFile writes the password in PasswordFile into URL (issue #130).
//
// One place says what the password is. A URL that already carries one, and a
// file naming another, is refused rather than resolved: whichever Pando picked,
// the operator who set the other would be debugging a refused connection with
// the wrong password in mind.
func (d *Database) withPasswordFile() error {
	if d.PasswordFile == "" || d.URL == "" {
		return nil
	}
	raw, err := os.ReadFile(d.PasswordFile)
	if err != nil {
		return fmt.Errorf("PANDO_DATABASE_PASSWORD_FILE is %s, which Pando could not read: %w. "+
			"It is the file holding the database password; with the bundled Compose file the secrets "+
			"service writes it on first start, so check that service ran", d.PasswordFile, err)
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if password == "" {
		return fmt.Errorf("PANDO_DATABASE_PASSWORD_FILE is %s, which is empty. It must hold the database password", d.PasswordFile)
	}
	u, err := url.Parse(d.URL)
	if err != nil || u.User == nil {
		// Said without the URL, which may hold a password of its own.
		return fmt.Errorf("PANDO_DATABASE_PASSWORD_FILE is set, and PANDO_DATABASE_URL does not name a user to " +
			"give the password to. Use a URL such as postgres://pando@postgres:5432/pando")
	}
	if _, has := u.User.Password(); has {
		return fmt.Errorf("PANDO_DATABASE_URL includes a password and PANDO_DATABASE_PASSWORD_FILE names a file " +
			"holding one. Set only one of them")
	}
	u.User = url.UserPassword(u.User.Username(), password)
	d.URL = u.String()
	return nil
}
