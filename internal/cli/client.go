// Package cli is the command-line client (design 04 §4).
//
// A client of the API and nothing more (R-261). The CLI must not have a
// capability the API lacks, and the way to keep that true is to give it no
// access to anything else: this package talks HTTP and does not import
// internal/core at all. If a command here needs something the API cannot do,
// the answer is an endpoint, not a shortcut.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to a Pando server.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Warn receives the version-skew warning (R-353). Nil says nothing.
	Warn io.Writer
}

// Credentials are what `pando login` stores.
//
// A token rather than a session cookie: a session is bound to a browser's
// lifetime and revoked on a password change, which is right for a browser and
// wrong for a script that runs at 3am. The token is delegated, so it acts as
// its owner and holds nothing they do not (R-058).
type Credentials struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	User  string `json:"user,omitempty"`
}

// credentialsPath is where the token lives.
//
// Under XDG_CONFIG_HOME so it is not in the shell history, not in the repo, and
// not in an environment variable that every child process inherits.
func credentialsPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding your home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "pando", "credentials.json"), nil
}

// SaveCredentials writes the token, readable only by its owner.
func SaveCredentials(c Credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// 0600, and the directory 0700. A token is a bearer credential: anything
	// that can read this file can act as the person who created it.
	return os.WriteFile(path, body, 0o600)
}

// LoadCredentials reads the stored token.
func LoadCredentials() (Credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return Credentials{}, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, ErrNotLoggedIn
	}
	if err != nil {
		return Credentials{}, err
	}

	var c Credentials
	if err := json.Unmarshal(body, &c); err != nil {
		return Credentials{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return c, nil
}

// ErrNotLoggedIn is returned when there is no stored credential.
var ErrNotLoggedIn = errors.New("not logged in — run `pando login` first, or set PANDO_SERVER and PANDO_TOKEN")

// The environment a machine authenticates with.
//
// `pando login` is for a person at a terminal: it stores a token in a file
// under their home directory, which is the right place for it and the wrong
// place for CI, a container, or an MCP client's configuration block. Those have
// an environment and no home directory worth writing to, and without this the
// only way to use a minted token was to write the credentials file by hand.
//
// Named constants because the reference quotes them: `docs/cli.md` and the
// console's API screen are generated, and a documented variable name that
// nothing reads is the drift this whole mechanism exists to prevent.
const (
	EnvServer = "PANDO_SERVER"
	EnvToken  = "PANDO_TOKEN"
)

// New builds a client from stored credentials, with the server URL overridable
// so a script can point at a different install without logging in again.
func New(urlOverride string) (*Client, error) {
	// The environment wins over the stored file, so a script or a container
	// gets what it was given rather than whatever the image happened to be
	// built with. The flag wins over both, because it is the most deliberate
	// of the three.
	envServer, envToken := os.Getenv(EnvServer), os.Getenv(EnvToken)

	creds, err := LoadCredentials()
	if err != nil && urlOverride == "" && envServer == "" {
		return nil, err
	}

	base := creds.URL
	if envServer != "" {
		base = envServer
	}
	if urlOverride != "" {
		base = urlOverride
	}
	if base == "" {
		return nil, ErrNotLoggedIn
	}

	token := creds.Token
	if envToken != "" {
		token = envToken
	}

	return &Client{
		BaseURL: strings.TrimSuffix(base, "/"),
		Token:   token,
		// Generous: a deploy resolves a ref, which means cloning, and a backup
		// streams a database dump plus every volume. Neither is a quick call
		// and neither should be cut off by a default nobody chose.
		HTTP: &http.Client{Timeout: 10 * time.Minute},
		Warn: os.Stderr,
	}, nil
}

// APIError is the server's error envelope (design 00 §3.2).
//
// Rendered as written. The messages are held to the R-105 standard — self
// contained, actionable, pasteable into an assistant — and paraphrasing them
// here would undo that at the last step.
type APIError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
	ReqID   string `json:"request_id,omitempty"`
}

func (e *APIError) Error() string {
	if e.Remedy != "" {
		return e.Message + "\n" + e.Remedy
	}
	return e.Message
}

// Do makes a request and decodes the response into out.
func (c *Client) Do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.BaseURL+"/api/v1"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", c.BaseURL, err)
	}
	warnSkew(c.Warn, c.BaseURL, resp)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		apiErr := &APIError{Status: resp.StatusCode}
		raw, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(raw, apiErr); err != nil || apiErr.Message == "" {
			apiErr.Message = fmt.Sprintf("The server returned %d.", resp.StatusCode)
		}
		return apiErr
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// decodeInto unmarshals an error envelope, tolerating a body that is not one.
func decodeInto(raw []byte, out any) error { return json.Unmarshal(raw, out) }

// Stream opens a response body for streaming, for logs and other long reads.
func (c *Client) Stream(method, path string, body any) (io.ReadCloser, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.BaseURL+"/api/v1"+path, reader)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	// No timeout on a stream: `pando logs -f` is supposed to sit there.
	streaming := &http.Client{}
	resp, err := streaming.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching %s: %w", c.BaseURL, err)
	}
	warnSkew(c.Warn, c.BaseURL, resp)
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		apiErr := &APIError{Status: resp.StatusCode}
		raw, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(raw, apiErr); err != nil || apiErr.Message == "" {
			apiErr.Message = fmt.Sprintf("The server returned %d.", resp.StatusCode)
		}
		return nil, apiErr
	}
	return resp.Body, nil
}
