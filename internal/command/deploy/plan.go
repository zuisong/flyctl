package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	fly "github.com/superfly/fly-go"
	"github.com/superfly/flyctl/internal/ctrlc"
	"github.com/superfly/flyctl/internal/flapsutil"
	mach "github.com/superfly/flyctl/internal/machine"
	"github.com/superfly/flyctl/internal/statuslogger"
	"github.com/superfly/flyctl/internal/tracing"
	"github.com/superfly/flyctl/iostreams"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

type AppState struct {
	Machines []*fly.Machine
}

type machinePairing struct {
	oldMachine *fly.Machine
	newMachine *fly.Machine
}

func (md *machineDeployment) appState(ctx context.Context) (*AppState, error) {
	machines, err := md.flapsClient.List(ctx, "")
	if err != nil {
		return nil, err
	}

	// TODO: could this be a list of machine id -> config?
	appState := &AppState{
		Machines: machines,
	}

	return appState, nil
}

type healthcheckResult struct {
	regularChecksPassed bool
	machineChecksPassed bool
}

var healthChecksPassed = sync.Map{}

func (md *machineDeployment) updateMachines(ctx context.Context, oldAppState, newAppState *AppState, pushForward bool, statusLogger statuslogger.StatusLogger) error {
	ctx, cancel := context.WithCancel(ctx)
	ctx, cancel = ctrlc.HookCancelableContext(ctx, cancel)
	defer cancel()

	oldMachines := make(map[string]*fly.Machine)
	for _, machine := range oldAppState.Machines {
		oldMachines[machine.ID] = machine
	}
	newMachines := make(map[string]*fly.Machine)
	for _, machine := range newAppState.Machines {
		newMachines[machine.ID] = machine
	}

	machineTuples := make([]machinePairing, 0)
	// TODO: a little tired rn, do we need to do this?
	for _, oldMachine := range oldMachines {
		// This means we want to update a machine
		if newMachine, ok := newMachines[oldMachine.ID]; ok {
			healthChecksPassed.LoadOrStore(oldMachine.ID, &healthcheckResult{})
			machineTuples = append(machineTuples, machinePairing{oldMachine: oldMachine, newMachine: newMachine})
		} else {
			// FIXME: this would currently delete unmanaged machines! no bueno
			// fmt.Println("Deleting machine", oldMachine.ID)
			// This means we should destroy the old machine
			// machineTuples = append(machineTuples, machinePairing{oldMachine: oldMachine, newMachine: nil})
		}
	}

	for _, newMachine := range newMachines {
		if _, ok := oldMachines[newMachine.ID]; !ok {
			// This means we should create the new machine
			healthChecksPassed.LoadOrStore(newMachine.ID, &healthcheckResult{})
			machineTuples = append(machineTuples, machinePairing{oldMachine: nil, newMachine: newMachine})
		}
	}

	var sl statuslogger.StatusLogger
	if statusLogger != nil {
		sl = statusLogger
	} else {
		sl = statuslogger.Create(ctx, len(machineTuples), true)
		defer sl.Destroy(false)
	}

	group := errgroup.Group{}
	for idx, machPair := range machineTuples {
		machPair := machPair
		oldMachine := machPair.oldMachine
		newMachine := machPair.newMachine

		idx := idx
		group.Go(func() error {
			checkResult, _ := healthChecksPassed.Load(machPair.oldMachine.ID)
			machineCheckResult := checkResult.(*healthcheckResult)
			err := md.updateMachineWChecks(ctx, oldMachine, newMachine, idx, sl, md.io, machineCheckResult)
			if err != nil {
				sl.Line(idx).LogStatus(statuslogger.StatusFailure, err.Error())
				return fmt.Errorf("failed to update machine %s: %w", oldMachine.ID, err)
			}
			return nil
		})
	}

	if updateErr := group.Wait(); updateErr != nil {
		if !pushForward {
			return updateErr
		}

		var unrecoverableErr unrecoverableError
		if strings.Contains(updateErr.Error(), "context canceled") || errors.As(updateErr, &unrecoverableErr) || strings.Contains(updateErr.Error(), "Unrecoverable error") {
			return updateErr
		}

		// if we fail to update the machines, we should revert the state back if possible
		for {
			currentState, err := md.appState(ctx)
			if err != nil {
				fmt.Println("Failed to get current state:", err)
				return err
			}
			err = md.updateMachines(ctx, currentState, newAppState, false, sl)
			if err == nil {
				break
			} else if strings.Contains(err.Error(), "context canceled") {
				return err
			} else {
				if errors.As(err, &unrecoverableErr) || strings.Contains(err.Error(), "Unrecoverable error") {
					return err
				}
				fmt.Println("Failed to update machines:", err, ". Retrying...")
			}
			time.Sleep(1 * time.Second)
		}

		return nil
	}

	return nil
}

type unrecoverableError struct {
	err error
}

func (e unrecoverableError) Error() string {
	return fmt.Sprintf("Unrecoverable error: %s", e.err)
}

