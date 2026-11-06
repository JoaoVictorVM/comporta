// Package cli implements the comporta command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

const defaultConfigPath = "comporta.yaml"

// errReported marks a failure whose diagnostics have already been written.
var errReported = errors.New("failure already reported")

type streams struct {
	stdout io.Writer
	stderr io.Writer
}

// Execute runs the command line and returns the process exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRootCommand(streams{stdout: stdout, stderr: stderr})
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(stderr, "error:", err)
		}
		return 1
	}
	return 0
}

func newRootCommand(out streams) *cobra.Command {
	root := &cobra.Command{
		Use:   "comporta",
		Short: "Configuration runtime of the Comporta LLM gateway",
		// Cobra must not print raw errors: the commands write sanitized
		// diagnostics themselves.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(out.stdout)
	root.SetErr(out.stderr)
	root.PersistentFlags().String("config", defaultConfigPath, "path to the configuration file")
	root.AddCommand(newConfigCommand(out))
	return root
}
