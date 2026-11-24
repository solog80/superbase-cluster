package main

import (
	"bufio"
	"bytes"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPhoneVariants(t *testing.T) {
	got := phoneVariants("256788198401")
	want := map[string]bool{"256788198401": true, "+256788198401": true, "0788198401": true, "788198401": true}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("unexpected variant %q in %v", v, got)
		}
		delete(want, v)
	}
	if len(want) != 0 {
		t.Fatalf("missing variants %v (got %v)", want, got)
	}
}

func TestPrettyName(t *testing.T) {
	cases := map[string]string{
		"Kiiza_Joel":          "Kiiza Joel",
		"  yoyeta__balyeku  ": "Yoyeta Balyeku",
		"":                    "",
	}
	for in, want := range cases {
		if got := prettyName(in); got != want {
			t.Fatalf("prettyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatUGX(t *testing.T) {
	cases := map[any]string{
		float64(5000):   "UGX 5,000",
		float64(200000): "UGX 200,000",
		"1100":          "UGX 1,100",
		nil:             "UGX 0",
	}
	for in, want := range cases {
		if got := formatUGX(in); got != want {
			t.Fatalf("formatUGX(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestContributionEmailEscapesInput(t *testing.T) {
	text, body := renderContributionEmail(contributionEmailData{
		Name:          "<script>alert(1)</script>",
		Amount:        "UGX 5,000",
		Reason:        "Contribution",
		TransactionID: "123",
		Date:          "21 Nov 2026",
		Phone:         "256788198401",
	})
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("html body did not escape user input")
	}
	if !strings.Contains(text, "<script>alert(1)</script>") {
		t.Fatal("plain text body should keep the raw name")
	}
}

func TestDumpWelcomeSample(t *testing.T) {
	if os.Getenv("DUMP_WELCOME_SAMPLE") == "" {
		t.Skip("set DUMP_WELCOME_SAMPLE=1 to write /tmp/welcome-sample.*")
	}
	text, body := renderWelcomeEmail("Solog", "solog80@gmail.com", "info@saltmedia.ug")
	if err := os.WriteFile("/tmp/welcome-sample.html", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/welcome-sample.txt", []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDumpContributionSample(t *testing.T) {
	if os.Getenv("DUMP_CONTRIBUTION_SAMPLE") == "" {
		t.Skip("set DUMP_CONTRIBUTION_SAMPLE=1 to write /tmp/contribution-sample.*")
	}
	text, body := renderContributionEmail(contributionEmailData{
		Name:          "Solog",
		Amount:        "UGX 50,000",
		Reason:        "Contribution",
		TransactionID: "TEST-20261121-001",
		Date:          "21 Nov 2026, 15:00 UTC",
		Phone:         "256 700 000 000",
	})
	if err := os.WriteFile("/tmp/contribution-sample.html", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/contribution-sample.txt", []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewMailerReadsPasswordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smtp_pass")
	if err := os.WriteFile(path, []byte("super-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SMTP_HOST", "mail.example.com")
	t.Setenv("SMTP_USER", "no-reply@example.com")
	t.Setenv("SMTP_PASS", "")
	t.Setenv("SMTP_PASS_FILE", path)
	m := newMailer()
	if m == nil {
		t.Fatal("newMailer returned nil")
	}
	if m.pass != "super-secret" {
		t.Fatalf("pass = %q, want super-secret", m.pass)
	}
}

func TestLiveSMTP(t *testing.T) {
	if os.Getenv("SMTP_LIVE_TEST") == "" {
		t.Skip("set SMTP_LIVE_TEST=1 to send a real message")
	}
	loadDotEnv()
	m := newMailer()
	if m == nil {
		t.Fatal("SMTP is not configured")
	}
	to := strings.TrimSpace(os.Getenv("SMTP_TEST_TO"))
	if to == "" {
		t.Fatal("SMTP_TEST_TO is required")
	}
	body := renderEmailHTML("Salt Media mail test", "SMTP delivery test", "<p>This is a test message from the Salt Media mesh.</p>")
	if err := m.send(to, "Salt Media system email test", "This is a test message from the Salt Media mesh.", body); err != nil {
		t.Fatal(err)
	}
}

func TestMailerSend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	messages := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		write := func(s string) {
			_, _ = writer.WriteString(s + "\r\n")
			_ = writer.Flush()
		}
		write("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					write("250 OK")
					continue
				}
				data.WriteString(line)
				data.WriteString("\n")
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				write("250-fake")
				write("250 AUTH PLAIN LOGIN")
			case strings.HasPrefix(line, "AUTH"):
				write("235 2.7.0 ok")
			case strings.HasPrefix(line, "MAIL FROM"):
				write("250 2.1.0 ok")
			case strings.HasPrefix(line, "RCPT TO"):
				write("250 2.1.5 ok")
			case strings.HasPrefix(line, "DATA"):
				write("354 End data with <CR><LF>.<CR><LF>")
				inData = true
			case strings.HasPrefix(line, "QUIT"):
				write("221 2.0.0 bye")
				messages <- data.String()
				return
			default:
				write("250 ok")
			}
		}
	}()
	m := &mailer{
		host:        "127.0.0.1",
		port:        strconv.Itoa(ln.Addr().(*net.TCPAddr).Port),
		user:        "u",
		pass:        "p",
		fromAddress: "no-reply@saltmedia.ug",
		fromName:    "Salt Media",
	}
	if err := m.send("user@example.com", "Hi", "text body", "<p>html body</p>"); err != nil {
		t.Fatal(err)
	}
	msg := <-messages
	if !strings.Contains(msg, "multipart/alternative") {
		t.Fatalf("missing multipart content type:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Transfer-Encoding: base64") {
		t.Fatalf("missing base64 transfer encoding:\n%s", msg)
	}
}

func TestBuildMIMEMessage(t *testing.T) {
	from, err := mail.ParseAddress("Salt Media <no-reply@saltmedia.ug>")
	if err != nil {
		t.Fatal(err)
	}
	to, err := mail.ParseAddress("user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := buildMIMEMessage(*from, *to, "Hello τ", "plain", "<p>html</p>")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(msg, []byte("To: <user@example.com>")) {
		t.Fatalf("missing To header:\n%s", msg)
	}
	if !bytes.Contains(msg, []byte("multipart/alternative")) {
		t.Fatalf("missing multipart content type:\n%s", msg)
	}
	if bytes.Contains(msg, []byte("Subject: Hello τ")) {
		t.Fatal("non-ASCII subject should be encoded")
	}
	if !bytes.Contains(msg, []byte("Subject: =?utf-8?")) {
		t.Fatalf("missing encoded subject:\n%s", msg)
	}
}
