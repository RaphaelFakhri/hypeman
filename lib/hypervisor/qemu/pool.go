package qemu

import (
	"context"
	"fmt"
	"sync"

	"github.com/kernel/hypeman/lib/hypervisor"
)

type clientCreationLock struct {
	token chan struct{}
	refs  int
}

// clientPool manages singleton QMP connections per socket path.
// QEMU's QMP socket only allows one connection at a time, so we must
// reuse existing connections rather than creating new ones.
var clientPool = struct {
	sync.RWMutex
	clients map[string]*QEMU
	locks   map[string]*clientCreationLock
}{
	clients: make(map[string]*QEMU),
	locks:   make(map[string]*clientCreationLock),
}

// GetOrCreate returns a standard QEMU client for the socket path,
// or creates a new one if none exists.
func GetOrCreate(socketPath string) (*QEMU, error) {
	return GetOrCreateForType(socketPath, hypervisor.TypeQEMU)
}

// GetOrCreateForType returns a QEMU client with the requested backend identity.
func GetOrCreateForType(socketPath string, hypervisorType hypervisor.Type) (*QEMU, error) {
	return GetOrCreateForTypeContext(context.Background(), socketPath, hypervisorType)
}

// GetOrCreateForTypeContext returns the pooled client or creates one using the
// caller's context for socket connection and QMP handshake.
func GetOrCreateForTypeContext(ctx context.Context, socketPath string, hypervisorType hypervisor.Type) (*QEMU, error) {
	requestedProfile, err := profileForType(hypervisorType)
	if err != nil {
		return nil, err
	}

	if client, err := pooledClient(socketPath, hypervisorType); err != nil || client != nil {
		return client, err
	}

	release, err := acquireClientCreationLock(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if client, err := pooledClient(socketPath, hypervisorType); err != nil || client != nil {
		return client, err
	}

	client, err := newClientContext(ctx, socketPath, requestedProfile)
	if err != nil {
		return nil, err
	}

	clientPool.Lock()
	clientPool.clients[socketPath] = client
	clientPool.Unlock()
	return client, nil
}

func pooledClient(socketPath string, hypervisorType hypervisor.Type) (*QEMU, error) {
	clientPool.RLock()
	client := clientPool.clients[socketPath]
	clientPool.RUnlock()
	if client == nil {
		return nil, nil
	}
	if client.profile.hypervisorType() != hypervisorType {
		return nil, poolTypeMismatchError(socketPath, client.profile.hypervisorType(), hypervisorType)
	}
	return client, nil
}

func acquireClientCreationLock(ctx context.Context, socketPath string) (func(), error) {
	clientPool.Lock()
	lock := clientPool.locks[socketPath]
	if lock == nil {
		lock = &clientCreationLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		clientPool.locks[socketPath] = lock
	}
	lock.refs++
	clientPool.Unlock()

	select {
	case <-ctx.Done():
		releaseClientCreationReference(socketPath, lock)
		return nil, ctx.Err()
	case <-lock.token:
		if err := ctx.Err(); err != nil {
			lock.token <- struct{}{}
			releaseClientCreationReference(socketPath, lock)
			return nil, err
		}
		return func() {
			lock.token <- struct{}{}
			releaseClientCreationReference(socketPath, lock)
		}, nil
	}
}

func releaseClientCreationReference(socketPath string, lock *clientCreationLock) {
	clientPool.Lock()
	lock.refs--
	if lock.refs == 0 && clientPool.locks[socketPath] == lock {
		delete(clientPool.locks, socketPath)
	}
	clientPool.Unlock()
}

func poolTypeMismatchError(socketPath string, cached, requested hypervisor.Type) error {
	return fmt.Errorf("QEMU client for %s is pooled as hypervisor %s, not %s", socketPath, cached, requested)
}

// resetClient drops a pooled connection before a new QEMU process reuses the
// same socket path. Disconnect happens outside the pool lock and stale users
// can only remove their own client generation.
func resetClient(socketPath string) {
	client := takeClient(socketPath, nil)
	closeClientAsync(client)
}

// removeClient removes client only if it is still the current generation for
// its socket path. This prevents a late error from an old QEMU process from
// evicting the replacement process's client.
func removeClient(client *QEMU) {
	if client == nil {
		return
	}
	removed := takeClient(client.socketPath, client)
	closeClientAsync(removed)
}

func removeClientAndClose(client *QEMU) {
	if client == nil {
		return
	}
	takeClient(client.socketPath, client)
	if client.client != nil {
		_ = client.client.Close()
	}
}

// Remove closes and removes the current client for socketPath.
func Remove(socketPath string) {
	client := takeClient(socketPath, nil)
	closeClientAsync(client)
}

// takeClient removes the current client. When expected is non-nil, removal is
// conditional on pointer identity to protect against socket-path reuse.
func takeClient(socketPath string, expected *QEMU) *QEMU {
	clientPool.Lock()
	defer clientPool.Unlock()

	client, ok := clientPool.clients[socketPath]
	if !ok || (expected != nil && client != expected) {
		return nil
	}
	delete(clientPool.clients, socketPath)
	return client
}

func closeClientAsync(client *QEMU) {
	if client != nil && client.client != nil {
		go client.client.Close()
	}
}
