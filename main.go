package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/go-plugins-helpers/sdk"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const pluginManifest = `{"Implements": ["LoggingDriver"]}`

// shutdownTimeout limits how long the plugin sends buffered messages when
// it stops. Docker kills the plugin 10 seconds after SIGTERM.
const shutdownTimeout = 8 * time.Second

// ContainerDetails is an alias for logger.Info.
type ContainerDetails = logger.Info

// ReadConfig is an alias for logger.ReadConfig.
type ReadConfig = logger.ReadConfig

// version is set at build time.
var version = ""

func main() {
	os.Exit(run())
}

func run() int {
	env := os.Getenv("ENV")
	logLevel := os.Getenv("LOG_LEVEL")
	zapLogger, err := newLogger(env, logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	zapLogger = zapLogger.With(
		zap.String("version", version),
		zap.String("environment", env),
	)
	defer func() {
		_ = zapLogger.Sync()
	}()

	driver := NewDriver(zapLogger)

	sdkHandler := sdk.NewHandler(pluginManifest)
	srv := NewServer(zapLogger, sdkHandler, driver)

	if pprofPort := os.Getenv("PPROF_PORT"); pprofPort != "" {
		go servePprof(zapLogger, pprofPort)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve()
	}()

	exitCode := 0
	select {
	case err := <-serveErr:
		zapLogger.Error("failed to start Server", zap.Error(err))
		exitCode = 1
	case <-ctx.Done():
		zapLogger.Info("shutting down")
	}

	shutdown(zapLogger, driver)

	return exitCode
}

// shutdown stops all log streams, so that they send the buffered messages.
// It waits for at most shutdownTimeout.
func shutdown(zapLogger *zap.Logger, driver *Driver) {
	done := make(chan struct{})
	go func() {
		driver.Close()
		close(done)
	}()

	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		zapLogger.Warn("timeout while stopping log streams, some messages are lost")
	}
}

// servePprof serves the profiling endpoints on the loopback interface only.
// The plugin uses the host network, so a wildcard address would expose
// the profiles, including heap dumps with bot tokens, to the whole network.
func servePprof(zapLogger *zap.Logger, port string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	zapLogger.Info("starting pprof Server", zap.String("addr", srv.Addr))
	if err := srv.ListenAndServe(); err != nil {
		zapLogger.Error("failed to start pprof Server", zap.Error(err))
	}
}

func newLogger(env, logLevel string) (*zap.Logger, error) {
	var cfg zap.Config
	if env == "production" {
		cfg = zap.NewProductionConfig()
	} else {
		cfg = zap.NewDevelopmentConfig()
	}

	var err error
	cfg.Level, err = zap.ParseAtomicLevel(logLevel)
	if err != nil {
		return nil, fmt.Errorf("failed to parse log level: %w", err)
	}

	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	return cfg.Build()
}
