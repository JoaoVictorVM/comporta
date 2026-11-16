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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

const fakeDrainTimeout = 5 * time.Second

func newFakeProviderCommand(out streams) *cobra.Command {
	var port int
	var bind, scenarioName string
	var modelScenarios []string
	command := &cobra.Command{
		Use:   "fake-provider",
		Short: "Run a local imitation of the Anthropic and OpenAI APIs, for tests without keys or cost",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			scenario, err := fakeprovider.ParseScenario(scenarioName)
			if err != nil {
				return err
			}
			byModel := map[string]fakeprovider.Scenario{}
			for _, entry := range modelScenarios {
				model, name, ok := strings.Cut(entry, "=")
				if !ok || model == "" {
					return errors.New("--model-scenario must be MODEL=SCENARIO")
				}
				if byModel[model], err = fakeprovider.ParseScenario(name); err != nil {
					return err
				}
			}
			if port < 1 || port > 65535 {
				return errors.New("--port must be between 1 and 65535")
			}
			addr, err := netip.ParseAddr(bind)
			if err != nil {
				return errors.New("--bind must be an IP address")
			}
			return runFakeProvider(command.Context(), out, net.JoinHostPort(addr.String(), strconv.Itoa(port)), fakeprovider.Options{Scenario: scenario, ByModel: byModel})
		},
	}
	command.Flags().IntVar(&port, "port", 9090, "port to listen on")
	command.Flags().StringVar(&bind, "bind", "127.0.0.1", "IP address to listen on")
	command.Flags().StringVar(&scenarioName, "scenario", string(fakeprovider.ScenarioSuccess),
		"success, rate-limit, overload, header-timeout, slow-stream, disconnect-midstream, agent-loop, server-error, bad-request, stream-error-before-chunk or stream-error-after-chunk")
	command.Flags().StringArrayVar(&modelScenarios, "model-scenario", nil, "MODEL=SCENARIO overriding the scenario for one model; repeatable, to play a fallback chain")
	return command
}

func runFakeProvider(ctx context.Context, out streams, address string, options fakeprovider.Options) error {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("could not listen on %s", address)
	}
	operations, cancelOperations := context.WithCancel(context.Background())
	defer cancelOperations()
	server := &http.Server{
		Handler:           fakeprovider.New(options),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return operations },
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	_, _ = fmt.Fprintf(out.stdout, "fake provider listening on %s, scenario %s\n", listener.Addr(), options.Scenario)

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
