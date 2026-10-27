package hosts

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Joiner is the joining side of R-415: a fresh Pando taking a join token,
// checking the cluster's authority against it, and being made a host.
type Joiner struct {
	// Fresh reports whether this Pando has had an account or an app; a Pando
	// that has cannot join (R-415).
	Fresh func(ctx context.Context) (accounts, apps bool, err error)

	Files JoinFiles

	// Version and Platform are this binary's; AgentAddress is where this
	// host's agent will listen, host:port (R-416).
	Version      string
	Platform     string
	AgentAddress string

	// HTTP makes the client a join connects with, from the pinned TLS
	// configuration. Nil is a client with a one-minute timeout.
	HTTP func(*tls.Config) *http.Client

	// DefaultName is the host's name when the join gives none: this
	// machine's hostname.
	DefaultName string

	// Restart starts Pando again once a join made at setup has been answered,
	// into the cluster: as a replica for the control role, as a host agent
	// for the app role alone.
	Restart func()

	// SetupToken checks the setup token a join from the setup screen presents
	// (state.Users.CheckSetupToken): the one POST /setup needs, so that being
	// first to a fresh Pando is not enough to join it to a cluster of one's
	// own (R-046, R-415). Nil refuses every such join.
	SetupToken func(ctx context.Context, token secret.Value) error

	Now func() time.Time
}

// SetupWithToken is the join POST /setup/join makes (R-415): refused unless
// setupToken is this Pando's setup token, checked before anything else.
func (j *Joiner) SetupWithToken(ctx context.Context, setupToken secret.Value, token, name string) (Joined, error) {
	if j.SetupToken == nil {
		return Joined{}, errs.New(errs.Internal, "This Pando cannot check its setup token, so it cannot join a cluster from the setup screen.").
			WithRemedy("Join with PANDO_JOIN_TOKEN set where Pando runs instead.")
	}
	if err := j.SetupToken(ctx, setupToken); err != nil {
		return Joined{}, err
	}
	return j.Setup(ctx, token, name)
}

// Setup is the join a fresh Pando's setup makes (R-415): SetupWithToken and
// PANDO_JOIN_TOKEN, which needs no setup token, since setting it takes the
// host already. It joins, then asks for the restart that takes this Pando
// into the cluster.
func (j *Joiner) Setup(ctx context.Context, token, name string) (Joined, error) {
	if strings.TrimSpace(name) == "" {
		name, _, _ = strings.Cut(strings.ToLower(j.DefaultName), ".")
	}
	joined, err := j.Join(ctx, token, name)
	if err != nil {
		return Joined{}, err
	}
	if j.Restart != nil {
		j.Restart()
	}
	return joined, nil
}

const joinRemedy = "Create a new join token on a host already in the cluster and start the join again."

// Join joins the cluster token names, as name, and writes what it was given
// (JoinFiles). It does not restart Pando; the caller does, once it has
// answered.
func (j *Joiner) Join(ctx context.Context, token, name string) (Joined, error) {
	d, err := Decode(token)
	if err != nil {
		return Joined{}, err
	}
	name = strings.TrimSpace(name)
	if err := CheckHostName(name); err != nil {
		return Joined{}, err
	}
	if err := j.checkFresh(ctx); err != nil {
		return Joined{}, err
	}

	key, csr, err := newHostKey(name)
	if err != nil {
		return Joined{}, errs.Wrap(errs.Internal, "Pando could not make this host's key.", err)
	}
	res, err := j.post(ctx, d, WireJoinRequest{
		Secret: d.Secret.Reveal(), Name: name, CSR: string(csr),
		Platform: j.Platform, Version: j.Version, Address: j.AgentAddress,
	})
	if err != nil {
		return Joined{}, err
	}
	if err := checkIssued(res, key, d.AuthorityHash, j.now()); err != nil {
		return Joined{}, err
	}
	// An app host never keeps a control host's keys, whatever it is sent
	// (R-415).
	if !slices.Contains(res.Host.Roles, state.HostRoleControl) {
		res.Keys = nil
	} else if res.Keys == nil {
		return Joined{}, errs.New(errs.Internal, "The cluster made this host a control host but sent none of the keys a control host needs.").
			WithRemedy("Remove the host from the cluster, create a new join token and start the join again.")
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Joined{}, errs.Wrap(errs.Internal, "Pando could not keep this host's key.", err)
	}
	joined := Joined{
		HostID: res.Host.ID, Name: res.Host.Name, Roles: res.Host.Roles, Cluster: d.Address,
		JoinedAt: j.now().UTC(), NetworkPool: res.NetworkPool,
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := j.Files.write(joined, res, keyPEM); err != nil {
		return Joined{}, err
	}
	if err := j.confirm(ctx, d, res.Certificate, keyPEM); err != nil {
		// Not joined: the cluster forgets the pending join, and the same
		// token and name can join again (R-415).
		j.Files.unmark()
		return Joined{}, err
	}
	return joined, nil
}

// confirmTries is how many times a joined host asks the cluster listener to
// confirm its join before it gives up.
const confirmTries = 3

// confirm presents the certificate the join issued to the cluster listener
// (Service.Peers), once what the join gave this host is written: until then
// the cluster holds the join pending, lists no such host, and lets the same
// name join again (R-415). A confirmation whose answer was lost is asked
// again; a confirmed join answers again.
func (j *Joiner) confirm(ctx context.Context, d Decoded, certPEM, keyPEM []byte) error {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not use the certificate the cluster issued this host.", err).WithRemedy(joinRemedy)
	}
	cfg := PinnedTLS(d.AuthorityHash)
	cfg.Certificates = []tls.Certificate{cert}
	sync := &PeerSync{Address: d.Address, HTTP: j.client(cfg)}
	for range confirmTries {
		if _, err = sync.fetch(ctx, d.Address); err == nil {
			return nil
		}
	}
	return errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("The cluster at %s answered the join, but this host could not confirm it, so it has not joined.", d.Address), err).
		WithRemedy("Start the join again with the same join token and name. The unfinished join is replaced.")
}

