package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

func awaitTestValue[T any](t *testing.T, ch <-chan T, timeoutMessage string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal(timeoutMessage)
		var zero T
		return zero
	}
}

func TestSocketMonitorHandshakeAndCommand(t *testing.T) {
	listener, err := net.Listen("unix", t.TempDir()+"/qemu.sock")
	require.NoError(t, err)
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			decoder := json.NewDecoder(conn)
			var command map[string]any
			for i, response := range []string{`{"QMP":{}}`, `{"return":{}}`, `{"return":{"status":"running"}}`} {
				if i > 0 {
					err = decoder.Decode(&command)
				}
				if err != nil {
					break
				}
				_, err = io.WriteString(conn, response+"\n")
				if err != nil {
					break
				}
			}
		}
		serverErr <- err
	}()

	monitor, err := newSocketMonitor(context.Background(), listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = monitor.Disconnect() })
	response, err := monitor.Run([]byte(`{"execute":"query-status"}` + "\n"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"return":{"status":"running"}}`, string(response))
	require.NoError(t, monitor.Disconnect())
	require.NoError(t, <-serverErr)
}

func TestNewClientContextBoundsQMPHandshake(t *testing.T) {
	listener, err := net.Listen("unix", t.TempDir()+"/qemu.sock")
	require.NoError(t, err)
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
			_, _ = conn.Read(make([]byte, 1))
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := NewClientContext(ctx, listener.Addr().String())
		result <- err
	}()

	conn := awaitTestValue(t, accepted, "QMP client did not connect")
	defer conn.Close()
	cancel()
	require.ErrorIs(t, awaitTestValue(t, result, "QMP handshake did not stop after cancellation"), context.Canceled)
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
	result := make(chan error, 1)
	go func() {
		_, err := hv.GetVMInfo(ctx)
		result <- err
	}()

	require.ErrorIs(t, awaitTestValue(t, result, "GetVMInfo did not return after its context deadline"), context.DeadlineExceeded)
	awaitTestValue(t, monitor.started, "QMP status command did not start")
	awaitTestValue(t, monitor.closed, "QMP connection was not closed after its context deadline")
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
