package instances

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

const (
	// hypervisorStateCacheTTL bounds how long a cached hypervisor state may be
	// reused before a fresh GetVMInfo call is required.
	hypervisorStateCacheTTL = 5 * time.Second
	// getVMInfoTimeout is the deadline passed to a hypervisor /vm.info query.
	// Backends with context-aware transports stop at the deadline; synchronous
	// backends may not.
	getVMInfoTimeout = 500 * time.Millisecond
)

// hypervisorStateCacheEntry stores the last observed hypervisor VM state for
// an instance, used to skip /vm.info queries on hot list paths.
type hypervisorStateCacheEntry struct {
	state       hypervisor.VMState
	refreshedAt time.Time
}

type instanceStateQueryLock struct {
	sync.RWMutex
	cacheMu    sync.Mutex
	generation uint64
}

func (m *manager) queryHypervisorState(ctx context.Context, stored *StoredMetadata) (hypervisorStateQueryResult, error) {
	if err := ctx.Err(); err != nil {
		return hypervisorStateQueryResult{}, err
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, getVMInfoTimeout)
	defer cancelWait()

	resultCh := m.hypervisorStateQueries.DoChan(stored.Id, func() (any, error) {
		lock := m.hypervisorStateQueryLock(stored.Id)
		if !lock.TryRLock() {
			return hypervisorStateQueryResult{operationInProgress: true}, nil
		}
		defer lock.RUnlock()

		if cached, ok := m.loadCachedHypervisorState(stored.Id); ok {
			return hypervisorStateQueryResult{state: cached, cacheHit: true}, nil
		}

		lock.cacheMu.Lock()
		generation := lock.generation
		lock.cacheMu.Unlock()

		log := logger.FromContext(ctx)
		queryCtx, cancelQuery := context.WithTimeout(context.WithoutCancel(ctx), getVMInfoTimeout)
		defer cancelQuery()

		hv, err := m.getHypervisor(stored.SocketPath, stored.HypervisorType)
		if err != nil {
			log.WarnContext(ctx, "failed to determine instance state",
				"instance_id", stored.Id,
				"socket", stored.SocketPath,
				"error", err,
			)
			return nil, fmt.Errorf("failed to create hypervisor client: %w", err)
		}

		info, err := hv.GetVMInfo(queryCtx)
		if err != nil {
			log.WarnContext(ctx, "failed to query hypervisor state",
				"instance_id", stored.Id,
				"socket", stored.SocketPath,
				"error", err,
			)
			return nil, fmt.Errorf("failed to query hypervisor: %w", err)
		}
		if !m.storeQueriedHypervisorState(lock, stored.Id, info.State, generation) {
			return nil, fmt.Errorf("hypervisor state changed during query")
		}
		return hypervisorStateQueryResult{state: info.State}, nil
	})

	select {
	case <-waitCtx.Done():
		return hypervisorStateQueryResult{}, waitCtx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return hypervisorStateQueryResult{}, result.Err
		}
		return result.Val.(hypervisorStateQueryResult), nil
	}
}

type hypervisorStateQueryResult struct {
	state               hypervisor.VMState
	cacheHit            bool
	operationInProgress bool
}

// hypervisorStateQueryLock returns the lock shared by state probes and standby snapshots.
func (m *manager) hypervisorStateQueryLock(id string) *instanceStateQueryLock {
	lock, _ := m.hypervisorStateQueryLocks.LoadOrStore(id, &instanceStateQueryLock{})
	return lock.(*instanceStateQueryLock)
}

func (m *manager) tryLockHypervisorStateForStandby(id string) *instanceStateQueryLock {
	lock := m.hypervisorStateQueryLock(id)
	if !lock.TryLock() {
		return nil
	}
	return lock
}

// loadCachedHypervisorState returns the cached hypervisor state for an instance
// when present and within the TTL window. Stale entries are treated as a miss.
func (m *manager) loadCachedHypervisorState(id string) (hypervisor.VMState, bool) {
	v, ok := m.hypervisorStateCache.Load(id)
	if !ok {
		return "", false
	}
	entry, ok := v.(hypervisorStateCacheEntry)
	if !ok {
		return "", false
	}
	if m.nowUTC().Sub(entry.refreshedAt) > hypervisorStateCacheTTL {
		return "", false
	}
	return entry.state, true
}

// storeCachedHypervisorState records the latest known hypervisor state for an
// instance so subsequent derive_state calls within the TTL window avoid the
// socket round-trip.
func (m *manager) storeCachedHypervisorState(id string, state hypervisor.VMState) {
	lock := m.hypervisorStateQueryLock(id)
	lock.cacheMu.Lock()
	lock.generation++
	m.hypervisorStateCache.Store(id, hypervisorStateCacheEntry{
		state:       state,
		refreshedAt: m.nowUTC(),
	})
	lock.cacheMu.Unlock()
}

func (m *manager) storeQueriedHypervisorState(lock *instanceStateQueryLock, id string, state hypervisor.VMState, generation uint64) bool {
	lock.cacheMu.Lock()
	defer lock.cacheMu.Unlock()
	if lock.generation != generation {
		return false
	}
	lock.generation++
	m.hypervisorStateCache.Store(id, hypervisorStateCacheEntry{
		state:       state,
		refreshedAt: m.nowUTC(),
	})
	return true
}

// invalidateCachedHypervisorState drops the cached hypervisor state for an
// instance. Called when the instance no longer has a live hypervisor socket
// (Stopped, Standby, Deleted) or transitions through a state where the cached
// value would be stale.
func (m *manager) invalidateCachedHypervisorState(id string) {
	lock := m.hypervisorStateQueryLock(id)
	lock.cacheMu.Lock()
	lock.generation++
	m.hypervisorStateCache.Delete(id)
	lock.cacheMu.Unlock()
}

// updateCachedHypervisorStateFromInstance reconciles the hypervisor state
// cache with the post-transition instance state observed by a lifecycle
// event. Lifecycle events are the source of truth for state changes hypeman
// itself drives; mirroring them into the cache avoids re-querying /vm.info
// on the next list call.
func (m *manager) updateCachedHypervisorStateFromInstance(inst *Instance) {
	if inst == nil {
		return
	}
	switch inst.State {
	case StateRunning, StateInitializing:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StateRunning)
	case StateCreated:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StateCreated)
	case StatePaused:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StatePaused)
	case StateShutdown:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StateShutdown)
	default:
		// Stopped, Standby, Unknown — no live socket worth caching.
		m.invalidateCachedHypervisorState(inst.Id)
	}
}
