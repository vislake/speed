// Package otlp registers observability's OTLP/gRPC trace and metric
// exporters. Importing it purely for its init() side effect is what a host
// wanting OTLP export does:
//
//	import _ "github.com/vislake/speed/go/observability/exporter/otlp"
//
// after which observability.Init(ctx, observability.WithOTLPEndpoint(...))
// wires the real exporters this package builds. Without that import, Init
// fails with an actionable error naming it, rather than silently falling
// back to the local exporters.
//
// This package exists to isolate otlptracegrpc, otlpmetricgrpc, and
// everything they pull in (gRPC, protobuf, and their own transitive
// dependency trees) out of go/observability's root package: a consumer
// that only imports the root package never sees any of it in its own
// go.mod or go.sum, per go/observability/AGENTS.md's measured
// indirect-dependency count. It has no exported API beyond the init()
// side effect -- there is nothing here for a caller to invoke directly.
package otlp

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	observability "github.com/vislake/speed/go/observability"
)

func init() {
	observability.RegisterOTLPExporters(buildProviders)
}

// buildProviders wires the OTLP/gRPC exporter set: both signals are pushed
// to cfg.OTLPEndpoint. It is the function this package registers with
// observability.RegisterOTLPExporters -- see that function's own doc
// comment for the registration convention -- and is called by
// observability.Init exactly when a caller supplies a non-empty
// WithOTLPEndpoint and this package has been blank-imported.
func buildProviders(ctx context.Context, cfg observability.Config, res *resource.Resource) (func(context.Context) error, error) {
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.OTLPInsecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("observability/exporter/otlp: build OTLP trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)

	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, fmt.Errorf("observability/exporter/otlp: build OTLP metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	shutdown := func(shutdownCtx context.Context) error {
		return errors.Join(tp.Shutdown(shutdownCtx), mp.Shutdown(shutdownCtx))
	}
	return shutdown, nil
}
