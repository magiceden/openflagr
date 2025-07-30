package config

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/caarlos0/env"
	"github.com/evalphobia/logrus_sentry"
	raven "github.com/getsentry/raven-go"
	newrelic "github.com/newrelic/go-agent"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
	// Explicitly removed:
	// "google.golang.org/grpc"
	// "google.golang.org/grpc/credentials/insecure"
)

// EvalOnlyModeDBDrivers is a list of DBDrivers that we should only run in EvalOnlyMode.
var EvalOnlyModeDBDrivers = map[string]struct{}{
	"json_file": {},
	"json_http": {},
}

// Global is the global dependency we can use, such as the new relic app instance
var Global = struct {
	NewrelicApp   newrelic.Application
	StatsdClient  *statsd.Client
	Prometheus    prometheusMetrics
	OpenTelemetry openTelemetryMetrics
}{}

func init() {
	env.Parse(&Config)

	setupEvalOnlyMode()
	setupSentry()
	setupLogrus()
	setupStatsd()
	setupNewrelic()
	setupPrometheus()
	setupOpenTelemetry()
}

func setupEvalOnlyMode() {
	if _, ok := EvalOnlyModeDBDrivers[Config.DBDriver]; ok {
		Config.EvalOnlyMode = true
	}
}

func setupLogrus() {
	l, err := logrus.ParseLevel(Config.LogrusLevel)
	if err != nil {
		logrus.WithField("err", err).Fatalf("failed to set logrus level:%s", Config.LogrusLevel)
	}
	logrus.SetLevel(l)
	logrus.SetOutput(os.Stdout)
	switch Config.LogrusFormat {
	case "text":
		logrus.SetFormatter(&logrus.TextFormatter{})
	case "json":
		logrus.SetFormatter(&logrus.JSONFormatter{})
	default:
		logrus.Warnf("unexpected logrus format: %s, should be one of: text, json", Config.LogrusFormat)
	}
}

func setupSentry() {
	if Config.SentryEnabled {
		raven.SetDSN(Config.SentryDSN)
		hook, err := logrus_sentry.NewSentryHook(Config.SentryDSN, []logrus.Level{
			logrus.PanicLevel,
			logrus.FatalLevel,
			logrus.ErrorLevel,
		})
		if Config.SentryEnvironment != "" {
			hook.SetEnvironment(Config.SentryEnvironment)
		}
		if err != nil {
			logrus.WithField("err", err).Error("failed to hook logurs to sentry")
			return
		}
		logrus.StandardLogger().Hooks.Add(hook)
	}
}

func setupStatsd() {
	if Config.StatsdEnabled {
		client, err := statsd.New(fmt.Sprintf("%s:%s", Config.StatsdHost, Config.StatsdPort))
		if err != nil {
			panic(fmt.Sprintf("unable to initialize statsd. %s", err))
		}
		client.Namespace = Config.StatsdPrefix

		Global.StatsdClient = client
	}
}

func setupNewrelic() {
	if Config.NewRelicEnabled {
		nCfg := newrelic.NewConfig(Config.NewRelicAppName, Config.NewRelicKey)
		nCfg.Enabled = true
		// These two cannot be enabled at the same time and cross application is enabled by default
		nCfg.DistributedTracer.Enabled = Config.NewRelicDistributedTracingEnabled
		nCfg.CrossApplicationTracer.Enabled = !Config.NewRelicDistributedTracingEnabled
		app, err := newrelic.NewApplication(nCfg)
		if err != nil {
			panic(fmt.Sprintf("unable to initialize newrelic. %s", err))
		}
		Global.NewrelicApp = app
	}
}

type prometheusMetrics struct {
	ScrapePath       string
	EvalCounter      *prometheus.CounterVec
	RequestCounter   *prometheus.CounterVec
	RequestHistogram *prometheus.HistogramVec
}

type openTelemetryMetrics struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	Tracer         trace.Tracer
	Meter          metric.Meter
	EvalCounter    metric.Int64Counter
	RequestCounter metric.Int64Counter
	RequestLatency metric.Float64Histogram
}

func setupPrometheus() {
	if Config.PrometheusEnabled {
		Global.Prometheus.ScrapePath = Config.PrometheusPath
		Global.Prometheus.EvalCounter = promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "flagr_eval_results",
			Help: "A counter of eval results",
		}, []string{"EntityType", "FlagID", "FlagKey", "VariantID", "VariantKey"})
		Global.Prometheus.RequestCounter = promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "flagr_requests_total",
			Help: "The total http requests received",
		}, []string{"status", "path", "method"})

		if Config.PrometheusIncludeLatencyHistogram {
			Global.Prometheus.RequestHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
				Name: "flagr_requests_buckets",
				Help: "A histogram of latencies for requests received",
			}, []string{"status", "path", "method"})
		}
	}
}

