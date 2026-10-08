package config

import (
	"fmt"

	"github.com/spf13/viper"

	"github.com/trypando/pando/internal/adapter/api"
)

// RegistryAdapterID is the ID of the image registry adapter PANDO_REGISTRY_*
// declares.
const RegistryAdapterID = "image_registry"

// withRegistry adds the image registry adapter the registry settings declare,
// when PANDO_REGISTRY_URL or registry.url is set (R-252, issue #153).
//
// It is a declaration like one under adapters:, and read-only for the same
// reason (R-271): what the operator wrote at startup is what runs, and a
// refusal to change it names the variable. The password is read from where
// it was given — PANDO_REGISTRY_PASSWORD or PANDO_REGISTRY_PASSWORD_FILE —
// when the adapter is configured, never kept in the declaration (R-190).
func withRegistry(v *viper.Viper, path string, r Registry, decls []AdapterDecl) ([]AdapterDecl, error) {
	if r.URL == "" {
		return decls, nil
	}
	src := sourceOf(v, "registry.url", path)
	for _, d := range decls {
		if d.Category == string(api.CategoryImageRegistry) && d.Default && d.Enabled {
			return nil, fmt.Errorf("%s declares the install's image registry, and the config file %s declares a default one at %s. "+
				"An install pushes builds to one registry: remove one of them", where(src), path, d.Source.Key)
		}
		if d.ID == RegistryAdapterID {
			return nil, fmt.Errorf("%s declares the image registry adapter %s, and the config file %s declares an adapter with that ID at %s. "+
				"Remove one of them", where(src), RegistryAdapterID, path, d.Source.Key)
		}
	}

	var password CredentialRef
	switch {
	case r.PasswordFile != "":
		password = CredentialRef{File: r.PasswordFile}
	case r.Password != "":
		// An inline password in the file would be a credential kept in it,
		// which no other declaration allows (R-190).
		if pw := sourceOf(v, "registry.password", path); pw.Kind != "env" {
			return nil, fmt.Errorf("the config file %s sets registry.password, and Pando does not read a credential from the config file. "+
				"Set PANDO_REGISTRY_PASSWORD, or registry.password_file to a file holding it", path)
		}
		password = CredentialRef{Env: "PANDO_REGISTRY_PASSWORD"}
	}

	d := AdapterDecl{
		ID: RegistryAdapterID, Category: string(api.CategoryImageRegistry), Name: "Image registry",
		Default: true, Enabled: true, Source: src,
		Config:      map[string]any{"url": r.URL},
		Credentials: map[string]CredentialRef{},
	}
	if r.Always {
		d.Config["always"] = true
	}
	switch r.Kind {
	case "", "basic":
		d.Kind = "oci"
		if r.Username != "" {
			d.Config["username"] = r.Username
		}
		if r.Layout != "" {
			d.Config["layout"] = r.Layout
		}
		if r.Insecure {
			d.Config["insecure"] = true
		}
		if password != (CredentialRef{}) {
			d.Credentials["password"] = password
		}
	case "ecr":
		d.Kind = "ecr"
		if r.Username != "" {
			d.Config["access_key_id"] = r.Username
		}
		if password != (CredentialRef{}) {
			d.Credentials["secret_access_key"] = password
		}
	default:
		return nil, fmt.Errorf("%s is %q. Valid answers: basic (a username and password) or ecr (an AWS access key)",
			where(sourceOf(v, "registry.kind", path)), r.Kind)
	}
	return append(decls, d), nil
}

// where names a setting's source for a startup error.
func where(src Source) string {
	if src.Kind == "env" {
		return src.Name
	}
	return "the config file " + src.Name + ", at " + src.Key + ","
}
