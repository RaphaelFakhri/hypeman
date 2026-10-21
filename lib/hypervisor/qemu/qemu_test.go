package qemu

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	goqemu "github.com/digitalocean/go-qemu/qemu"
	"github.com/digitalocean/go-qemu/qmp"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockingStatusMonitor struct {
	started chan struct{}
	closed  chan struct{}
	start   sync.Once
	close   sync.Once
}

func (m *blockingStatusMonitor) Connect() error { return nil }

func (m *blockingStatusMonitor) Disconnect() error {
	m.close.Do(func() { close(m.closed) })
	return nil
}

func (m *blockingStatusMonitor) Run([]byte) ([]byte, error) {
	m.start.Do(func() { close(m.started) })
	<-m.closed
	return nil, errors.New("monitor disconnected")
}

func (m *blockingStatusMonitor) Events(context.Context) (<-chan qmp.Event, error) {
	return nil, qmp.ErrEventsNotSupported
}

func TestNewClientContextBoundsQMPHandshake(t *testing.T) {
	listener, err := net.Listen("unix", t.TempDir()+"/qemu.sock")
	require.NoError(t, err)
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			close(accepted)
			_, _ = conn.Read(make([]byte, 1))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = NewClientContext(ctx, listener.Addr().String())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	<-accepted
}

func TestGetVMInfoClosesStalledQMPConnectionOnContextDeadline(t *testing.T) {
	monitor := &blockingStatusMonitor{started: make(chan struct{}), closed: make(chan struct{})}
	domain, err := goqemu.NewDomain(monitor, "vm")
	require.NoError(t, err)

	socketPath := t.TempDir() + "/qemu.sock"
	hv := &QEMU{client: &Client{domain: domain}, socketPath: socketPath}
	clientPool.Lock()
	clientPool.clients[socketPath] = hv
	clientPool.Unlock()
	t.Cleanup(func() { Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = hv.GetVMInfo(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	<-monitor.started
	<-monitor.closed
}

func TestGetTargetGuestMemoryBytesUsesSavedConfigOnColdStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, saveVMConfig(dir, savedVMConfig{VMConfig: hypervisor.VMConfig{MemoryBytes: 768}}))

	hv := &QEMU{socketPath: dir + "/qemu.sock"}
	target, err := hv.GetTargetGuestMemoryBytes(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(768), target)
}
