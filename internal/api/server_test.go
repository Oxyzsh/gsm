package api

import (
	"context"
	"log/slog"
	"net"
	"testing"

	gsmv1 "gsm/api/gen/gsm/v1"
	"gsm/internal/modem"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type stubController struct {
	statusResult modem.StatusResult
	statusErr    error
	smsResult    modem.SendResult
	smsErr       error
	voiceErr     error
	smsCalls     int
}

func (s *stubController) Status(context.Context) (modem.StatusResult, error) {
	return s.statusResult, s.statusErr
}
func (s *stubController) SetEnabled(context.Context, bool) (modem.StatusResult, error) {
	return s.statusResult, nil
}
func (s *stubController) SendSMS(context.Context, string, string) (modem.SendResult, error) {
	s.smsCalls++
	return s.smsResult, s.smsErr
}
func (s *stubController) SendVoice(context.Context, string, string) (modem.StatusResult, error) {
	return modem.StatusResult{}, s.voiceErr
}
func (s *stubController) Redial(context.Context) error       { return nil }
func (s *stubController) Reinitialize(context.Context) error { return nil }

func startGRPC(t *testing.T, controller modem.Controller) gsmv1.GsmServiceClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	gsmv1.RegisterGsmServiceServer(server, New(controller, new(slog.LevelVar)))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gsmv1.NewGsmServiceClient(conn)
}

func TestGetStatusMapsResult(t *testing.T) {
	client := startGRPC(t, &stubController{statusResult: modem.StatusResult{
		Status: modem.StatusNoSignal, Enabled: true, RSSI: 12, BER: 0, HWType: "em05-ce", Firmware: "A04", Detail: "weak",
	}})
	response, err := client.GetStatus(context.Background(), &gsmv1.GetStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetStatus() != gsmv1.GsmStatus_GSM_STATUS_NO_SIGNAL || response.GetRssi() != 12 || response.GetHwType() != "em05-ce" || response.GetFirmware() != "A04" {
		t.Fatalf("response=%+v", response)
	}
}

func TestSendSMSValidationMapsToInvalidArgument(t *testing.T) {
	stub := &stubController{smsErr: &modem.ValidationError{Field: "phone", Reason: "bad"}}
	client := startGRPC(t, stub)
	_, err := client.SendSMS(context.Background(), &gsmv1.SendSMSRequest{PhoneNumber: "bad", Message: "x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code=%v err=%v", status.Code(err), err)
	}
}

func TestSendVoiceUnsupportedMapsToFailedPrecondition(t *testing.T) {
	client := startGRPC(t, &stubController{voiceErr: modem.ErrVoiceUnsupported})
	_, err := client.SendVoice(context.Background(), &gsmv1.SendVoiceRequest{PhoneNumber: "13612345678", Message: "x"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code=%v err=%v", status.Code(err), err)
	}
}

func TestCancellationIsPreserved(t *testing.T) {
	client := startGRPC(t, &stubController{statusErr: context.Canceled})
	_, err := client.GetStatus(context.Background(), &gsmv1.GetStatusRequest{})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("code=%v err=%v", status.Code(err), err)
	}
}

func TestSetLogLevel(t *testing.T) {
	level := new(slog.LevelVar)
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	gsmv1.RegisterGsmServiceServer(server, New(&stubController{}, level))
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := gsmv1.NewGsmServiceClient(conn)
	if _, err := client.SetLogLevel(context.Background(), &gsmv1.SetLogLevelRequest{Level: gsmv1.SetLogLevelRequest_LEVEL_DEBUG}); err != nil {
		t.Fatal(err)
	}
	if level.Level() != slog.LevelDebug {
		t.Fatalf("level=%v", level.Level())
	}
}