func (e unrecoverableError) Unwrap() error {
	return e.err
}

func compareConfigs(oldConfig, newConfig *fly.MachineConfig) bool {
	opt := cmp.FilterPath(func(p cmp.Path) bool {
		vx := p.Last().String()

		if vx == `["fly_flyctl_version"]` {
			return true
		}
		return false
	}, cmp.Ignore())

	return cmp.Equal(oldConfig, newConfig, opt)
}

func (md *machineDeployment) updateMachineWChecks(ctx context.Context, oldMachine, newMachine *fly.Machine, idx int, sl statuslogger.StatusLogger, io *iostreams.IOStreams, healthcheckResult *healthcheckResult) error {
	var machine *fly.Machine = oldMachine
	var lease *fly.MachineLease

	defer func() {
		if machine == nil || lease == nil {
			return
		}

		// even if we fail to update the machine, we need to clear the lease
		// clear the existing lease
		ctx := context.WithoutCancel(ctx)
		err := clearMachineLease(ctx, machine.ID, lease.Data.Nonce)
		if err != nil {
			fmt.Println("Failed to clear lease for machine", machine.ID, "due to error", err)
			sl.Line(idx).LogStatus(statuslogger.StatusFailure, fmt.Sprintf("Failed to clear lease for machine %s", machine.ID))
		}
	}()

	// whether we need to create a new machine or update an existing one
	if oldMachine != nil {
		sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Acquiring lease for %s", oldMachine.ID))
		newLease, err := acquireMachineLease(ctx, oldMachine.ID)
		if err != nil {
			return err
		}
		lease = newLease

		if newMachine == nil {
			destroyMachine(ctx, oldMachine.ID, lease.Data.Nonce)
		} else {
			// if the config hasn't changed, we don't need to update the machine
			sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Updating machine config for %s", oldMachine.ID))
			newMachine, err := md.updateMachineConfig(ctx, oldMachine, newMachine.Config, sl.Line(idx))
			if err != nil {
				return err
			}
			machine = newMachine
		}
	} else if newMachine != nil {
		sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Creating machine for %s", newMachine.ID))
		var err error
		machine, err = createMachine(ctx, newMachine.Config, newMachine.Region)
		if err != nil {
			return err
		}

		sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Acquiring lease for %s", newMachine.ID))
		newLease, err := acquireMachineLease(ctx, machine.ID)
		if err != nil {
			return err
		}
		lease = newLease
	}

	var err error

	flapsClient := flapsutil.ClientFromContext(ctx)
	lm := mach.NewLeasableMachine(flapsClient, io, machine, false)

	sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Waiting for machine %s to reach a good state", oldMachine.ID))
	err = waitForMachineState(ctx, lm, []string{"stopped", "started", "suspended"}, 5*time.Minute, sl.Line(idx))
	if err != nil {
		return err
	}

	// start the machine
	sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Starting machine %s", oldMachine.ID))
	err = startMachine(ctx, machine.ID, lease.Data.Nonce)
	if err != nil {
		return err
	}

	// wait for the machine to reach the running state
	sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Waiting for machine %s to reach the 'started' state", machine.ID))
	err = waitForMachineState(ctx, lm, []string{"started"}, 5*time.Minute, sl.Line(idx))
	if err != nil {
		return err
	}

	if !healthcheckResult.machineChecksPassed {
		sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Running machine checks on machine %s", machine.ID))
		err = md.runTestMachines(ctx, machine, sl.Line(idx))
		if err != nil {
			return &unrecoverableError{err: err}
		}
		healthcheckResult.machineChecksPassed = true
	}

	if !healthcheckResult.regularChecksPassed {
		sl.Line(idx).LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Checking health of machine %s", machine.ID))
		err = lm.WaitForHealthchecksToPass(ctx, 5*time.Minute)
		if err != nil {
			return &unrecoverableError{err: err}
		}
		healthcheckResult.regularChecksPassed = true
	}

	sl.Line(idx).LogStatus(statuslogger.StatusSuccess, fmt.Sprintf("Machine %s is now in a good state", machine.ID))

	return nil
}

func destroyMachine(ctx context.Context, machineID string, lease string) error {
	flapsClient := flapsutil.ClientFromContext(ctx)
	err := flapsClient.Destroy(ctx, fly.RemoveMachineInput{
		ID:   machineID,
		Kill: true,
	}, lease)
	if err != nil {
		return err
	}

	return nil
}

func detectMultipleImageVersions(ctx context.Context) ([]*fly.Machine, error) {
	flapsClient := flapsutil.ClientFromContext(ctx)
	machines, err := flapsClient.List(ctx, "")
	if err != nil {
		return nil, err
	}

	// First, we get the latest image
	var latestImage string
	var latestUpdated time.Time

	for _, machine := range machines {
		updated, err := time.Parse(time.RFC3339, machine.UpdatedAt)
		if err != nil {
			return nil, err
		}

		if updated.After(latestUpdated) {
			latestUpdated = updated
			latestImage = machine.Config.Image
		}
	}

	var badMachines []*fly.Machine
	// Next, we find any machines that are not using the latest image
	for _, machine := range machines {
		if machine.Config.Image != latestImage {
			badMachines = append(badMachines, machine)
		}
	}

	return badMachines, nil
}

