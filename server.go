package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/go-plugins-helpers/sdk"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type driver interface {
	StartLogging(streamPath string, containerDetails *ContainerDetails) (*logStream, error)
	ReadLogs(ctx context.Context, containerID string, config *ReadConfig) (io.ReadCloser, error)
	StopLogging(streamPath string) error
}

type (
	logDriverStartLoggingRequest struct {
		File string           `json:"File"`
		Info ContainerDetails `json:"Info"`
	}

	logDriverReadLogsRequest struct {
		Info struct {
			ContainerID string `json:"ContainerID"`
		} `json:"Info"`
		Config ReadConfig `json:"Config"`
	}

	logDriverStopLoggingRequest struct {
		File string `json:"File"`
	}

	response struct {
		Cap *logger.Capability `json:"Cap,omitempty"`
		Err string             `json:"Err,omitempty"`
	}
)

// Server is a server that handles requests from Docker.
type Server struct {
	driver     driver
	sdkHandler sdk.Handler

	zapLogger *zap.Logger
}

func NewServer(zapLogger *zap.Logger, sdkHandler sdk.Handler, driver driver) *Server {
	srv := &Server{
		driver:     driver,
		sdkHandler: sdkHandler,
		zapLogger:  zapLogger,
	}

	sdkHandler.HandleFunc("/LogDriver.StartLogging", srv.startLoggingHandler)
	sdkHandler.HandleFunc("/LogDriver.StopLogging", srv.stopLoggingHandler)
	sdkHandler.HandleFunc("/LogDriver.Capabilities", srv.capabilitiesHandler)
	sdkHandler.HandleFunc("/LogDriver.ReadLogs", srv.readLogsHandler)

	return srv
}

func (s *Server) Serve() error {
	if err := s.sdkHandler.ServeUnix(driverName, 0); err != nil {
		return errors.Wrap(err, "failed to serve unix socket")
	}
	return nil
}

func (s *Server) startLoggingHandler(w http.ResponseWriter, r *http.Request) {
	var req logDriverStartLoggingRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		s.writeResponse(w, http.StatusBadRequest, err)
		s.zapLogger.Error(
			"failed to decode request",
			zap.String("path", r.URL.Path),
			zap.Error(err),
		)
		return
	}

	if _, err := s.driver.StartLogging(req.File, &req.Info); err != nil {
		s.writeResponse(w, http.StatusInternalServerError, err)
		s.zapLogger.Error(
			"failed to start logging",
			zap.String("container_name", req.Info.ContainerName),
			zap.Error(err),
		)
		return
	}

	s.zapLogger.Debug("start logging request was called for the container", zap.Any("req", req))

	s.writeResponse(w, http.StatusOK, nil)
}

func (s *Server) stopLoggingHandler(w http.ResponseWriter, r *http.Request) {
	var req logDriverStopLoggingRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		s.writeResponse(w, http.StatusBadRequest, err)
		s.zapLogger.Error(
			"failed to decode request",
			zap.String("path", r.URL.Path),
			zap.Error(err),
		)
		return
	}

	if err := s.driver.StopLogging(req.File); err != nil {
		s.writeResponse(w, http.StatusInternalServerError, err)
		s.zapLogger.Error(
			"failed to stop logging",
			zap.String("streamPath", req.File),
			zap.Error(err),
		)
		return
	}

	s.zapLogger.Debug("stop logging request was called for the container", zap.Any("req", req))

	s.writeResponse(w, http.StatusOK, nil)
}

func (s *Server) capabilitiesHandler(w http.ResponseWriter, _ *http.Request) {
	s.zapLogger.Debug("capabilities request was called")

	_ = json.NewEncoder(w).Encode(response{
		Cap: &logger.Capability{
			ReadLogs: true,
		},
	})
}

func (s *Server) readLogsHandler(w http.ResponseWriter, r *http.Request) {
	var req logDriverReadLogsRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		s.writeResponse(w, http.StatusBadRequest, err)
		s.zapLogger.Error(
			"failed to decode request",
			zap.String("path", r.URL.Path),
			zap.Error(err),
		)
		return
	}

	containerID := req.Info.ContainerID

	lr, err := s.driver.ReadLogs(r.Context(), containerID, &req.Config)
	if err != nil {
		s.writeResponse(w, http.StatusInternalServerError, err)
		s.zapLogger.Error(
			"failed to read logs",
			zap.String("container_id", containerID),
			zap.Any("req", req),
			zap.Error(err),
		)
		return
	}
	defer lr.Close()

	s.zapLogger.Debug(
		"read logs request was called for the container",
		zap.String("container_id", containerID),
		zap.Any("req", req),
	)

	w.Header().Set("Content-Type", "application/x-json-stream")

	wf := ioutils.NewWriteFlusher(w)
	_, _ = io.Copy(wf, lr)
}

func (s *Server) writeResponse(w http.ResponseWriter, status int, err error) {
	resp := response{}
	if err != nil {
		resp.Err = err.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(resp)
}
