package api

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	gsmv1 "gsm/api/gen/gsm/v1"
	"gsm/internal/modem"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	gsmv1.UnimplementedGsmServiceServer

	controller modem.Controller
	level      *slog.LevelVar
	closing    atomic.Bool
}

func New(controller modem.Controller, level *slog.LevelVar) *Server {
	return &Server{controller: controller, level: level}
}

func (s *Server) BeginShutdown() {
	s.closing.Store(true)
}

func (s *Server) GetStatus(ctx context.Context, _ *gsmv1.GetStatusRequest) (*gsmv1.GetStatusResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	result, err := s.controller.Status(ctx)
	if err != nil {
		if mapped := mapContextError(err); mapped != nil {
			return nil, mapped
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gsmv1.GetStatusResponse{
		Status:    toProtoStatus(result.Status),
		Enabled:   result.Enabled,
		Rssi:      int32(result.RSSI),
		Ber:       int32(result.BER),
		HwType:    result.HWType,
		Firmware:  result.Firmware,
		Detail:    result.Detail,
		CheckedAt: timestamppb.New(result.CheckedAt),
	}, nil
}

func (s *Server) SetEnabled(ctx context.Context, request *gsmv1.SetEnabledRequest) (*gsmv1.SetEnabledResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	result, err := s.controller.SetEnabled(ctx, request.GetEnabled())
	if err != nil {
		if statusErr := mapContextError(err); statusErr != nil {
			return nil, statusErr
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gsmv1.SetEnabledResponse{Status: toProtoStatus(result.Status), Detail: result.Detail}, nil
}

func (s *Server) SendSMS(ctx context.Context, request *gsmv1.SendSMSRequest) (*gsmv1.SendSMSResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	result, err := s.controller.SendSMS(ctx, request.GetPhoneNumber(), request.GetMessage())
	if err != nil {
		var validation *modem.ValidationError
		switch {
		case errors.As(err, &validation):
			return nil, status.Error(codes.InvalidArgument, validation.Error())
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, mapContextError(err)
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	return &gsmv1.SendSMSResponse{
		Status:   toProtoStatus(result.Status),
		Detail:   result.Detail,
		Segments: int32(result.Segments),
	}, nil
}

func (s *Server) SendVoice(ctx context.Context, request *gsmv1.SendVoiceRequest) (*gsmv1.SendVoiceResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	result, err := s.controller.SendVoice(ctx, request.GetPhoneNumber(), request.GetMessage())
	if err != nil {
		var validation *modem.ValidationError
		switch {
		case errors.As(err, &validation):
			return nil, status.Error(codes.InvalidArgument, validation.Error())
		case errors.Is(err, modem.ErrVoiceUnsupported):
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, mapContextError(err)
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	return &gsmv1.SendVoiceResponse{Status: toProtoStatus(result.Status), Detail: result.Detail}, nil
}

func (s *Server) SetLogLevel(_ context.Context, request *gsmv1.SetLogLevelRequest) (*gsmv1.SetLogLevelResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if s.level == nil {
		return nil, status.Error(codes.FailedPrecondition, "runtime log level is not configured")
	}
	switch request.GetLevel() {
	case gsmv1.SetLogLevelRequest_LEVEL_DEBUG:
		s.level.Set(slog.LevelDebug)
	case gsmv1.SetLogLevelRequest_LEVEL_INFO:
		s.level.Set(slog.LevelInfo)
	case gsmv1.SetLogLevelRequest_LEVEL_WARN:
		s.level.Set(slog.LevelWarn)
	case gsmv1.SetLogLevelRequest_LEVEL_ERROR:
		s.level.Set(slog.LevelError)
	default:
		return nil, status.Error(codes.InvalidArgument, "log level must be specified")
	}
	return &gsmv1.SetLogLevelResponse{}, nil
}

func (s *Server) available() error {
	if s.closing.Load() {
		return status.Error(codes.Unavailable, "gsmd is shutting down")
	}
	return nil
}

func toProtoStatus(value modem.Status) gsmv1.GsmStatus {
	switch value {
	case modem.StatusOK:
		return gsmv1.GsmStatus_GSM_STATUS_OK
	case modem.StatusDisabled:
		return gsmv1.GsmStatus_GSM_STATUS_DISABLED
	case modem.StatusNoHW:
		return gsmv1.GsmStatus_GSM_STATUS_NO_HW
	case modem.StatusNoCard:
		return gsmv1.GsmStatus_GSM_STATUS_NO_CARD
	case modem.StatusNoSignal:
		return gsmv1.GsmStatus_GSM_STATUS_NO_SIGNAL
	case modem.StatusDialFail:
		return gsmv1.GsmStatus_GSM_STATUS_DIAL_FAIL
	case modem.StatusBusy:
		return gsmv1.GsmStatus_GSM_STATUS_BUSY
	case modem.StatusOperationFailed:
		return gsmv1.GsmStatus_GSM_STATUS_OPERATION_FAILED
	default:
		return gsmv1.GsmStatus_GSM_STATUS_UNSPECIFIED
	}
}

func mapContextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		return nil
	}
}
