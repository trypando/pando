package sourceconn

import (
	"context"
	"net"
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

// WebhookSecret is the secret a connection's own webhook is signed with, and
// whether it has one (api.SourceCapabilities.Webhook). A connection that does
// not exist has none, so a webhook for it is refused like any unsigned one.
func (s *Service) WebhookSecret(ctx context.Context, id string) (secret.Value, bool, error) {
	if s == nil || s.Credentials == nil || !s.stored(id) {
		return secret.Value{}, false, nil
	}
	c, err := s.Get(ctx, id)
	if err != nil || !c.Capabilities.Webhook {
		return secret.Value{}, false, nil //nolint:nilerr // no such connection is no secret, not a failure
	}
	creds, err := s.Credentials.Resolve(ctx, id)
	if err != nil {
		return secret.Value{}, false, err
	}
	v := creds[api.CredentialWebhookSecret]
	return v, !v.IsZero(), nil
}

// Public says whether an address could be reached from the internet: an
// https or http address whose host is a name with a dot in it, not a
// loopback, private or link-local one, or a public IP address. A provider
// such as github.com refuses a webhook at any other, and the App with it.
func Public(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
	}
	if !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}
