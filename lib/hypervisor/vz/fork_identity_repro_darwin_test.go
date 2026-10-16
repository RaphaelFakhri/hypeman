//go:build darwin

package vz

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	applevz "github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/require"
)

// TestPrepareForkMachineIdentifierCollisionRepro intentionally fails when enabled.
// It diagnoses duplicated platform identity, not guest-visible consequences.
func TestPrepareForkMachineIdentifierCollisionRepro(t *testing.T) {
	if os.Getenv("HYPEMAN_VZ_IDENTITY_REPRO") != "1" {
		t.Skip("set HYPEMAN_VZ_IDENTITY_REPRO=1 to reproduce duplicate VZ machine identifiers")
	}

	machineID, err := applevz.NewGenericMachineIdentifier()
	require.NoError(t, err)
	original := shimconfig.SnapshotManifest{
		Hypervisor:       string(hypervisor.TypeVZ),
		MachineStateFile: shimconfig.SnapshotMachineStateFile,
		ShimConfig: shimconfig.ShimConfig{
			MachineIdentifierData: base64.StdEncoding.EncodeToString(machineID.DataRepresentation()),
		},
	}
	data, err := json.Marshal(original)
	require.NoError(t, err)

	manifestPath := filepath.Join(t.TempDir(), shimconfig.SnapshotManifestFile)
	require.NoError(t, os.WriteFile(manifestPath, data, 0600))
	_, err = NewStarter().PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: manifestPath,
		SourceDataDir:      "/guests/source",
		TargetDataDir:      "/guests/fork",
	})
	require.NoError(t, err)

	data, err = os.ReadFile(manifestPath)
	require.NoError(t, err)
	var fork shimconfig.SnapshotManifest
	require.NoError(t, json.Unmarshal(data, &fork))

	encoded, err := base64.StdEncoding.DecodeString(fork.ShimConfig.MachineIdentifierData)
	require.NoError(t, err)
	restoredID, err := applevz.NewGenericMachineIdentifierWithData(encoded)
	require.NoError(t, err)
	_, err = applevz.NewGenericPlatformConfiguration(applevz.WithGenericMachineIdentifier(restoredID))
	require.NoError(t, err)

	t.Log("fork manifest reconstructs a VZ platform; no VM was booted")
	require.NotEqual(t, machineID.DataRepresentation(), restoredID.DataRepresentation(),
		"source and fork reconstruct the same platform identifier; concurrent execution needs an explicit identity-collision policy")
}
