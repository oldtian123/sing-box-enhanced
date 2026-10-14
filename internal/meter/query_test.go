package meter

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

type fakeStats interface {
	query(context.Context, *wireMessage) (*wireMessage, error)
}

type fakeServer func(context.Context, *wireMessage) (*wireMessage, error)

func (server fakeServer) query(ctx context.Context, request *wireMessage) (*wireMessage, error) {
	return server(ctx, request)
}

func serveStats(t *testing.T, server fakeServer) string {
	t.Helper()
	return serveStatsDelayed(t, server, 0)
}

func serveStatsDelayed(t *testing.T, server fakeServer, delay time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := grpc.NewServer(grpc.ForceServerCodec(wireCodec{}))
	service.RegisterService(&grpc.ServiceDesc{
		ServiceName: "v2ray.core.app.stats.command.StatsService",
		HandlerType: (*fakeStats)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "QueryStats",
			Handler: func(server any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				var request wireMessage
				if err := decode(&request); err != nil {
					return nil, err
				}
				return server.(fakeStats).query(ctx, &request)
			},
		}},
	}, server)
	address := listener.Addr().String()
	if delay > 0 {
		_ = listener.Close()
	}
	stopped := make(chan struct{})
	go func() {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-stopped:
				return
			}
			var err error
			listener, err = net.Listen("tcp", address)
			if err != nil {
				t.Errorf("start delayed statistics server: %v", err)
				return
			}
		}
		_ = service.Serve(listener)
	}()
	t.Cleanup(func() {
		close(stopped)
		service.Stop()
	})
	return address
}

func counter(name string, value int64) []byte {
	entry := protowire.AppendTag(nil, 1, protowire.BytesType)
	entry = protowire.AppendString(entry, name)
	if value != 0 {
		entry = protowire.AppendTag(entry, 2, protowire.VarintType)
		entry = protowire.AppendVarint(entry, uint64(value))
	}
	return statEntry(entry)
}

func statEntry(entry []byte) []byte {
	response := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(response, entry)
}

