package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

type SMTPConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
	To       []string
	TLS      string // starttls (default), tls (implicit, port 465), none
}

func (c SMTPConfig) Enabled() bool {
	return c.Host != "" && c.From != "" && len(c.To) > 0
}

func (c SMTPConfig) Send(subject, body string) error {
	addr := net.JoinHostPort(c.Host, c.Port)
	tlsCfg := &tls.Config{ServerName: c.Host}
	dialer := &net.Dialer{Timeout: 15 * time.Second}

	var conn net.Conn
	var err error
	if c.TLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(45 * time.Second))
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()

	if c.TLS == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return fmt.Errorf("server %s does not offer STARTTLS (set SMTP_TLS=none to send unencrypted)", addr)
		}
		if err := cl.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if c.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := cl.Mail(c.From); err != nil {
		return err
	}
	for _, to := range c.To {
		if err := cl.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	id := make([]byte, 12)
	rand.Read(id)
	domain := c.Host
	if i := strings.LastIndex(c.From, "@"); i >= 0 {
		domain = strings.TrimRight(c.From[i+1:], "> ")
	}
	msg := strings.Join([]string{
		"From: " + c.From,
		"To: " + strings.Join(c.To, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: <" + hex.EncodeToString(id) + "@" + domain + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"),
	}, "\r\n")
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
