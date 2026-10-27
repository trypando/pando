package hosts

import (
	"encoding/base64"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

// The cluster listener's JSON (R-415). Plain strings, because secret.Value
// redacts itself in every marshaler (R-194) and these bodies exist to carry
// secrets: the join token's, and a control host's keys. Built only at the
// listener and the joining host, never logged.

// Paths on the cluster listener.
const (
	AuthorityPath = "/cluster/authority"
	JoinPath      = "/cluster/join"
	// PeersPath lists the cluster's hosts to a host that has joined, which
	// presents its own certificate (Service.Peers, R-421).
	PeersPath = "/cluster/peers"
	// RenewPath issues a joined host new certificates before its own expire,
	// to the host presenting the one it holds (Service.Renew, R-421).
	RenewPath = "/cluster/renew"
)

// WireRenewRequest is POST /cluster/renew's body: a certificate request for
// the host's next key.
type WireRenewRequest struct {
	CSR string `json:"csr"`
}

// WireRenewResult is POST /cluster/renew's answer. HostClientKey is the
// probe certificate's key, sent only over the mutually authenticated
// connection.
type WireRenewResult struct {
	Host           state.Host `json:"host"`
	Certificate    string     `json:"certificate"`
	HostClientCert string     `json:"host_client_certificate"`
	HostClientKey  string     `json:"host_client_key"`
}

// Wire is the renewal as the listener sends it.
func (r RenewResult) Wire() WireRenewResult {
	return WireRenewResult{
		Host: r.Host, Certificate: string(r.Certificate),
		HostClientCert: string(r.HostClientCert), HostClientKey: r.HostClientKey.Reveal(),
	}
}

// WireJoinRequest is POST /cluster/join's body.
type WireJoinRequest struct {
	Secret   string `json:"secret"`
	Name     string `json:"name"`
	CSR      string `json:"csr"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
	Address  string `json:"address"`
}

// Request is the body as core reads it.
func (w WireJoinRequest) Request() JoinRequest {
	return JoinRequest{
		Secret: secret.New(w.Secret), Name: w.Name, CSR: []byte(w.CSR),
		Platform: w.Platform, Version: w.Version, Address: w.Address,
	}
}

// WireKeys is a control host's keys on the wire. Key files are base64, since
// a key is bytes.
type WireKeys struct {
	DatabaseURL      string `json:"database_url"`
	SecretsKey       string `json:"secrets_key,omitempty"`
	SecretsKeyPath   string `json:"secrets_key_path,omitempty"`
	TokenKey         string `json:"token_key"`
	ClusterAuthority string `json:"cluster_authority"`
}

// WireJoinResult is POST /cluster/join's answer.
type WireJoinResult struct {
	Host           state.Host `json:"host"`
	Certificate    string     `json:"certificate"`
	Authority      string     `json:"authority"`
	HostClientCert string     `json:"host_client_certificate,omitempty"`
	HostClientKey  string     `json:"host_client_key,omitempty"`
	NetworkPool    string     `json:"network_pool,omitempty"`
	Keys           *WireKeys  `json:"keys,omitempty"`
}

func b64(v secret.Value) string {
	if v.IsZero() {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(v.Reveal()))
}

func unb64(s string) (secret.Value, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return secret.Value{}, err
	}
	return secret.New(string(b)), nil
}

// Wire is the result as the listener sends it.
func (r JoinResult) Wire() WireJoinResult {
	out := WireJoinResult{
		Host: r.Host, Certificate: string(r.Certificate), Authority: string(r.Authority),
		HostClientCert: string(r.HostClientCert), HostClientKey: r.HostClientKey.Reveal(), NetworkPool: r.NetworkPool,
	}
	if r.Keys != nil {
		out.Keys = &WireKeys{
			DatabaseURL:      r.Keys.DatabaseURL.Reveal(),
			SecretsKey:       b64(r.Keys.SecretsKey),
			SecretsKeyPath:   r.Keys.SecretsKeyPath,
			TokenKey:         b64(r.Keys.TokenKey),
			ClusterAuthority: r.Keys.Authority.Reveal(),
		}
	}
	return out
}

// result is the answer as the joining host reads it.
func (w WireJoinResult) result() (JoinResult, error) {
	out := JoinResult{
		Host: w.Host, Certificate: []byte(w.Certificate), Authority: []byte(w.Authority),
		HostClientCert: []byte(w.HostClientCert), HostClientKey: secret.New(w.HostClientKey), NetworkPool: w.NetworkPool,
	}
	if w.Keys == nil {
		return out, nil
	}
	k := &ControlKeys{
		DatabaseURL:    secret.New(w.Keys.DatabaseURL),
		SecretsKeyPath: w.Keys.SecretsKeyPath,
		Authority:      secret.New(w.Keys.ClusterAuthority),
	}
	var err error
	if k.TokenKey, err = unb64(w.Keys.TokenKey); err != nil {
		return out, err
	}
	if k.SecretsKey, err = unb64(w.Keys.SecretsKey); err != nil {
		return out, err
	}
	out.Keys = k
	return out, nil
}
