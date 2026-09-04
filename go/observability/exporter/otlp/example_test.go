package otlp_test

// Runnable documentation for this subpackage's one public contract: the
// side effect its init() function has on go/observability's own Init.
// Compiled AND executed by `go test`, matching go/observability's own
// example_test.go convention.

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	obs "github.com/vislake/speed/go/observability"
	_ "github.com/vislake/speed/go/observability/exporter/otlp"
)

// exampleMetricsCollector accepts every OTLP metrics export
// unconditionally -- this example only needs Init's shutdown to have
// somewhere real to flush to, not to inspect what was sent.
type exampleMetricsCollector struct {
	colmetricpb.UnimplementedMetricsServiceServer
}

func (exampleMetricsCollector) Export(context.Context, *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

// Example shows the whole of what blank-importing this package buys a
// host: obs.Init, given a non-empty WithOTLPEndpoint, succeeds instead of
// failing with the actionable "no OTLP exporter registered" error
// go/observability's own doc comment on Init describes for the case this
// package is not imported.
func Example() {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	grpcServer := grpc.NewServer()
	colmetricpb.RegisterMetricsServiceServer(grpcServer, exampleMetricsCollector{})
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	shutdown, err := obs.Init(context.Background(),
		obs.WithOTLPEndpoint(lis.Addr().String()),
		obs.WithOTLPInsecure(true),
	)
	fmt.Println("init:", err)
	fmt.Println("shutdown:", shutdown(context.Background()))

	// Output:
	// init: <nil>
	// shutdown: <nil>
}
