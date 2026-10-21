package qemu

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestClientCreationLockHonorsContext(t *testing.T) {
	path := t.TempDir() + "/qemu.sock"
	release, _, _, err := acquireClientCreationLock(context.Background(), path)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, _, err = acquireClientCreationLock(ctx, path)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	release()
}

func TestResetClientInvalidatesInFlightCreation(t *testing.T) {
	path := t.TempDir() + "/qemu.sock"
	release, lock, generation, err := acquireClientCreationLock(context.Background(), path)
	require.NoError(t, err)
	defer release()

	resetClient(path)

	clientPool.RLock()
	gotGeneration := lock.generation
	clientPool.RUnlock()
	require.NotEqual(t, generation, gotGeneration)
}

func TestRemoveClientDoesNotEvictReplacement(t *testing.T) {
	socketPath := t.TempDir() + "/qemu.sock"
	stale := &QEMU{socketPath: socketPath, profile: StandardProfile{}}
	replacement := &QEMU{socketPath: socketPath, profile: MicroVMProfile{}}
	clientPool.Lock()
	clientPool.clients[socketPath] = replacement
	clientPool.Unlock()
	t.Cleanup(func() {
		clientPool.Lock()
		delete(clientPool.clients, socketPath)
		clientPool.Unlock()
	})

	removeClient(stale)

	clientPool.RLock()
	got := clientPool.clients[socketPath]
	clientPool.RUnlock()
	require.Same(t, replacement, got)
}

func TestGetOrCreateForTypeRejectsCachedBackendMismatch(t *testing.T) {
	socketPath := t.TempDir() + "/qemu.sock"
	clientPool.Lock()
	clientPool.clients[socketPath] = &QEMU{socketPath: socketPath, profile: StandardProfile{}}
	clientPool.Unlock()
	t.Cleanup(func() {
		clientPool.Lock()
		delete(clientPool.clients, socketPath)
		clientPool.Unlock()
	})

	_, err := GetOrCreateForType(socketPath, hypervisor.TypeQEMUMicroVM)
	require.ErrorContains(t, err, "pooled as hypervisor qemu, not qemu-microvm")

	clientPool.RLock()
	cached := clientPool.clients[socketPath]
	clientPool.RUnlock()
	require.Equal(t, hypervisor.TypeQEMU, cached.profile.hypervisorType(), "a stale caller must not evict the live client")
}
