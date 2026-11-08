package cli

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/metering"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/server"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
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
			catalog, err := pricing.Embedded()
			if err != nil {
				return err
			}
			configEvents := &metering.ConfigEvents{Logger: logger}
			manager, err := config.NewManager(config.ManagerOptions{Path: path, Logger: logger, Options: config.Options{Catalog: catalog}, Sink: configEvents})
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
			store, err := sqlite.Prepare(command.Context(), snapshot.DataDir())
			if err != nil {
				logger.Error(err.Error())
				return errReported
			}
			defer store.Close()
			identity, err := useridentity.Open(command.Context(), useridentity.Options{
				Dir: snapshot.DataDir(), History: store, ResetHistory: resetHistory, Logger: logger,
			})
			if err != nil {
				logger.Error(err.Error())
				return errReported
			}
			if err := store.Initialize(command.Context()); err != nil {
				return err
			}
			if err := store.Recover(command.Context(), time.Now()); err != nil {
				return err
			}
			configEvents.Publisher = store
			accounting := metering.New(store, catalog, logger, func() int64 { return manager.Capture().Storage().RetentionDays })
			if err := runGateway(command.Context(), logger, manager, keyManager, identity, accounting, configEvents.Run); err != nil {
				return err
			}
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = store.Checkpoint(shutdown)
			cancel()
			if closeErr := store.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				logger.Error("usage shutdown incomplete; journal retained for recovery")
				return errReported
			}
			logger.Info("shutdown complete")
			return nil
		},
	}
	command.Flags().BoolVar(&resetHistory, "reset-user-history", false,
		"Recover from a lost hmac.key by unlinking user history while preserving app totals; never replaces an existing key")
	return command
}

func runGateway(ctx context.Context, logger *slog.Logger, manager *config.Manager, keyManager *keys.Manager, identity *useridentity.Service, accounting *metering.Service, configWorker server.Worker) error {
	guard := auth.NewGuard(nil, logger)
	authHTTP := auth.NewHTTP(keyManager, auth.NewAuthenticator(keyManager, nil), guard)
	handler := proxy.NewHandler(proxy.Options{Auth: authHTTP, Identity: identity, Logger: logger, Accounting: accounting})
	defer handler.Close()
	gateway, err := server.Open(ctx, server.Options{
		Source:  manager,
		Proxy:   server.ProxySurface(manager, handler),
		Admin:   server.AdminSurface(manager),
		Workers: []server.Worker{manager.Run, keyManager.Run, accounting.Run, configWorker},
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