func clearMachineLease(ctx context.Context, machID, leaseNonce string) error {
	// TODO: remove this when valentin's work is done
	flapsClient := flapsutil.ClientFromContext(ctx)
	for {
		err := flapsClient.ReleaseLease(ctx, machID, leaseNonce)
		if err == nil {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
}

// returns when the machine is in one of the possible states, or after passing the timeout threshold
func waitForMachineState(ctx context.Context, lm mach.LeasableMachine, possibleStates []string, timeout time.Duration, sl statuslogger.StatusLine) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var mutex sync.Mutex

	var waitErr error
	numCompleted := 0
	successfulFinish := false

	for _, state := range possibleStates {
		state := state
		go func() {
			err := lm.WaitForState(ctx, state, timeout, false)
			sl.LogStatus(statuslogger.StatusRunning, fmt.Sprintf("Machine %s reached %s state", lm.Machine().ID, state))

			mutex.Lock()
			defer func() {
				numCompleted += 1
				mutex.Unlock()
			}()

			if err != nil {
				waitErr = err
			} else {
				successfulFinish = true
			}
		}()
	}

	// TODO(billy): i'm sure we can use channels here
	for {
		mutex.Lock()
		if successfulFinish || numCompleted == len(possibleStates) {
			mutex.Unlock()
			return waitErr
		}
		mutex.Unlock()

		time.Sleep(1 * time.Second)
	}
}

func acquireMachineLease(ctx context.Context, machID string) (*fly.MachineLease, error) {
	flapsClient := flapsutil.ClientFromContext(ctx)
	lease, err := flapsClient.AcquireLease(ctx, machID, fly.IntPointer(3600))
	if err != nil {
		// TODO: tell users how to manually clear the lease
		// TODO: have a flag to automatically clear the lease
		if strings.Contains(err.Error(), "failed to get lease") {
			return nil, unrecoverableError{err: err}
		} else {
			return nil, err
		}
	}

	return lease, nil
}

func (md *machineDeployment) updateMachineConfig(ctx context.Context, oldMachine *fly.Machine, newMachineConfig *fly.MachineConfig, sl statuslogger.StatusLine) (*fly.Machine, error) {
	if compareConfigs(oldMachine.Config, newMachineConfig) {
		return oldMachine, nil
	}

	lm := mach.NewLeasableMachine(md.flapsClient, md.io, oldMachine, false)
	md.updateMachine(ctx, &machineUpdateEntry{
		leasableMachine: lm,
		launchInput: &fly.LaunchMachineInput{
			Config: newMachineConfig,
			ID:     oldMachine.ID,
		},
	}, sl)
	return lm.Machine(), nil
}

func createMachine(ctx context.Context, machConfig *fly.MachineConfig, region string) (*fly.Machine, error) {
	flapsClient := flapsutil.ClientFromContext(ctx)
	machine, err := flapsClient.Launch(ctx, fly.LaunchMachineInput{
		Config: machConfig,
		Region: region,
	})
	if err != nil {
		return nil, err
	}

	return machine, nil
}

func (md *machineDeployment) updateMachine(ctx context.Context, e *machineUpdateEntry, sl statuslogger.StatusLine) error {
	ctx, span := tracing.GetTracer().Start(ctx, "update_machine", trace.WithAttributes(
		attribute.String("id", e.launchInput.ID),
		attribute.Bool("requires_replacement", e.launchInput.RequiresReplacement),
	))
	defer span.End()

	fmtID := e.leasableMachine.FormattedMachineId()

	replaceMachine := func() error {
		statuslogger.Logf(ctx, "Replacing %s by new machine", md.colorize.Bold(fmtID))
		if err := md.updateMachineByReplace(ctx, e); err != nil {
			return err
		}
		statuslogger.Logf(ctx, "Created machine %s", md.colorize.Bold(fmtID))
		return nil
	}

	if e.launchInput.RequiresReplacement {
		return replaceMachine()
	}

	sl.Logf("Updating %s", md.colorize.Bold(fmtID))
	if err := md.updateMachineInPlace(ctx, e); err != nil {
		switch {
		case len(e.leasableMachine.Machine().Config.Mounts) > 0:
			// Replacing a machine with a volume will cause the placement logic to pick wthe same host
			// dismissing the value of replacing it in case of lack of host capacity
			return err
		case strings.Contains(err.Error(), "could not reserve resource for machine"):
			return replaceMachine()
		default:
			return err
		}
	}
	return nil
}

func startMachine(ctx context.Context, machineID string, leaseNonce string) error {
	flapsClient := flapsutil.ClientFromContext(ctx)
	_, err := flapsClient.Start(ctx, machineID, leaseNonce)
	if err != nil {
		fmt.Println("Failed to start machine", machineID, "due to error", err)
		return err
	}

	return nil
}
