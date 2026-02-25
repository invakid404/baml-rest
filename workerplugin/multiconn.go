package workerplugin

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/invakid404/baml-rest/bamlutils"
	"google.golang.org/grpc"
)

// ExtraGRPCConns is the number of additional gRPC connections created per
// worker via the GRPCBroker. Together with the primary go-plugin connection,
// this gives (1 + ExtraGRPCConns) total connections per worker. Each
// connection gets its own HTTP/2 transport, preventing head-of-line blocking
// when many concurrent RPCs are multiplexed on a single connection — the
// single loopyWriter goroutine per HTTP/2 transport serializes all frame
// writes, causing a convoy effect at high concurrency.
const ExtraGRPCConns = 3

// multiConnWorker distributes RPCs across multiple gRPC connections to the
// same worker process via round-robin.
type multiConnWorker struct {
	workers []Worker
	conns   []*grpc.ClientConn // brokered connections to close on teardown
	counter atomic.Uint64
}

var _ Worker = (*multiConnWorker)(nil)
var _ io.Closer = (*multiConnWorker)(nil)

func (m *multiConnWorker) pick() Worker {
	n := m.counter.Add(1) - 1
	return m.workers[n%uint64(len(m.workers))]
}

func (m *multiConnWorker) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *StreamResult, error) {
	return m.pick().CallStream(ctx, methodName, inputJSON, streamMode)
}

func (m *multiConnWorker) Parse(ctx context.Context, methodName string, inputJSON []byte) (*ParseResult, error) {
	return m.pick().Parse(ctx, methodName, inputJSON)
}

// Management RPCs go to the primary (go-plugin) connection — they're
// lightweight and don't benefit from distribution.

func (m *multiConnWorker) Health(ctx context.Context) (bool, error) {
	return m.workers[0].Health(ctx)
}

func (m *multiConnWorker) GetMetrics(ctx context.Context) ([][]byte, error) {
	return m.workers[0].GetMetrics(ctx)
}

func (m *multiConnWorker) TriggerGC(ctx context.Context) (*GCResult, error) {
	return m.workers[0].TriggerGC(ctx)
}

func (m *multiConnWorker) GetGoroutines(ctx context.Context, filter string) (*GoroutinesResult, error) {
	return m.workers[0].GetGoroutines(ctx, filter)
}

// Close closes the brokered gRPC connections. The primary connection is
// managed by go-plugin and closed via client.Kill().
func (m *multiConnWorker) Close() error {
	for _, conn := range m.conns {
		conn.Close()
	}
	return nil
}
