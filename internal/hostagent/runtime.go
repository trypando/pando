package hostagent

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
)

// A joined host never exposes its container runtime's API (R-416). Its agent,
// on the one port the host publishes, carries the runtime calls a control
// host's runtime adapter makes to the host's own rootless socket, beside the
// proxy traffic it forwards. Only for a control host: an app host's
// certificate, or an agent's own, is refused before the socket is touched.

// runtimeTarget is the header line's word for the host's container runtime.
const runtimeTarget = "docker"

// isRuntimeRequest reports whether the header line asks for the runtime.
func isRuntimeRequest(line string) bool {
	fields := strings.Fields(line)
	return len(fields) == 2 && fields[0] == protocolVersion && fields[1] == runtimeTarget
}

// openRuntime connects to the host's runtime for a control host's
// certificate.
func (a *Agent) openRuntime(ctx context.Context, cs tls.ConnectionState) (net.Conn, error) {
	if a.RuntimeSocket == "" && a.DialRuntime == nil {
		return nil, errors.New("this agent does not carry container runtime calls")
	}
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 || !IsControl(cs.VerifiedChains[0][0]) {
		return nil, errors.New("container runtime calls are carried only for a control host of this cluster")
	}
	ctx, cancel := context.WithTimeout(ctx, headerTimeout)
	defer cancel()
	conn, err := a.dialRuntime(ctx)
	if err != nil {
		return nil, errors.New("the host's container runtime is not answering")
	}
	return conn, nil
}

func (a *Agent) dialRuntime(ctx context.Context) (net.Conn, error) {
	if a.DialRuntime != nil {
		return a.DialRuntime(ctx)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", a.RuntimeSocket)
}

// DialRuntime opens a stream through the agent at addr to its host's
// container runtime API, as the client tlsConfig authenticates: a control
// host's (IsControl). A Docker client dials one per connection it keeps.
func DialRuntime(ctx context.Context, addr string, tlsConfig *tls.Config) (net.Conn, error) {
	return dialAgent(ctx, addr, tlsConfig, protocolVersion+" "+runtimeTarget)
}
