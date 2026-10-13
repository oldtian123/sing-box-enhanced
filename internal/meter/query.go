// Package meter reads local sing-box V2Ray API inbound counters.
package meter

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

// sing-box overrides the service name declared in its stats.proto at startup.
const queryMethod = "/v2ray.core.app.stats.command.StatsService/QueryStats"

// Query returns cumulative inbound counters without resetting the core.
// The caller must persist its own watermarks and identify core restarts.
func Query(ctx context.Context, address string) (map[string]int64, error) {
	if err := validateAddress(address); err != nil {
		return nil, err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	connection, err := grpc.NewClient("passthrough:///"+address,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("create statistics client: %w", err)
	}
	defer connection.Close()
	// An empty QueryStatsRequest selects all counters with reset=false.
	var request, response wireMessage
	if err := connection.Invoke(ctx, queryMethod, &request, &response,
		grpc.ForceCodec(wireCodec{}), grpc.WaitForReady(true)); err != nil {
		return nil, fmt.Errorf("query statistics: %w", err)
	}
	return parseResponse(response)
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return fmt.Errorf("statistics address must be 127.0.0.1:port or [::1]:port")
	}
	for _, character := range port {
		if character < '0' || character > '9' {
			return fmt.Errorf("statistics port must be between 1 and 65535")
		}
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return fmt.Errorf("statistics port must be between 1 and 65535")
	}
	return nil
}

// wireCodec uses the upstream protobuf wire format without importing its core.
type wireMessage []byte
type wireCodec struct{}

func (wireCodec) Name() string { return "proto" }

func (wireCodec) Marshal(value any) ([]byte, error) {
	message, ok := value.(*wireMessage)
	if !ok {
		return nil, fmt.Errorf("unexpected statistics message type %T", value)
	}
	return *message, nil
}

func (wireCodec) Unmarshal(data []byte, value any) error {
	message, ok := value.(*wireMessage)
	if !ok {
		return fmt.Errorf("unexpected statistics message type %T", value)
	}
	*message = append((*message)[:0], data...)
	return nil
}

func parseResponse(data []byte) (map[string]int64, error) {
	counters := make(map[string]int64)
	seen := make(map[string]bool)
	for len(data) > 0 {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 || !number.IsValid() || kind == protowire.EndGroupType {
			return nil, fmt.Errorf("malformed statistics response tag")
		}
		data = data[size:]
		if number != 1 {
			size = protowire.ConsumeFieldValue(number, kind, data)
			if size < 0 {
				return nil, fmt.Errorf("malformed statistics response field")
			}
			data = data[size:]
			continue
		}
		if kind != protowire.BytesType {
			return nil, fmt.Errorf("malformed statistics entry type")
		}
		entry, size := protowire.ConsumeBytes(data)
		if size < 0 {
			return nil, fmt.Errorf("malformed statistics entry")
		}
		data = data[size:]
		name, value, err := parseStat(entry)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate statistics counter %q", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, "inbound>>>") {
			continue
		}
		parts := strings.Split(name, ">>>")
		if len(parts) != 4 || parts[1] == "" || parts[2] != "traffic" ||
			(parts[3] != "uplink" && parts[3] != "downlink") {
			return nil, fmt.Errorf("malformed inbound counter %q", name)
		}
		counters[name] = value
	}
	return counters, nil
}

func parseStat(data []byte) (string, int64, error) {
	var name string
	var value int64
	var hasName, hasValue bool
	for len(data) > 0 {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 || !number.IsValid() || kind == protowire.EndGroupType {
			return "", 0, fmt.Errorf("malformed counter tag")
		}
		data = data[size:]
		switch number {
		case 1:
			if kind != protowire.BytesType || hasName {
				return "", 0, fmt.Errorf("malformed counter name field")
			}
			var bytes []byte
			bytes, size = protowire.ConsumeBytes(data)
			if size < 0 || !utf8.Valid(bytes) {
				return "", 0, fmt.Errorf("malformed counter name")
			}
			name, hasName = string(bytes), true
		case 2:
			if kind != protowire.VarintType || hasValue {
				return "", 0, fmt.Errorf("malformed counter value field")
			}
			var unsigned uint64
			unsigned, size = protowire.ConsumeVarint(data)
			if size < 0 || int64(unsigned) < 0 {
				return "", 0, fmt.Errorf("malformed or negative counter value")
			}
			value, hasValue = int64(unsigned), true
		default:
			size = protowire.ConsumeFieldValue(number, kind, data)
			if size < 0 {
				return "", 0, fmt.Errorf("malformed counter field")
			}
		}
		data = data[size:]
	}
	if !hasName || name == "" {
		return "", 0, fmt.Errorf("statistics counter has no name")
	}
	return name, value, nil
}
