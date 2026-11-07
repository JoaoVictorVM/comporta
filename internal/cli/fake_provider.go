package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

const fakeDrainTimeout = 5 * time.Second

func newFakeProviderCommand(out streams) *cobra.Command {
	var port int
	var bind, scenarioName string
	command := &cobra.Command{
		Use:   "fake-provider",
		Short: "Run a local imitation of the Anthropic and OpenAI APIs, for tests without keys or cost",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			scenario, err := fakeprovider.ParseScenario(scenarioName)
			if err != nil {
				return err
			}
			if port < 1 || port > 65535 {
				return errors.New("--port must be between 1 and 65535")
			}
			addr, err := netip.ParseAddr(bind)
			if err != nil {
				return errors.New("--bind must be an IP address")
			}
			return runFakeProvider(command.Context(), out, net.JoinHostPort(addr.String(), strconv.Itoa(port)), scenario)
		},
	}
	command.Flags().IntVar(&port, "port", 9090, "port to listen on")
	command.Flags().StringVar(&bind, "bind", "127.0.0.1", "IP address to listen on")
	command.Flags().StringVar(&scenarioName, "scenario", string(fakeprovider.ScenarioSuccess),
		"success, rate-limit, overload, header-timeout, slow-stream or disconnect-midstream")
	return command
}

func runFakeProvider(ctx context.Context, out streams, address string, scenario fakeprovider.Scenario) error {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("could not listen on %s", address)
	}
	operations, cancelOperations := context.WithCancel(context.Background())
	defer cancelOperations()
	server := &http.Server{
		Handler:           fakeprovider.New(fakeprovider.Options{Scenario: scenario}),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return operations },
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	_, _ = fmt.Fprintf(out.stdout, "fake provider listening on %s, scenario %s\n", listener.Addr(), scenario)

	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("the fake provider stopped unexpectedly")
		}
		return nil
	case <-ctx.Done():
	}
	// Requests held open by a scenario would otherwise keep the drain waiting.
	cancelOperations()
	drain, cancelDrain := context.WithTimeout(context.Background(), fakeDrainTimeout)
	defer cancelDrain()
	if err := server.Shutdown(drain); err != nil {
		_ = server.Close()
	}
	<-served
	return nil
}
