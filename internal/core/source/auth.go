package source

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// authMethod turns a source connection's credential into go-git's.
//
// Everything stays in this process: a token is an Authorization header on
// requests go-git makes, a key is held by its SSH client. Neither is written
// into the clone (R-112), so a checkout handed to the builder carries none.
func authMethod(c api.GitCredential, url string) (transport.AuthMethod, error) {
	switch {
	case !c.SSHPrivateKey.IsZero():
		ep, err := transport.NewEndpoint(url)
		if err != nil {
			return nil, errs.Wrap(errs.ValidInvalid, "The repository's SSH address could not be read.", err)
		}
		user := c.SSHUser
		if user == "" {
			user = "git"
		}
		keys, err := gitssh.NewPublicKeys(user, []byte(c.SSHPrivateKey.Reveal()), c.SSHPassphrase.Reveal())
		if err != nil {
			// The parse error can quote the key; it is not passed on.
			return nil, errs.New(errs.ValidInvalid,
				"The source connection's SSH private key could not be read.").
				WithRemedy("Replace the key in the connection's settings under Sources in the console.")
		}
		port := ep.Port
		if port == 0 {
			port = 22
		}
		helper, err := knownHosts(c.KnownHosts, net.JoinHostPort(ep.Host, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		keys.HostKeyCallbackHelper = helper
		return keys, nil
	case !c.BearerToken.IsZero():
		return &githttp.TokenAuth{Token: c.BearerToken.Reveal()}, nil
	case !c.Password.IsZero():
		user := c.Username
		if user == "" {
			user = "git"
		}
		return &githttp.BasicAuth{Username: user, Password: c.Password.Reveal()}, nil
	}
	return nil, nil
}

// knownHosts reads pinned host keys into the check go-git makes of the host,
// with the host key algorithms they name, so the host is asked for a key of a
// type that was pinned. go-git reads them from a file, so they are written to
// one for as long as that takes.
func knownHosts(text, hostPort string) (gitssh.HostKeyCallbackHelper, error) {
	var none gitssh.HostKeyCallbackHelper
	if strings.TrimSpace(text) == "" {
		return none, errs.New(errs.ValidInvalid,
			"The source connection uses an SSH key and pins no host key, so Pando cannot tell it is talking to the real host.").
			WithRemedy("Add the host's public key, as ssh-keyscan <host> prints it, to the connection's known hosts under Sources in the console.")
	}
	f, err := os.CreateTemp("", "pando-known-hosts-")
	if err != nil {
		return none, errs.Wrap(errs.Internal, "Could not prepare the SSH connection.", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(strings.TrimSpace(text) + "\n"); err != nil {
		_ = f.Close()
		return none, errs.Wrap(errs.Internal, "Could not prepare the SSH connection.", err)
	}
	if err := f.Close(); err != nil {
		return none, errs.Wrap(errs.Internal, "Could not prepare the SSH connection.", err)
	}
	db, err := gitssh.NewKnownHostsDb(f.Name())
	if err != nil {
		return none, errs.Wrap(errs.ValidInvalid,
			"The source connection's known hosts could not be read.", err).
			WithRemedy("Replace them with the output of ssh-keyscan <host> under Sources in the console.")
	}
	return gitssh.HostKeyCallbackHelper{
		HostKeyCallback:   db.HostKeyCallback(),
		HostKeyAlgorithms: db.HostKeyAlgorithms(hostPort),
	}, nil
}

// listRefs asks the repository what it has, without cloning.
func listRefs(ctx context.Context, acc *gitAccess) ([]*plumbing.Reference, error) {
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin", URLs: []string{acc.url},
	})
	return remote.ListContext(ctx, &git.ListOptions{Auth: acc.auth, CABundle: acc.caBundle})
}

// Probe checks that src can be read, with the credential its connection
// gives or anonymously, without cloning it. It is how creating an app from a
// private repository fails at once, saying why, rather than at detection.
//
// The caller has checked the source allowlist first (R-092).
func (s Sources) Probe(ctx context.Context, src spec.Source) error {
	if src.Type != spec.SourceGit || src.URL == "" {
		return nil
	}
	acc, err := s.access(ctx, src, PurposeCheck)
	if err != nil {
		return err
	}
	if _, err := listRefs(ctx, acc); err != nil {
		if denied := accessError(err, src.URL, acc.connection); denied != nil {
			return denied
		}
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return errs.Newf(errs.ValidInvalid, "The repository %s is empty, so there is nothing to deploy.", src.URL).
				WithRemedy("Push a commit to it, then add the app again.")
		}
		// Anything else — the host did not answer, a proxy got in the way —
		// is left for detection to report: the address may well be right.
		return nil
	}
	return nil
}

// accessError says why a repository could not be read, when the reason is
// access rather than the network (R-105). Nil for any other failure.
func accessError(err error, url, connection string) error {
	msg := strings.ToLower(err.Error())
	hostKey := strings.Contains(msg, "knownhosts") || strings.Contains(msg, "host key")
	authFailed := errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) ||
		strings.Contains(msg, "unable to authenticate")
	notFound := errors.Is(err, transport.ErrRepositoryNotFound)

	switch {
	case hostKey && connection != "":
		return errs.Newf(errs.SourceUnreadable,
			"Pando did not read %s: the host's SSH key is not the one the source connection %q pins, so it may not be the real host.",
			url, connection).
			WithDetail("url", url).
			WithRemedy("If the host's key really changed, replace the connection's known hosts with the output of ssh-keyscan <host> under Sources in the console.")
	case (authFailed || notFound) && connection != "":
		reason := "The connection's credential was not accepted. It may have expired or been revoked, or it may not have access to this repository."
		if notFound {
			reason = "The repository does not exist, or the connection's credential cannot see it."
		}
		return errs.Newf(errs.SourceUnreadable,
			"Pando could not read %s with the source connection %q. %s", url, connection, reason).
			WithDetail("url", url).
			WithRemedy("Give the credential read access to the repository, or replace it under Sources in the console, then try again.")
	case authFailed || notFound:
		return errs.Newf(errs.SourceUnreadable,
			"Pando could not read %s. The repository is private or does not exist, and no source connection on this installation covers it.", url).
			WithDetail("url", url).
			WithRemedy("Check the address. If the repository is private, connect the host it is on under Sources in the console, then try again.")
	}
	return nil
}
