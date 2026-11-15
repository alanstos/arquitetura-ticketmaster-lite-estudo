// Package telemetry liga o OpenTelemetry (traces, métricas e logs) e exporta
// tudo via OTLP/HTTP para um coletor — no docker compose, o container
// grafana/otel-lgtm (Collector + Tempo + Prometheus + Loki + Grafana). Ver ADR-011.
//
// Sem OTEL_EXPORTER_OTLP_ENDPOINT a telemetria fica desligada: os providers
// globais continuam os no-op do OTel e a instrumentação vira custo ~zero.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Telemetry guarda o estado do OTel para o main: se está ligado e como desligar.
type Telemetry struct {
	Enabled  bool
	shutdown []func(context.Context) error
}

// Setup configura os providers globais (otel.SetTracerProvider etc.). Os
// exporters leem as variáveis padrão OTEL_EXPORTER_OTLP_* sozinhos.
func Setup(ctx context.Context, serviceName, version string) (*Telemetry, error) {
	t := &Telemetry{}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return t, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, err
	}

	// Traces: spans saem em lotes (batch) para não pagar uma ida à rede por span.
	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	// W3C traceparent: permite continuar um trace vindo de outro serviço/frontend.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	// Métricas: agregadas em memória e empurradas a cada 10 s (push, não scrape).
	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(10*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// Logs: registros do slog viram log records OTLP (Loki), já com trace_id/span_id.
	logExp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)), sdklog.WithResource(res))
	global.SetLoggerProvider(lp)

	t.Enabled = true
	// Ordem: logs por último, para registrar eventuais erros do shutdown dos outros.
	t.shutdown = []func(context.Context) error{tp.Shutdown, mp.Shutdown, lp.Shutdown}
	return t, nil
}

// Shutdown descarrega (flush) o que ainda está nos buffers e fecha os exporters.
// Sem isso, os últimos spans/métricas/logs antes de parar o processo se perdem.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, fn := range t.shutdown {
		errs = append(errs, fn(ctx))
	}
	return errors.Join(errs...)
}

// NewLogger devolve o logger da aplicação: JSON no stdout (sempre, com
// trace_id/span_id quando houver span no context) e, se a telemetria estiver
// ligada, também via OTLP para o Loki.
func (t *Telemetry) NewLogger(serviceName string) *slog.Logger {
	var h slog.Handler = traceContextHandler{slog.NewJSONHandler(os.Stdout, nil)}
	if t.Enabled {
		h = fanoutHandler{h, otelslog.NewHandler(serviceName)}
	}
	return slog.New(h)
}
