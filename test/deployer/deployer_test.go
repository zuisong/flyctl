//go:build integration
// +build integration

package deployer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/superfly/flyctl/test/testlib"
)

func TestDeployBasicNode(t *testing.T) {
	ctx := context.TODO()

	d, err := testlib.NewDeployerTestEnvFromEnv(ctx, t)
	require.NoError(t, err)

	defer d.Close()

	err = testlib.CopyFixtureIntoWorkDir(d.WorkDir(), "deploy-node")
	require.NoError(t, err)

	flyTomlPath := fmt.Sprintf("%s/fly.toml", d.WorkDir())

	appName := d.CreateRandomAppName()
	require.NotEmpty(t, appName)

	err = testlib.OverwriteConfig(flyTomlPath, map[string]any{
		"app":    appName,
		"region": d.PrimaryRegion(),
		"env": map[string]string{
			"TEST_ID": d.ID(),
		},
	})
	require.NoError(t, err)

	// app required
	d.Fly("apps create %s -o %s", appName, d.OrgSlug())

	deploy := d.NewRun(testlib.DeployOnly, testlib.DeployNow, testlib.WithAppSource(d.WorkDir()))

	defer deploy.Close()

	err = deploy.Start(ctx)

	require.Nil(t, err)

	_, err = deploy.Wait()
	require.Nil(t, err)

	require.Zero(t, deploy.ExitCode())

	body, err := testlib.RunHealthCheck(fmt.Sprintf("https://%s.fly.dev", appName))
	require.NoError(t, err)

	require.Contains(t, string(body), fmt.Sprintf("Hello, World! %s", d.ID()))
}

func TestLaunchBasicNode(t *testing.T) {
	ctx := context.TODO()

	d, err := testlib.NewDeployerTestEnvFromEnv(ctx, t)
	require.NoError(t, err)

	defer d.Close()

	err = testlib.CopyFixtureIntoWorkDir(d.WorkDir(), "deploy-node")
	require.NoError(t, err)

	flyTomlPath := fmt.Sprintf("%s/fly.toml", d.WorkDir())

	appName := d.CreateRandomAppName()
	require.NotEmpty(t, appName)

	err = testlib.OverwriteConfig(flyTomlPath, map[string]any{
		"app":    "dummy-app-name",
		"region": d.PrimaryRegion(),
		"env": map[string]string{
			"TEST_ID": d.ID(),
		},
	})
	require.NoError(t, err)

	// app required
	d.Fly("apps create %s -o %s", appName, d.OrgSlug())

	deploy := d.NewRun(testlib.WithApp(appName), testlib.WithCopyConfig, testlib.WithoutCustomize, testlib.WithouExtensions, testlib.DeployNow, testlib.WithAppSource(d.WorkDir()))

	defer deploy.Close()

	err = deploy.Start(ctx)

	require.Nil(t, err)

	_, err = deploy.Wait()
	require.Nil(t, err)

	require.Zero(t, deploy.ExitCode())

	body, err := testlib.RunHealthCheck(fmt.Sprintf("https://%s.fly.dev", appName))
	require.NoError(t, err)

	require.Contains(t, string(body), fmt.Sprintf("Hello, World! %s", d.ID()))
}

func TestLaunchGoFromRepo(t *testing.T) {
	ctx := context.TODO()

	d, err := testlib.NewDeployerTestEnvFromEnv(ctx, t)
	require.NoError(t, err)

	defer d.Close()

	appName := d.CreateRandomAppName()
	require.NotEmpty(t, appName)

	// app required
	d.Fly("apps create %s -o %s", appName, d.OrgSlug())

	deploy := d.NewRun(testlib.WithApp(appName), testlib.WithRegion("yyz"), testlib.WithoutCustomize, testlib.WithouExtensions, testlib.DeployNow, testlib.WithGitRepo("https://github.com/fly-apps/go-example"))

	defer deploy.Close()

	err = deploy.Start(ctx)

	require.Nil(t, err)

	_, err = deploy.Wait()
	require.Nil(t, err)

	require.Zero(t, deploy.ExitCode())

	body, err := testlib.RunHealthCheck(fmt.Sprintf("https://%s.fly.dev", appName))
	require.NoError(t, err)

	require.Contains(t, string(body), "I'm running in the yyz region")
}
