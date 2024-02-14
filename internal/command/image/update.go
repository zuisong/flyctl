package image

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	fly "github.com/superfly/fly-go"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/command/apps"
	"github.com/superfly/flyctl/internal/flag"
)

func newUpdate() *cobra.Command {
	const (
		long = `This will update the application's image to the latest available version.
The update will perform a rolling restart against each VM, which may result in a brief service disruption.`
		short = "Updates the app's image to the latest available version. (Fly Postgres only)"
		usage = "update"
	)

	cmd := command.New(usage, short, long, runUpdate,
		command.RequireSession,
		command.RequireAppName,
	)

	cmd.Args = cobra.NoArgs

	flag.Add(cmd,
		flag.App(),
		flag.AppConfig(),
		flag.Yes(),
		flag.String{
			Name:        "image",
			Description: "Target a specific image",
		},
		flag.Bool{
			Name:        "skip-health-checks",
			Description: "Skip waiting for health checks inbetween VM updates. (Machines only)",
			Default:     false,
		},
	)

	return cmd
}

func runUpdate(ctx context.Context) error {
	var (
		appName = appconfig.NameFromContext(ctx)
		client  = fly.ClientFromContext(ctx)
	)

	app, err := client.GetAppCompact(ctx, appName)
	if err != nil {
		return fmt.Errorf("get app: %w", err)
	}

	ctx, err = apps.BuildContext(ctx, app)
	if err != nil {
		return err
	}

	if app.IsPostgresApp() {
		return updatePostgresOnMachines(ctx, app)
	}
	return updateImageForMachines(ctx, app)
}