func TestQueryWireProtocolAndInboundFiltering(t *testing.T) {
	response := counter("inbound>>>hy2>>>traffic>>>uplink", 700)
	response = append(response, counter("inbound>>>hy2>>>traffic>>>downlink", 0)...)
	response = append(response, counter("inbound>>>anytls>>>traffic>>>uplink", 900)...)
	response = append(response, counter("outbound>>>direct>>>traffic>>>uplink", 1600)...)
	response = append(response, counter("user>>>shared>>>traffic>>>uplink", 1600)...)
	response = protowire.AppendTag(response, 99, protowire.VarintType)
	response = protowire.AppendVarint(response, 1)
	address := serveStats(t, func(ctx context.Context, request *wireMessage) (*wireMessage, error) {
		if len(*request) != 0 {
			t.Error("request must omit reset and patterns")
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second {
			t.Error("request must have a default two-second deadline")
		}
		message := wireMessage(response)
		return &message, nil
	})
	got, err := Query(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"inbound>>>hy2>>>traffic>>>uplink":    700,
		"inbound>>>hy2>>>traffic>>>downlink":  0,
		"inbound>>>anytls>>>traffic>>>uplink": 900,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestQueryRejectsCorruptCounters(t *testing.T) {
	name := "inbound>>>hy2>>>traffic>>>uplink"
	duplicateName := protowire.AppendTag(nil, 1, protowire.BytesType)
	duplicateName = protowire.AppendString(duplicateName, name)
	duplicateName = protowire.AppendTag(duplicateName, 1, protowire.BytesType)
	duplicateName = protowire.AppendString(duplicateName, "another")
	duplicateValue := protowire.AppendTag(nil, 1, protowire.BytesType)
	duplicateValue = protowire.AppendString(duplicateValue, name)
	duplicateValue = append(duplicateValue, 0x10, 0x01, 0x10, 0x02)
	fixtures := map[string][]byte{
		"bad response tag":       {0},
		"truncated entry":        {0x0a, 0x05, 0x01},
		"wrong entry type":       {0x08, 0x01},
		"missing name":           statEntry([]byte{0x10, 0x01}),
		"invalid UTF-8 name":     statEntry([]byte{0x0a, 0x01, 0xff}),
		"truncated value":        statEntry([]byte{0x0a, 0x01, 'a', 0x10, 0x80}),
		"negative value":         counter(name, -1),
		"negative ignored value": counter("user>>>a>>>traffic>>>uplink", -1),
		"duplicate name field":   statEntry(duplicateName),
		"duplicate value field":  statEntry(duplicateValue),
		"duplicate counters":     append(counter(name, 4), counter(name, 5)...),
		"bad inbound counter":    counter("inbound>>>hy2>>>uplink", 4),
	}
	for name, response := range fixtures {
		t.Run(name, func(t *testing.T) {
			address := serveStats(t, func(context.Context, *wireMessage) (*wireMessage, error) {
				message := wireMessage(response)
				return &message, nil
			})
			got, err := Query(context.Background(), address)
			if err == nil || got != nil {
				t.Fatalf("corrupt response returned %v, %v", got, err)
			}
		})
	}
}

func TestQueryEmptyResponse(t *testing.T) {
	address := serveStats(t, func(context.Context, *wireMessage) (*wireMessage, error) {
		message := wireMessage(nil)
		return &message, nil
	})
	got, err := Query(context.Background(), address)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty statistics returned %v, %v", got, err)
	}
}

func TestQueryPropagatesRPCFailure(t *testing.T) {
	address := serveStats(t, func(context.Context, *wireMessage) (*wireMessage, error) {
		return nil, status.Error(codes.Unimplemented, "statistics disabled")
	})
	got, err := Query(context.Background(), address)
	if got != nil || status.Code(err) != codes.Unimplemented {
		t.Fatalf("RPC failure returned %v, %v", got, err)
	}
}

func TestQueryHonorsCallerDeadline(t *testing.T) {
	address := serveStats(t, func(ctx context.Context, request *wireMessage) (*wireMessage, error) {
		<-ctx.Done()
		return nil, status.Error(codes.DeadlineExceeded, "caller deadline")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := Query(ctx, address)
	if got != nil || status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("deadline returned %v, %v", got, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("caller deadline was replaced by the default")
	}
}

func TestQueryWaitsForCoreStartup(t *testing.T) {
	name := "inbound>>>startup>>>traffic>>>uplink"
	address := serveStatsDelayed(t, func(context.Context, *wireMessage) (*wireMessage, error) {
		message := wireMessage(counter(name, 150))
		return &message, nil
	}, 100*time.Millisecond)
	got, err := Query(context.Background(), address)
	if err != nil || got[name] != 150 {
		t.Fatalf("delayed core startup returned %v, %v", got, err)
	}
}

func TestQueryStartupWaitHonorsCallerDeadline(t *testing.T) {
	address := serveStatsDelayed(t, func(context.Context, *wireMessage) (*wireMessage, error) {
		message := wireMessage(nil)
		return &message, nil
	}, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := Query(ctx, address)
	if got != nil || status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("startup deadline returned %v, %v", got, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("startup wait replaced the caller deadline")
	}
}

func TestAddressRestrictions(t *testing.T) {
	for _, address := range []string{
		"localhost:8080", "0.0.0.0:8080", "127.0.0.2:8080", "192.0.2.1:8080",
		"[::]:8080", "[::ffff:127.0.0.1]:8080", "127.0.0.1:0", "127.0.0.1:65536",
		"127.0.0.1:http", "127.0.0.1:+80", "127.0.0.1:", "127.0.0.1",
	} {
		if _, err := Query(context.Background(), address); err == nil {
			t.Errorf("accepted address %q", address)
		}
	}
	for _, address := range []string{"127.0.0.1:1", "[::1]:65535"} {
		if err := validateAddress(address); err != nil {
			t.Errorf("rejected loopback address %q: %v", address, err)
		}
	}
}
