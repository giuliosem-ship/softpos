package auth

import (
	"context"
	"log/slog"
	"sync"
)

// OTPSender delivers a one-time code to a phone (SMS/WhatsApp provider).
// No real provider is wired yet; implementations must never log the code
// outside explicit dev mode.
type OTPSender interface {
	SendOTP(ctx context.Context, phone, code string) error
}

// LogSender writes the OTP to the structured log. With RevealCode=false (the
// default) only a masked phone is logged, so it is safe but unusable for real
// logins; RevealCode=true is meant only for explicit local dev mode.
type LogSender struct {
	RevealCode bool
}

func (s LogSender) SendOTP(_ context.Context, phone, code string) error {
	if s.RevealCode {
		slog.Warn("DEV MODE OTP (never enable in production)", "phone", maskPhone(phone), "code", code)
		return nil
	}
	slog.Info("otp issued (log sender: code not delivered)", "phone", maskPhone(phone))
	return nil
}

func maskPhone(phone string) string {
	if len(phone) <= 4 {
		return "****"
	}
	return "****" + phone[len(phone)-4:]
}

// MemorySender records the last code per phone (tests).
type MemorySender struct {
	mu    sync.Mutex
	codes map[string]string
	Err   error // returned by SendOTP when set
}

func NewMemorySender() *MemorySender { return &MemorySender{codes: map[string]string{}} }

func (s *MemorySender) SendOTP(_ context.Context, phone, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return s.Err
	}
	s.codes[phone] = code
	return nil
}

// LastCode returns the last code sent to phone.
func (s *MemorySender) LastCode(phone string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[phone]
	return c, ok
}
