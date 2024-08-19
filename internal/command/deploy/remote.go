package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/azazeal/pause"
	"github.com/cenkalti/backoff"
	"github.com/superfly/fly-go"
	"github.com/superfly/fly-go/flaps"
	"github.com/superfly/flyctl/gql"
	"github.com/superfly/flyctl/internal/config"
	"github.com/superfly/flyctl/internal/flag"
	"github.com/superfly/flyctl/internal/flapsutil"
	"github.com/superfly/flyctl/internal/flyutil"
	"github.com/superfly/flyctl/internal/haikunator"
	"github.com/superfly/flyctl/internal/logger"
	"github.com/superfly/flyctl/internal/render"
	"github.com/superfly/flyctl/iostreams"
	"github.com/superfly/flyctl/logs"
	"golang.org/x/sync/errgroup"
)

type Deployer struct {
	app     *fly.App
	machine *fly.Machine
	flaps   flapsutil.FlapsClient
}

// Exec a remote "curl --unix-socket /path/to/socket -X GET http://localhost/ready" to check if the deployer is ready
// The deployer should return a 200 status code if it is ready
func (d *Deployer) Ready(ctx context.Context) (bool, error) {
	var io = iostreams.FromContext(ctx)

	fmt.Fprintln(io.Out, "Waiting for remote deployer to be ready")

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 500 * time.Millisecond
	b.MaxElapsedTime = 5 * time.Second

	cmd := "curl --unix-socket /var/run/fly/deployer.sock -X GET http://localhost/ready"

	err := backoff.Retry(func() error {
		res, err := d.flaps.Exec(ctx, d.machine.ID, &fly.MachineExecRequest{
			Cmd: cmd,
		})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("remote deployer not ready")
		}
		if !strings.Contains(res.StdOut, "OK") {
			return fmt.Errorf("remote deployer not ready: %s", res.StdOut)
		}
		return nil // Successful readiness check
	}, b)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (d *Deployer) Done(ctx context.Context) (<-chan struct{}, error) {
	done := make(chan struct{})

	go func() {
		defer close(done)

		operation := func() error {
			updateMachine, err := d.flaps.Get(ctx, d.machine.ID)
			if err != nil {
				if ctx.Err() == context.Canceled {
					return backoff.Permanent(err)
				}
				if ctx.Err() == context.DeadlineExceeded {
					fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Timeout reached waiting for machine %s to exit: %v\n", d.machine.ID, err)
					return backoff.Permanent(err)
				}
				fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Error getting machine %s from API: %v\n", d.machine.ID, err)
				return err
			}

			// Look for an exit event following a start event
			exitEvent := updateMachine.GetLatestEventOfTypeAfterType("start", "exit")
			if exitEvent != nil {
				return nil // Exit event found, operation is complete
			}

			return fmt.Errorf("exit event not found yet")
		}

		// Use an exponential backoff strategy
		b := backoff.NewExponentialBackOff()
		b.InitialInterval = 500 * time.Millisecond
		b.MaxInterval = 2 * time.Second
		b.MaxElapsedTime = 2 * time.Minute

		// Run the operation with backoff
		err := backoff.Retry(operation, backoff.WithContext(b, ctx))
		if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
			fmt.Fprintf(iostreams.FromContext(ctx).ErrOut, "Exiting with error: %v\n", err)
		}
	}()

	return done, nil
}