func setupOpenTelemetry() {
	if !Config.OpenTelemetryEnabled {
		return
	}

	// Create a resource with service information
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName(Config.OpenTelemetryServiceName),
		),
	)
	if err != nil {
		logrus.WithField("err", err).Error("failed to create OpenTelemetry resource")
		return
	}

	// Setup trace provider if traces are enabled
	if Config.OpenTelemetryTracesEnabled {
		var traceExporter sdktrace.SpanExporter
		var err error

		switch Config.OpenTelemetryExporterType {
		case "otlp":
			// Create OTLP gRPC exporter for traces
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			var opts []otlptracegrpc.Option
			opts = append(opts, otlptracegrpc.WithEndpoint(Config.OpenTelemetryExporterEndpoint))
			if Config.OpenTelemetryExporterInsecure {
				opts = append(opts, otlptracegrpc.WithInsecure())
			}

			traceExporter, err = otlptracegrpc.New(ctx, opts...)
		case "stdout":
			// Create stdout exporter for traces (useful for debugging)
			traceExporter, err = stdouttrace.New()
		case "none":
			// No exporter, just use the SDK
			traceExporter = nil
		default:
			logrus.Errorf("unsupported OpenTelemetry exporter type: %s", Config.OpenTelemetryExporterType)
			return
		}

		if err != nil {
			logrus.WithField("err", err).Error("failed to create OpenTelemetry trace exporter")
			return
		}

		// Create trace provider
		tracerProvider := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		)

		if traceExporter != nil {
			tracerProvider.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(traceExporter))
		}

		// Set global trace provider
		otel.SetTracerProvider(tracerProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))

		Global.OpenTelemetry.TracerProvider = tracerProvider
		Global.OpenTelemetry.Tracer = tracerProvider.Tracer("flagr")
	}

	// Setup meter provider if metrics are enabled
	if Config.OpenTelemetryMetricsEnabled {
		var metricExporter sdkmetric.Exporter
		var err error

		switch Config.OpenTelemetryExporterType {
		case "otlp":
			// Create OTLP gRPC exporter for metrics
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			var opts []otlpmetricgrpc.Option
			opts = append(opts, otlpmetricgrpc.WithEndpoint(Config.OpenTelemetryExporterEndpoint))
			if Config.OpenTelemetryExporterInsecure {
				opts = append(opts, otlpmetricgrpc.WithInsecure())
			}

			metricExporter, err = otlpmetricgrpc.New(ctx, opts...)
		case "stdout":
			// Create stdout exporter for metrics (useful for debugging)
			metricExporter, err = stdoutmetric.New()
		case "none":
			// No exporter, just use the SDK
			metricExporter = nil
		default:
			logrus.Errorf("unsupported OpenTelemetry exporter type: %s", Config.OpenTelemetryExporterType)
			return
		}

		if err != nil {
			logrus.WithField("err", err).Error("failed to create OpenTelemetry metric exporter")
			return
		}

		// Create meter provider
		meterProvider := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
		)

		if metricExporter != nil {
			reader := sdkmetric.NewPeriodicReader(metricExporter)
			meterProvider = sdkmetric.NewMeterProvider(
				sdkmetric.WithResource(res),
				sdkmetric.WithReader(reader),
			)
		}

		// Set global meter provider
		otel.SetMeterProvider(meterProvider)

		Global.OpenTelemetry.MeterProvider = meterProvider
		Global.OpenTelemetry.Meter = meterProvider.Meter("flagr")

		// Create metrics
		evalCounter, err := Global.OpenTelemetry.Meter.Int64Counter(
			"flagr_eval_results",
			metric.WithDescription("A counter of eval results"),
		)
		if err != nil {
			logrus.WithField("err", err).Error("failed to create OpenTelemetry eval counter")
		} else {
			Global.OpenTelemetry.EvalCounter = evalCounter
		}

		requestCounter, err := Global.OpenTelemetry.Meter.Int64Counter(
			"flagr_requests_total",
			metric.WithDescription("The total http requests received"),
		)
		if err != nil {
			logrus.WithField("err", err).Error("failed to create OpenTelemetry request counter")
		} else {
			Global.OpenTelemetry.RequestCounter = requestCounter
		}

		requestLatency, err := Global.OpenTelemetry.Meter.Float64Histogram(
			"flagr_requests_duration_seconds",
			metric.WithDescription("A histogram of latencies for requests received"),
		)
		if err != nil {
			logrus.WithField("err", err).Error("failed to create OpenTelemetry request latency histogram")
		} else {
			Global.OpenTelemetry.RequestLatency = requestLatency
		}
	}

	logrus.Info("OpenTelemetry setup completed")
}
