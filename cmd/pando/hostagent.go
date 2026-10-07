package main

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/log"
)

// hostAgentCmd is the forwarding agent each app host of a multi-host Docker
// install runs (O-45, design 06 §4), and the command that makes the
// certificate authority Pando and the agents authenticate each other with.
func hostAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host-agent",
		Short: "The forwarding agent on each host of a multi-host Docker install",
		Long: "Each app host of a multi-host Docker install runs a forwarding agent, which carries " +
			"connections from Pando's proxy, and only from it, to app containers on that host. " +
			"The Docker hosts runtime starts and replaces the agents itself; the one command here " +
			"an operator runs is new-authority.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(hostAgentServeCmd(), hostAgentAuthorityCmd())
	return cmd
}

// hostAgentServeCmd runs the agent. Hidden: the runtime adapter starts it,
// from Pando's own image, with everything it needs in its environment. It
// needs no database and no container runtime socket.
func hostAgentServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "serve",
		Short:  "Run the host agent",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			certPEM := os.Getenv(multidocker.EnvAgentCert)
			keyPEM := os.Getenv(multidocker.EnvAgentKey)
			authorities := os.Getenv(multidocker.EnvAgentAuthorities)
			if certPEM == "" || keyPEM == "" || authorities == "" {
				return fmt.Errorf("the host agent was started without its certificate, key or authority, so it did not start. Pando sets %s, %s and %s when it starts the agent",
					multidocker.EnvAgentCert, multidocker.EnvAgentKey, multidocker.EnvAgentAuthorities)
			}
			tlsConfig, err := hostagent.ServerTLS([]byte(certPEM), []byte(keyPEM), []byte(authorities))
			if err != nil {
				return fmt.Errorf("the host agent did not start: %w", err)
			}
			pool, err := netip.ParsePrefix(os.Getenv(multidocker.EnvAgentPool))
			if err != nil {
				return fmt.Errorf("the host agent did not start: %s is not an address range: %w", multidocker.EnvAgentPool, err)
			}
			listen := os.Getenv(multidocker.EnvAgentListen)
			if listen == "" {
				listen = ":" + strconv.Itoa(hostagent.DefaultPort)
			}

			logger, err := log.New("info", false)
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("the host agent could not listen on %s: %w", listen, err)
			}
			logger.Info("host agent listening", zap.String("addr", listen), zap.String("pool", pool.String()))
			agent := &hostagent.Agent{TLS: tlsConfig, Pool: pool.Masked(), Logger: logger}
			return agent.Serve(ctx, ln)
		},
	}
}

// hostAgentAuthorityCmd prints a new certificate authority for the agents.
func hostAgentAuthorityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new-authority",
		Short: "Print a new certificate authority for the host agents",
		Long: "Prints a certificate authority, certificate and private key, as PEM. Paste it into the " +
			"Docker hosts runtime's agent_authority credential, which Pando stores encrypted. Pando " +
			"issues its own client certificate and each agent's certificate from it. To replace it, " +
			"put a new one before the old one in the credential, wait for the agents to be replaced, " +
			"then remove the old one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pem, err := hostagent.NewAuthority()
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(pem)
			return err
		},
	}
}
