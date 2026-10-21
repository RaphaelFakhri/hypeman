package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/digitalocean/go-qemu/qmp"
)

type socketMonitor struct {
	conn        net.Conn
	connectDone chan struct{}
	disconnect  chan struct{}
	disconnect1 sync.Once
	responses   chan monitorResponse
	events      chan qmp.Event
	listenDone  chan struct{}
	listeners   atomic.Int32
	commandMu   sync.Mutex
	closeErr    error
}

type monitorResponse struct {
	data []byte
	err  error
}

func newSocketMonitor(ctx context.Context, socketPath string) (*socketMonitor, error) {
	conn, err := (&net.Dialer{Timeout: qmpConnectTimeout}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("create socket monitor: %w", err)
	}
	m := &socketMonitor{
		conn:        conn,
		connectDone: make(chan struct{}),
		disconnect:  make(chan struct{}),
	}
	connected := make(chan error, 1)
	go func() { connected <- m.Connect() }()
	select {
	case err := <-connected:
		if err != nil {
			_ = m.Disconnect()
			return nil, fmt.Errorf("connect to qmp: %w", err)
		}
	case <-ctx.Done():
		_ = m.Disconnect()
		<-connected
		return nil, fmt.Errorf("connect to qmp: %w", ctx.Err())
	}
	return m, nil
}

func (m *socketMonitor) Connect() error {
	defer close(m.connectDone)
	decoder := json.NewDecoder(m.conn)
	if err := decoder.Decode(new(any)); err != nil {
		return err
	}
	if err := json.NewEncoder(m.conn).Encode(qmp.Command{Execute: "qmp_capabilities"}); err != nil {
		return err
	}
	var response json.RawMessage
	if err := decoder.Decode(&response); err != nil {
		return err
	}
	if err := qmpResponseError(response); err != nil {
		return err
	}

	m.responses = make(chan monitorResponse, 1)
	m.events = make(chan qmp.Event)
	m.listenDone = make(chan struct{})
	go m.listen()
	return nil
}

func (m *socketMonitor) Disconnect() error {
	m.disconnect1.Do(func() {
		close(m.disconnect)
		m.closeErr = m.conn.Close()
	})
	<-m.connectDone
	if m.listenDone != nil {
		<-m.listenDone
	}
	return m.closeErr
}

func (m *socketMonitor) Run(command []byte) ([]byte, error) {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	responses := m.responses
	if responses == nil {
		return nil, errors.New("QMP monitor is not connected")
	}
	if _, err := m.conn.Write(command); err != nil {
		return nil, err
	}
	select {
	case response, ok := <-responses:
		return response.result(ok)
	case <-m.disconnect:
		return nil, net.ErrClosed
	}
}

func (r monitorResponse) result(ok bool) ([]byte, error) {
	if !ok {
		return nil, io.EOF
	}
	if r.err != nil {
		return nil, r.err
	}
	if err := qmpResponseError(r.data); err != nil {
		return nil, err
	}
	return r.data, nil
}

func (m *socketMonitor) Events(context.Context) (<-chan qmp.Event, error) {
	events := m.events
	if events == nil {
		return nil, errors.New("QMP monitor is not connected")
	}
	m.listeners.Add(1)
	return events, nil
}

func qmpResponseError(data []byte) error {
	var response struct {
		Error *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return err
	}
	if response.Error != nil && response.Error.Desc != "" {
		return errors.New(response.Error.Desc)
	}
	return nil
}

func (m *socketMonitor) listen() {
	defer close(m.listenDone)
	defer close(m.events)
	defer close(m.responses)
	scanner := bufio.NewScanner(m.conn)
	for scanner.Scan() {
		if !m.dispatchMessage(scanner.Bytes()) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		m.sendResponse(monitorResponse{err: err})
	}
}

func (m *socketMonitor) dispatchMessage(data []byte) bool {
	var event qmp.Event
	if json.Unmarshal(data, &event) != nil {
		return true
	}
	if event.Event != "" {
		if m.listeners.Load() == 0 {
			return true
		}
		return m.sendEvent(event)
	}
	return m.sendResponse(monitorResponse{data: append([]byte(nil), data...)})
}

func (m *socketMonitor) sendEvent(event qmp.Event) bool {
	select {
	case m.events <- event:
		return true
	case <-m.disconnect:
		return false
	}
}

func (m *socketMonitor) sendResponse(response monitorResponse) bool {
	select {
	case m.responses <- response:
		return true
	case <-m.disconnect:
		return false
	}
}

var _ qmp.Monitor = (*socketMonitor)(nil)
