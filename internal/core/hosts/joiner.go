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

	Now func() time.Time
}

// Setup is the join a fresh Pando's setup makes (R-415): POST /setup/join and
// PANDO_JOIN_TOKEN. It joins, then asks for the restart that takes this
// Pando into the cluster.
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
	if err := checkIssued(res, key, d.AuthorityHash); err != nil {
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
	joined := Joined{HostID: res.Host.ID, Name: res.Host.Name, Roles: res.Host.Roles, Cluster: d.Address, JoinedAt: j.now().UTC()}
	if err := j.Files.write(joined, res, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return Joined{}, err
	}
	return joined, nil
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
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
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
	payload, err := json.Marshal(body) //nolint:gosec // G117: the join token's secret, sent only over the pinned connection (R-415)
	if err != nil {
		return JoinResult{}, errs.Wrap(errs.Internal, "Pando could not make the join request.", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Address+JoinPath, bytes.NewReader(payload))
	if err != nil {
		return JoinResult{}, errs.Wrap(errs.ValidInvalid, fmt.Sprintf("The join token's address %s is not one Pando can connect to.", d.Address), err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.client(PinnedTLS(d.AuthorityHash)).Do(req)
	if err != nil {
		return JoinResult{}, connectError(d.Address, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return JoinResult{}, errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf("The connection to the cluster at %s ended during the join.", d.Address), err).
			WithRemedy(joinRemedy)
	}
	if resp.StatusCode != http.StatusCreated {
		return JoinResult{}, refusal(d.Address, resp.StatusCode, data)
	}
	var wire WireJoinResult
	if err := json.Unmarshal(data, &wire); err != nil {
		return JoinResult{}, errs.Wrap(errs.Internal, fmt.Sprintf("The cluster at %s answered the join with something Pando cannot read.", d.Address), err)
	}
	res, err := wire.result()
	if err != nil {
		return JoinResult{}, errs.Wrap(errs.Internal, fmt.Sprintf("The cluster at %s sent keys Pando cannot read.", d.Address), err)
	}
	return res, nil
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
func checkIssued(res JoinResult, key *ecdsa.PrivateKey, hash string) error {
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
	if err := verifyPinned([]*x509.Certificate{cert, ca}, hash); err != nil {
		// verifyPinned checks server authentication; a host certificate has
		// it, so a failure here is a certificate from another authority.
		return bad("it is not from the authority the join token names")
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return bad("it is for another key")
	}
	return nil
}
