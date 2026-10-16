//go:build darwin

package instances

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

// TestVZConcurrentForkMachineIdentifierRepro checks identity after both guests
// resume and execute commands. It does not assert that duplicate IDs cause crashes.
func TestVZConcurrentForkMachineIdentifierRepro(t *testing.T) {
	if os.Getenv("HYPEMAN_VZ_IDENTITY_REPRO") != "1" {
		t.Skip("set HYPEMAN_VZ_IDENTITY_REPRO=1 to reproduce concurrent VZ identity collisions")
	}
	if runtime.GOARCH != "arm64" || !isMacOS14OrLater(t) {
		t.Skip("VZ standby/restore requires Apple Silicon and macOS 14+")
	}
	ensureMkfsExt4Available(t)

	mgr, tmpDir := setupVZTestManager(t)
	p := paths.New(tmpDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	ref := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: ref})
	require.NoError(t, err)
	waitName := img.Name
	if img.Digest != "" {
		parsed, err := images.ParseNormalizedRef(img.Name)
		require.NoError(t, err)
		waitName = parsed.Repository() + "@" + img.Digest
	}
	require.NoError(t, imageManager.WaitForReady(ctx, waitName))
	require.NoError(t, system.NewManager(p).EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "vz-identity-source",
		Image:          ref,
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
		Cmd:            []string{"sleep", "infinity"},
	})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	sourceID := source.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, sourceID) })
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 60*time.Second))
	_, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, 30*time.Second)
	require.NoError(t, err)
	_, err = mgr.StandbyInstance(ctx, sourceID, StandbyInstanceRequest{})
	require.NoError(t, err)
	sourceMachineID := readVZReproMachineIdentifier(t, p.InstanceSnapshotLatest(sourceID))

	fork, err := mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{
		Name:        "vz-identity-fork",
		TargetState: StateStandby,
	})
	require.NoError(t, err)
	forkID := fork.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, forkID) })
	forkMachineID := readVZReproMachineIdentifier(t, p.InstanceSnapshotLatest(forkID))

	for _, id := range []string{sourceID, forkID} {
		_, err = mgr.RestoreInstance(ctx, id)
		require.NoError(t, err)
		require.NoError(t, waitForExecAgent(ctx, mgr, id, 60*time.Second))
	}
	for _, id := range []string{sourceID, forkID} {
		inst, err := waitForInstanceState(ctx, mgr, id, StateRunning, 30*time.Second)
		require.NoError(t, err)
		out, code, err := vzExecCommand(ctx, inst, "echo", "identity-repro-running")
		require.NoError(t, err)
		require.Equal(t, 0, code)
		require.Equal(t, "identity-repro-running", strings.TrimSpace(out))
	}

	t.Log("source and fork are both running and can execute guest commands")
	require.NotEqual(t, sourceMachineID, forkMachineID,
		"two running VZ guests were restored with the same platform identifier")
}

func readVZReproMachineIdentifier(t *testing.T, snapshotDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(snapshotDir, shimconfig.SnapshotManifestFile))
	require.NoError(t, err)
	var manifest shimconfig.SnapshotManifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.NotEmpty(t, manifest.ShimConfig.MachineIdentifierData)
	return manifest.ShimConfig.MachineIdentifierData
}