func deployRemotely(ctx context.Context, manifest *DeployManifest) error {
	var (
		client = flyutil.ClientFromContext(ctx)
		io     = iostreams.FromContext(ctx)
	)

	org, err := client.GetOrganizationByApp(ctx, manifest.AppName)
	if err != nil {
		return err
	}

	region := os.Getenv("FLY_REMOTE_BUILDER_REGION")

	deployer, err := EnsureDeployer(ctx, org, manifest.AppName, region)
	if err != nil {
		return err
	}

	// convert manifest to base64 so that we can pipe it to `fly deploy --manifest -`
	manifestBase64, err := manifest.ToBase64()
	if err != nil {
		return err
	}

	// the command should convert the encoded manifest back to json and pipe it to `fly deploy --manifest -`
	cmd := fmt.Sprintf(`bash -c "echo %s | curl -s --unix-socket /var/run/fly/deployer.sock -X POST --data-binary @- http://localhost/deploy"`, manifestBase64)
	fmt.Fprintln(io.Out, "Executing deploy command on remote deployer")

	res, err := deployer.flaps.Exec(ctx, deployer.machine.ID, &fly.MachineExecRequest{
		Cmd: cmd,
	})
	if err != nil {
		return err
	}

	if res.ExitCode != 0 {
		if res.StdErr != "" {
			fmt.Fprint(io.ErrOut, res.StdErr)
		}
		return fmt.Errorf("remote deploy failed with exit code %d", res.ExitCode)
	}
	if res.StdOut != "" {
		fmt.Fprint(io.Out, res.StdOut)
	}
	if res.StdErr != "" {
		fmt.Fprint(io.ErrOut, res.StdErr)
	}

	if flag.GetBool(ctx, "watch") {
		opts := &logs.LogOptions{
			AppName: deployer.app.Name,
			VMID:    deployer.machine.ID,
		}
		var eg *errgroup.Group
		eg, ctx = errgroup.WithContext(ctx)

		var streams []<-chan logs.LogEntry
		if opts.NoTail {
			streams = []<-chan logs.LogEntry{
				poll(ctx, eg, client, opts),
			}
		} else {
			pollingCtx, cancelPolling := context.WithCancel(ctx)
			streams = []<-chan logs.LogEntry{
				poll(pollingCtx, eg, client, opts),
				nats(ctx, eg, client, opts, cancelPolling),
			}
		}

		// Handle log streaming in another goroutine
		eg.Go(func() error {
			return printStreams(ctx, streams...)
		})

		eg.Go(func() error {
			done, err := deployer.Done(ctx)
			if err != nil {
				return err
			}

			// Wait for either the machine to exit or the context to be canceled
			select {
			case <-done:
				// Machine has exited, proceed with cleanup
				fmt.Fprintln(iostreams.FromContext(ctx).Out, "Machine has exited, stopping log streaming.")
			case <-ctx.Done():
				// Context was canceled (e.g., by Ctrl-C), proceed with cleanup
				fmt.Fprintln(iostreams.FromContext(ctx).Out, "Context canceled, stopping log streaming.")
			}
			return nil
		})

		// Wait for all goroutines to finish
		if waitErr := eg.Wait(); waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			return waitErr
		}
		return nil
	}

	return nil
}

func EnsureDeployer(ctx context.Context, org *fly.Organization, appName, region string) (*Deployer, error) {
	var deployer *Deployer

	deployer, err := findExistingDeployer(ctx, org, appName, region)

	switch {
	case err == nil:
		return deployer, nil
	case !strings.Contains(err.Error(), "no deployer found"):
		return nil, err
	default:
		// continue to create a new deployer
	}

	deployer, err = createDeployer(ctx, org, appName, region)
	if err != nil {
		return nil, err
	}

	switch ready, err := deployer.Ready(ctx); {
	case err != nil:
		return nil, err
	case !ready:
		return nil, fmt.Errorf("remote deployer not ready")
	}
	return deployer, nil
}

// findExistingDeployer finds an existing deployer app in the org by getting all apps and filtering by the app role and the app name with "fly-deployer-*"
func findExistingDeployer(ctx context.Context, org *fly.Organization, appName, region string) (*Deployer, error) {
	var client = flyutil.ClientFromContext(ctx)

	app, err := client.GetDeployerAppByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	flapsClient, err := flapsutil.NewClientWithOptions(ctx, flaps.NewClientOpts{
		AppName: app.Name,
		OrgSlug: org.Slug,
	})
	if err != nil {
		return nil, err
	}

	machines, err := flapsClient.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	if len(machines) > 0 {
		return nil, fmt.Errorf("a deployment is already in progress")
	}

	machine, err := createDeployerMachine(ctx, flapsClient, org.Slug, appName, region, org.PaidPlan)
	if err != nil {
		return nil, err
	}
	return &Deployer{
		app:     app,
		machine: machine,
		flaps:   flapsClient,
	}, nil
}

func createDeployer(ctx context.Context, org *fly.Organization, appName, region string) (*Deployer, error) {
	var (
		io     = iostreams.FromContext(ctx)
		client = flyutil.ClientFromContext(ctx)
	)

	var (
		appRole      = "fly-deployer"
		deployerName = appRole + haikunator.Haikunator().Build()
	)

	flapsClient, err := flapsutil.NewClientWithOptions(ctx, flaps.NewClientOpts{
		AppName: deployerName,
		OrgSlug: org.Slug,
	})
	if err != nil {
		return nil, err
	}
	ctx = flapsutil.NewContextWithClient(ctx, flapsClient)

	deployerApp, err := client.CreateApp(ctx, fly.CreateAppInput{
		OrganizationID:  org.ID,
		Name:            deployerName,
		AppRoleID:       appRole,
		Machines:        true,
		PreferredRegion: fly.StringPointer(region),
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = client.DeleteApp(ctx, deployerName)
		}
	}()

	if err := flapsClient.WaitForApp(ctx, deployerApp.Name); err != nil {
		return nil, err
	}

	app, err := client.GetAppCompact(ctx, appName)
	if err != nil {
		return nil, err
	}

	token, err := getDeployToken(ctx, appName, org.ID, &gql.LimitedAccessTokenOptions{
		"app_id": app.ID,
	})
	if err != nil {
		return nil, err
	}

	secrets := map[string]string{
		"FLY_API_TOKEN": token,
	}
	fmt.Fprintln(io.Out, "Setting deploy token")

	if _, err := client.SetSecrets(ctx, deployerApp.Name, secrets); err != nil {
		return nil, err
	}

	machine, err := createDeployerMachine(ctx, flapsClient, org.Slug, appName, region, org.PaidPlan)
	if err != nil {
		return nil, err
	}
	return &Deployer{
		app:     deployerApp,
		machine: machine,
		flaps:   flapsClient,
	}, nil
}

