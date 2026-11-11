package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/go-plugins-helpers/sdk"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const pluginManifest = `{"Implements": ["LoggingDriver"]}`

// ContainerDetails is an alias for logger.Info.
type ContainerDetails = logger.Info

// ReadConfig is an alias for logger.ReadConfig.
type ReadConfig = logger.ReadConfig

// version is set at build time.
var version = ""

func main() {
	env := os.Getenv("ENV")
	logLevel := os.Getenv("LOG_LEVEL")
	zapLogger, err := newLogger(env, logLevel)
	if err != nil {
		panic(err)
	}
	zapLogger = zapLogger.With(
		zap.String("version", version),
		zap.String("environment", env),
	)

	driver := NewDriver(zapLogger)

	sdkHandler := sdk.NewHandler(pluginManifest)
	srv := NewServer(zapLogger, sdkHandler, driver)

	if pprofPort := os.Getenv("PPROF_PORT"); pprofPort != "" {
		go servePprof(zapLogger, pprofPort)
	}

	if err := srv.Serve(); err != nil {
		zapLogger.Error("failed to start Server", zap.Error(err))
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
