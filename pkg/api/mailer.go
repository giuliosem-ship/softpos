package api

import (
	"context"
	"log/slog"
)

// Mailer sends email (CFDI delivery to the receptor). Only LogMailer exists
// until an SMTP / email provider is contracted; a real one implements this
// interface (docs/CFDI.md).
type Mailer interface {
	Send(ctx context.Context, m Mail) error
}

// Mail is one message with attachments.
type Mail struct {
	To          string
	Subject     string
	Body        string
	Attachments []MailAttachment
}

// MailAttachment is a named file.
type MailAttachment struct {
	Name        string
	ContentType string
	Data        []byte
}

// LogMailer logs the message metadata instead of sending it.
type LogMailer struct{}

func (LogMailer) Send(_ context.Context, m Mail) error {
	names := make([]string, 0, len(m.Attachments))
	size := 0
	for _, a := range m.Attachments {
		names = append(names, a.Name)
		size += len(a.Data)
	}
	slog.Info("mail not sent (log mailer)", "to", m.To, "subject", m.Subject, "attachments", names, "bytes", size)
	return nil
}

// SetMailer replaces the mailer (default LogMailer).
func (s *Server) SetMailer(m Mailer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailer = m
}

func (s *Server) mailSender() Mailer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mailer
}