func createDeployerMachine(ctx context.Context, flapsClient flapsutil.FlapsClient, orgSlug, appName, region string, paidPlan bool) (*fly.Machine, error) {
	guest := fly.MachineGuest{
		CPUKind:  "shared",
		CPUs:     4,
		MemoryMB: 4096,
	}
	if paidPlan {
		guest.CPUKind = "shared"
		guest.CPUs = 8
		guest.MemoryMB = 8192
	}

	envVars := map[string]string{
		"ALLOW_ORG_SLUG": orgSlug,
		"FLY_DEPLOY_APP": appName,
	}

	var image = "docker.io/codebaker/fly-deployer:3d10c78"

	if os.Getenv("FLY_DEPLOYER_IMAGE") != "" {
		image = os.Getenv("FLY_DEPLOYER_IMAGE")
	}

	fmt.Fprintf(iostreams.FromContext(ctx).Out, "Using deployer image: %s\n", image)

	machineInput := fly.LaunchMachineInput{
		Region: region,
		Config: &fly.MachineConfig{
			Env:   envVars,
			Guest: &guest,
			Image: image,
			Restart: &fly.MachineRestart{
				Policy:     "on-failure",
				MaxRetries: 3,
			},
			AutoDestroy: true, // we want the machine to be destroyed after a successful deploy
		},
	}

	machine, err := flapsClient.Launch(ctx, machineInput)
	if err != nil {
		return nil, err
	}

	var state = "started"

	if err := flapsClient.Wait(ctx, machine, state, 60*time.Second); err != nil {
		return nil, err
	}
	return machine, nil
}

func getDeployToken(ctx context.Context, appName, orgID string, options *gql.LimitedAccessTokenOptions) (string, error) {
	var (
		profile = "deploy"
		expiry  = time.Minute * 300
		client  = flyutil.ClientFromContext(ctx)
	)

	resp, err := gql.CreateLimitedAccessToken(
		ctx,
		client.GenqClient(),
		appName,
		orgID,
		profile,
		options,
		expiry.String(),
	)
	if err != nil {
		return "", fmt.Errorf("failed creating token: %w", err)
	}
	return resp.CreateLimitedAccessToken.LimitedAccessToken.TokenHeader, nil
}

func printStreams(ctx context.Context, streams ...<-chan logs.LogEntry) error {
	var eg *errgroup.Group
	eg, ctx = errgroup.WithContext(ctx)

	out := iostreams.FromContext(ctx).Out
	json := config.FromContext(ctx).JSONOutput

	for _, stream := range streams {
		stream := stream

		eg.Go(func() error {
			return printStream(ctx, out, stream, json)
		})
	}
	return eg.Wait()
}

func printStream(ctx context.Context, w io.Writer, stream <-chan logs.LogEntry, json bool) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case entry, ok := <-stream:
			if !ok {
				return nil
			}

			var err error
			if json {
				err = render.JSON(w, entry)
			} else {
				err = render.LogEntry(w, entry,
					render.HideAllocID(),
					render.RemoveNewlines(),
					render.HideRegion(),
				)
			}

			if err != nil {
				return err
			}
		}
	}
}

func nats(ctx context.Context, eg *errgroup.Group, client flyutil.Client, opts *logs.LogOptions, cancelPolling context.CancelFunc) <-chan logs.LogEntry {
	c := make(chan logs.LogEntry)

	eg.Go(func() error {
		defer close(c)

		stream, err := logs.NewNatsStream(ctx, client, opts)
		if err != nil {
			logger := logger.FromContext(ctx)

			logger.Debugf("could not connect to wireguard tunnel: %v\n", err)
			logger.Debug("falling back to log polling...")

			return nil
		}

		// we wait for 2 seconds before canceling the polling context so that
		// we get a few records
		pause.For(ctx, 2*time.Second)
		cancelPolling()

		for entry := range stream.Stream(ctx, opts) {
			c <- entry
		}

		return nil
	})

	return c
}

func poll(ctx context.Context, eg *errgroup.Group, client flyutil.Client, opts *logs.LogOptions) <-chan logs.LogEntry {
	c := make(chan logs.LogEntry)

	eg.Go(func() (err error) {
		defer close(c)

		if err = logs.Poll(ctx, c, client, opts); errors.Is(err, context.Canceled) {
			// if the parent context is cancelled then the errorgroup will return
			// context.Canceled because nats and/or printStreams will return it.
			err = nil
		}
		return
	})

	return c
}
