package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
)

func newConfigCommand(out streams) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration files",
	}
	command.AddCommand(newValidateCommand(out))
	return command
}

func newValidateCommand(out streams) *cobra.Command {
	var resolveSecrets bool
	command := &cobra.Command{
		Use:   "validate [path]",
		Short: "Validate a configuration file",
		Long: "Validate syntax, structure and references. Secrets are not read unless " +
			"--resolve-secrets is set, so this works in CI without credentials.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			path, err := configPath(command, args)
			if err != nil {
				return err
			}
			return runValidate(out, path, resolveSecrets)
		},
	}
	command.Flags().BoolVar(&resolveSecrets, "resolve-secrets", false, "also read the environment, .env and secret files")
	return command
}

// configPath applies the precedence argument, then --config, then the default.
func configPath(command *cobra.Command, args []string) (string, error) {
	flag, err := command.Flags().GetString("config")
	if err != nil {
		return "", err
	}
	if len(args) == 0 {
		return flag, nil
	}
	if command.Flags().Changed("config") && !samePath(args[0], flag) {
		return "", errors.New("path argument and --config name different files")
	}
	return args[0], nil
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

func runValidate(streams streams, path string, resolveSecrets bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return errors.New("configuration path cannot be resolved")
	}
	catalog, err := pricing.Embedded()
	if err != nil {
		return err
	}
	opts := config.Options{
		Catalog:        catalog,
		ResolveSecrets: resolveSecrets,
		SystemTimezone: config.DetectSystemTimezone(),
	}
	if resolveSecrets {
		opts.Environment, err = config.CaptureEnvironment(filepath.Dir(absolute), os.Environ(), nil)
		if err != nil {
			printDiagnostics(streams.stderr, err)
			return errReported
		}
	}
	candidate, err := config.Load(absolute, opts)
	if err != nil {
		printDiagnostics(streams.stderr, err)
		return errReported
	}
	for _, warning := range candidate.Warnings() {
		_, _ = fmt.Fprintln(streams.stderr, "warning:", warning.String())
	}
	_, _ = fmt.Fprintln(streams.stdout, "configuration valid")
	return nil
}

func printDiagnostics(out io.Writer, err error) {
	var failure *config.Error
	if !errors.As(err, &failure) {
		_, _ = fmt.Fprintln(out, "error: configuration could not be loaded")
		return
	}
	for _, diagnostic := range failure.Diagnostics {
		if diagnostic.Line > 0 {
			_, _ = fmt.Fprintf(out, "%s (line %d, column %d)\n", diagnostic, diagnostic.Line, diagnostic.Column)
			continue
		}
		_, _ = fmt.Fprintln(out, diagnostic)
	}
}
