package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/server"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

func newServeCommand(out streams) *cobra.Command {
	var resetHistory bool
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway: the proxy and admin listeners, until interrupted",
		Long: "Load the configuration and app keys, open the proxy and admin listeners and apply " +
			"saved changes without a restart. Create a key first with \"comporta keys create <app>\".",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, err := command.Flags().GetString("config")
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewJSONHandler(out.stderr, nil))
			manager, err := config.NewManager(config.ManagerOptions{Path: path, Logger: logger})
			if err != nil {
				logStartupFailure(logger, err)
				return errReported
			}
			snapshot := manager.Capture()
			keyManager, err := keys.NewManager(keys.ManagerOptions{Dir: snapshot.DataDir(), Logger: logger})
			if err != nil {
				logger.Error(err.Error())
				return errReported
			}
			// Before the listeners: without a key no request can be pseudonymized, and
			// this failure does not depend on the financial fail mode.
			identity, err := useridentity.Open(command.Context(), useridentity.Options{
				Dir: snapshot.DataDir(), ResetHistory: resetHistory, Logger: logger,
			})
			if err != nil {
				logger.Error(err.Error())
				return errReported
			}
			return runGateway(command.Context(), logger, manager, keyManager, identity)
		},
	}
	command.Flags().BoolVar(&resetHistory, "reset-user-history", false,
		"Recover from a lost hmac.key. Refused while usage data exists: it never deletes files and never replaces a key")
	return command
}

func runGateway(ctx context.Context, logger *slog.Logger, manager *config.Manager, keyManager *keys.Manager, identity *useridentity.Service) error {
	guard := auth.NewGuard(nil, logger)
	authHTTP := auth.NewHTTP(keyManager, auth.NewAuthenticator(keyManager, nil), guard)
	handler := proxy.NewHandler(proxy.Options{Auth: authHTTP, Identity: identity, Logger: logger})
	defer handler.Close()
	gateway, err := server.Open(ctx, server.Options{
		Source:  manager,
		Proxy:   server.ProxySurface(manager, handler),
		Admin:   server.AdminSurface(manager),
		Workers: []server.Worker{manager.Run, keyManager.Run},
		Logger:  logger,
	})
	if err != nil {
		logger.Error(err.Error())
		return errReported
	}
	snapshot := manager.Capture()
	logger.Info("configuration active",
		"generation", snapshot.Generation(), "apps", len(snapshot.AppNames()), "providers", len(snapshot.ProviderNames()))
	if err := gateway.Run(ctx); err != nil {
		logger.Error(err.Error())
		return errReported
	}
	logger.Info("shutdown complete")
	return nil
}

func logStartupFailure(logger *slog.Logger, err error) {
	var failure *config.Error
	if !errors.As(err, &failure) {
		logger.Error("configuration could not be loaded")
		return
	}
	for _, diagnostic := range failure.Diagnostics {
		logger.Error(diagnostic.String(), "code", diagnostic.Code)
	}
}