func (j *Joiner) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// checkFresh refuses a Pando that is already something (R-415).
func (j *Joiner) checkFresh(ctx context.Context) error {
	if _, done, err := j.Files.Read(); err != nil || done {
		if err != nil {
			return err
		}
		return errs.New(errs.StateInvalid, "This Pando has already joined a cluster.").
			WithRemedy("To move it to another cluster, remove it from its cluster and reinstall Pando on it.")
	}
	accounts, apps, err := j.Fresh(ctx)
	if err != nil {
		return err
	}
	if accounts || apps {
		return errs.New(errs.StateInvalid, "This Pando already has accounts or apps, so it cannot join a cluster. Only a new installation can join one; joining would merge two installations.").
			WithRemedy("Join from a new installation of Pando with an empty database.")
	}
	return nil
}

// newHostKey makes this host's key and a certificate request for it.
func newHostKey(name string) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := hostCSR(key, name)
	return key, csr, err
}

// hostCSR is a certificate request for key, named name.
func hostCSR(key *ecdsa.PrivateKey, name string) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

func (j *Joiner) client(cfg *tls.Config) *http.Client {
	if j.HTTP != nil {
		return j.HTTP(cfg)
	}
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: cfg, Proxy: http.ProxyFromEnvironment}}
}

// post sends the join over a connection pinned to the token's authority: the
// secret goes nowhere else (R-415).
func (j *Joiner) post(ctx context.Context, d Decoded, body WireJoinRequest) (JoinResult, error) {
	var wire WireJoinResult
	if err := exchange(ctx, j.client(PinnedTLS(d.AuthorityHash)), d.Address, JoinPath, body, &wire); err != nil {
		return JoinResult{}, err
	}
	res, err := wire.result()
	if err != nil {
		return JoinResult{}, errs.Wrap(errs.Internal, fmt.Sprintf("The cluster at %s sent keys Pando cannot read.", d.Address), err)
	}
	return res, nil
}

// exchange posts body to path on the cluster listener at address, as JSON,
// and reads its 201 answer into out.
func exchange(ctx context.Context, client *http.Client, address, path string, body, out any) error {
	payload, err := json.Marshal(body) //nolint:gosec // G117: the join token's secret, sent only over the pinned connection (R-415)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not make the request to the cluster.", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address+path, bytes.NewReader(payload))
	if err != nil {
		return errs.Wrap(errs.ValidInvalid, fmt.Sprintf("The cluster's address %s is not one Pando can connect to.", address), err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return connectError(address, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("The connection to the cluster at %s ended before it answered.", address), err).
			WithRemedy(joinRemedy)
	}
	if resp.StatusCode != http.StatusCreated {
		return refusal(address, resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errs.Wrap(errs.Internal, fmt.Sprintf("The cluster at %s answered with something Pando cannot read.", address), err)
	}
	return nil
}

// connectError says why the cluster could not be reached, and in particular
// when it was reached and is not the cluster the token was made for.
func connectError(address string, err error) error {
	if errors.Is(err, errAuthorityMismatch) {
		return errs.New(errs.AuthInvalid, fmt.Sprintf("The server at %s is not the cluster this join token was made for: its certificate authority does not match the token. Nothing was sent to it.", address)).
			WithRemedy("Check the address, or create a new join token on a host already in the cluster.")
	}
	return errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("Pando could not reach the cluster at %s.", address), err).
		WithRemedy(fmt.Sprintf("Check that this host can reach %s and that the port is open, then start the join again.", address))
}

// refusal is the cluster's refusal, passed on as it said it.
func refusal(address string, status int, body []byte) error {
	var e errs.Error
	if json.Unmarshal(body, &e) == nil && e.Code != "" && e.Message != "" {
		out := errs.New(e.Code, e.Message)
		if e.Remedy != "" {
			out = out.WithRemedy(e.Remedy)
		}
		return out
	}
	return errs.Newf(errs.AdapterFailed, "The cluster at %s refused the join with HTTP status %d.", address, status).WithRemedy(joinRemedy)
}

// checkIssued checks what the cluster issued: a certificate for this host's
// own key, from the authority the token pins.
func checkIssued(res JoinResult, key *ecdsa.PrivateKey, hash string, at time.Time) error {
	bad := func(what string) error {
		return errs.Newf(errs.Internal, "The cluster sent a certificate Pando cannot use: %s.", what).WithRemedy(joinRemedy)
	}
	caBlock, _ := pem.Decode(res.Authority)
	certBlock, _ := pem.Decode(res.Certificate)
	if caBlock == nil || certBlock == nil {
		return bad("it is not PEM")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return bad("its authority cannot be read")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return bad("it cannot be read")
	}
	if err := verifyPinned([]*x509.Certificate{cert, ca}, hash, at); err != nil {
		// verifyPinned checks server authentication; a host certificate has
		// it, so a failure here is a certificate from another authority.
		return bad("it is not from the authority the join token names")
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return bad("it is for another key")
	}
	return nil
}
