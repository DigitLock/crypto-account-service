// Package crstest is a fake CRS for tests (docs/test-plan-s2.md §1): a gRPC server of the vendored contract on
// a loopback port whose answers the test scripts per currency. It is used only by tests.
package crstest

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/DigitLock/crypto-account-service/internal/crs/pb"
)

// WrongDouble is the double field rate of every answer: far from any real rate, so a quote that read it would
// show at once (D-15: the double is never used).
const WrongDouble = 987654.321

// Answer is the scripted answer for one currency. A Code other than codes.OK is returned as a gRPC status.
type Answer struct {
	RateDecimal string
	Outdated    bool
	Code        codes.Code
	Delay       time.Duration
}

// Server is a running fake CRS.
type Server struct {
	pb.UnimplementedCurrencyRateServiceServer
	Addr string

	mu      sync.Mutex
	answers map[string]Answer
	calls   []*pb.GetRateRequest
}

// Start serves on a loopback port until the end of the test. A currency without an answer is NOT_FOUND.
func Start(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: ln.Addr().String(), answers: map[string]Answer{}}
	srv := grpc.NewServer()
	pb.RegisterCurrencyRateServiceServer(srv, s)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return s
}

// Set scripts the answer for currency → USD.
func (s *Server) Set(currency string, a Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[currency] = a
}

// Calls returns the requests received so far.
func (s *Server) Calls() []*pb.GetRateRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*pb.GetRateRequest, len(s.calls))
	copy(out, s.calls)
	return out
}

// GetRate implements the contract.
func (s *Server) GetRate(ctx context.Context, req *pb.GetRateRequest) (*pb.GetRateResponse, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	a, ok := s.answers[req.GetFromCurrency()]
	s.mu.Unlock()
	if !ok || req.GetToCurrency() != "USD" {
		return nil, status.Error(codes.NotFound, "rate not found")
	}
	if a.Delay > 0 {
		select {
		case <-time.After(a.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if a.Code != codes.OK {
		return nil, status.Error(a.Code, "scripted failure")
	}
	return &pb.GetRateResponse{Rate: &pb.Rate{
		FromCurrency: req.GetFromCurrency(), ToCurrency: "USD", Rate: WrongDouble, UpdatedAt: timestamppb.Now(),
		IsOutdated: a.Outdated, SourceProvider: "fake", RateDecimal: a.RateDecimal,
	}}, nil
}
