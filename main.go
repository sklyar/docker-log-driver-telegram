package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"

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

// version is set at build time
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	driver := NewDriver(ctx, zapLogger)

	sdkHandler := sdk.NewHandler(pluginManifest)
	srv := NewServer(zapLogger, sdkHandler, driver)

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		defer cancel()

		if err := srv.Serve(); err != nil {
			zapLogger.Error("failed to start Server", zap.Error(err))
		}
	}()

	pprofPort := os.Getenv("PPROF_PORT")
	if pprofPort != "" {
		go func() {
			defer wg.Done()
			defer cancel()

			zapLogger.Info("starting pprof Server", zap.String("port", pprofPort))
			if err := http.ListenAndServe(fmt.Sprintf(":%s", pprofPort), nil); err != nil {
				zapLogger.Error("failed to start pprof Server", zap.Error(err))
			}
		}()
	}

	wg.Wait()
}

func newLogger(env string, logLevel string) (*zap.Logger, error) {
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
