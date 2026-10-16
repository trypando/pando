package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// AdminPasswordEnv is the variable the first administrator's password may be
// given in. Load unsets it once read, so it is not inherited by every child
// process for the life of the server (R-194).
const AdminPasswordEnv = "PANDO_ADMIN_PASSWORD"

// read settles the first administrator's password, from its file if one is
// named, and unsets AdminPasswordEnv either way: read once, it is not left in
// the environment for every child process to inherit (R-194).
func (b *Bootstrap) read() error {
	defer func() { _ = os.Unsetenv(AdminPasswordEnv) }()
	return b.withAdminPasswordFile()
}

// withAdminPasswordFile reads the first administrator's password from
// AdminPasswordFile, the way the database password is read from its file: so
// it is in neither the Compose file nor `docker inspect`.
//
// An empty file, or a directory, is no password rather than an error. That is
// what Compose mounts for a config whose content is an unset variable, and an
// operator who chose no password wants the setup token instead (R-046). A path
// that does not exist is an error: it is a typo, and silently setting nobody up
// would hide it.
func (b *Bootstrap) withAdminPasswordFile() error {
	if b.AdminPasswordFile == "" {
		return nil
	}
	if b.AdminPassword != "" {
		return fmt.Errorf("%s and PANDO_ADMIN_PASSWORD_FILE are both set. Set only one of them; "+
			"the file keeps the password out of the process environment", AdminPasswordEnv)
	}
	info, err := os.Stat(b.AdminPasswordFile)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("PANDO_ADMIN_PASSWORD_FILE is %s, which does not exist. "+
			"It names a file holding the first administrator's password; unset it to set the administrator up in the console instead", b.AdminPasswordFile)
	}
	if err == nil && info.IsDir() {
		return nil
	}
	raw, err := os.ReadFile(b.AdminPasswordFile)
	if err != nil {
		return fmt.Errorf("PANDO_ADMIN_PASSWORD_FILE is %s, which Pando could not read: %w", b.AdminPasswordFile, err)
	}
	b.AdminPassword = strings.TrimRight(string(raw), "\r\n")
	return nil
}
