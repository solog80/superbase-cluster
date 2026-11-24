package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

type mailer struct {
	host        string
	port        string
	user        string
	pass        string
	fromAddress string
	fromName    string
	implicitTLS bool
	dryRun      bool
	support     string
	redirectTo  string
}

func newMailer() *mailer {
	host := strings.TrimSpace(getenv("SMTP_HOST", "mail.saltmedia.ug"))
	user := strings.TrimSpace(os.Getenv("SMTP_USER"))
	pass := os.Getenv("SMTP_PASS")
	if path := strings.TrimSpace(os.Getenv("SMTP_PASS_FILE")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[mailer] SMTP_PASS_FILE %s: %v", path, err)
		} else {
			pass = strings.TrimSpace(string(raw))
		}
	}
	from := strings.TrimSpace(getenv("SMTP_FROM", user))
	if host == "" || from == "" {
		return nil
	}
	m := &mailer{
		host:        host,
		port:        getenv("SMTP_PORT", "587"),
		user:        user,
		pass:        pass,
		fromAddress: from,
		fromName:    getenv("SMTP_FROM_NAME", "Salt Media"),
		implicitTLS: strings.EqualFold(getenv("SMTP_TLS", "starttls"), "implicit"),
		dryRun:      getenvBool("EMAIL_DRY_RUN", false),
		support:     getenv("EMAIL_SUPPORT_ADDRESS", "info@saltmedia.ug"),
		redirectTo:  strings.TrimSpace(getenv("EMAIL_REDIRECT_TO", "")),
	}
	if m.port == "" {
		m.port = "587"
	}
	return m
}

func (m *mailer) send(to, subject, textBody, htmlBody string) error {
	toAddr, err := mail.ParseAddress(strings.TrimSpace(to))
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}
	if m.redirectTo != "" {
		redirectAddr, err := mail.ParseAddress(m.redirectTo)
		if err != nil {
			return fmt.Errorf("invalid EMAIL_REDIRECT_TO %q: %w", m.redirectTo, err)
		}
		subject = "[to: " + toAddr.Address + "] " + subject
		toAddr = redirectAddr
	}
	fromAddr, err := mail.ParseAddress(m.fromAddress)
	if err != nil {
		return fmt.Errorf("invalid sender %q: %w", m.fromAddress, err)
	}
	if m.dryRun {
		log.Printf("[mailer] dry-run to=%s subject=%q", toAddr.Address, subject)
		return nil
	}
	msg, err := buildMIMEMessage(mail.Address{Name: m.fromName, Address: fromAddr.Address}, *toAddr, subject, textBody, htmlBody)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(m.host, m.port)
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	if m.implicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))

	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()
	if !m.implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}
	if m.user != "" {
		if err := c.Auth(smtp.PlainAuth("", m.user, m.pass, m.host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(fromAddr.Address); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := c.Rcpt(toAddr.Address); err != nil {
		return fmt.Errorf("smtp rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp message close: %w", err)
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("smtp quit: %w", err)
	}
	return nil
}

func buildMIMEMessage(from mail.Address, to mail.Address, subject, textBody, htmlBody string) ([]byte, error) {
	boundary, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	boundary = "saltmedia-" + boundary
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from.String())
	fmt.Fprintf(&b, "To: %s\r\n", to.String())
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(base64Lines(textBody))
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(base64Lines(htmlBody))
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), nil
}

func base64Lines(s string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	return b.String()
}

type emailLayoutData struct {
	Title     string
	Preheader string
	Content   template.HTML
}

var emailLayout = template.Must(template.New("email-layout").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
</head>
<body style="margin:0;padding:0;background-color:#f4f4f5;font-family:Arial,Helvetica,sans-serif;color:#111111;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;">{{.Preheader}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f4f4f5;">
<tr>
<td align="center" style="padding:24px 12px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;background-color:#ffffff;border-radius:14px;overflow:hidden;border:1px solid #e5e7eb;">
<tr>
<td style="background-color:#a40000;padding:20px 26px;">
<img src="https://saltmedia.ug/images/Salt%20Logos/Salt-Media-Logo-Blue.png" alt="Salt Media" height="40" style="display:block;border:0;max-width:100%;height:40px;">
</td>
</tr>
<tr>
<td style="padding:30px 26px 10px;font-size:15px;line-height:1.65;color:#222222;">{{.Content}}</td>
</tr>
<tr>
<td style="padding:18px 26px 26px;font-size:12px;line-height:1.6;color:#6b7280;border-top:1px solid #f0f0f0;">
Salt Media &middot; <a href="https://saltmedia.ug" style="color:#a40000;text-decoration:none;">saltmedia.ug</a><br>
This is an automated message. Please do not reply.
</td>
</tr>
</table>
</td>
</tr>
</table>
</body>
</html>`))

type welcomeEmailData struct {
	Name    string
	Email   string
	Site    string
	Support string
}

var welcomeEmailHTML = template.Must(template.New("welcome-html").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Welcome to Salt Media</title>
</head>
<body style="margin:0;padding:0;background-color:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;">Your Salt Media account is ready. Live TV, Salt FM, on demand and live chat.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f4f4f5;">
<tr>
<td align="center" style="padding:24px 12px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;background-color:#ffffff;border-radius:14px;overflow:hidden;border:1px solid #e5e7eb;">
<tr>
<td style="background-color:#ffffff;padding:30px 24px 24px;text-align:center;border-bottom:1px solid #f0f0f0;">
<img src="https://saltmedia.ug/images/Salt%20Logos/Salt-Media-Logo-Blue.png" alt="Salt Media" height="40" style="display:inline-block;border:0;height:40px;max-width:100%;">
</td>
</tr>
<tr>
<td style="background-color:#141414;padding:42px 30px 38px;">
<p style="margin:0 0 10px;color:#d66a6a;font-size:11px;font-weight:bold;letter-spacing:2px;text-transform:uppercase;">Welcome to Salt Media</p>
<h1 style="margin:0 0 16px;color:#ffffff;font-size:38px;line-height:1.05;font-weight:800;letter-spacing:-0.6px;">You're in.</h1>
<p style="margin:0 0 26px;color:#b3b3b3;font-size:16px;line-height:1.6;">Hi {{.Name}}, your account is ready. Stream live TV, listen to Salt FM, and catch up on demand — all in one place.</p>
<a href="{{.Site}}" style="display:inline-block;background-color:#a40000;color:#ffffff;text-decoration:none;font-size:15px;font-weight:bold;padding:13px 26px;border-radius:4px;">Open Salt Media</a>
<p style="margin:18px 0 0;color:#666666;font-size:12px;">Watch on your phone, tablet or the web.</p>
</td>
</tr>
<tr>
<td style="padding:32px 30px 0;">
<p style="margin:0 0 8px;color:#a40000;font-size:11px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">Featured this week</p>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#111111;border-radius:10px;overflow:hidden;">
<tr>
<td>
<img src="https://edge.solofx.net/storage/v1/object/public/epg-images/posters/the_gang_bf8c6b_16x9.jpg" alt="The G.A.N.G on Salt Media" width="100%" style="display:block;width:100%;height:auto;border:0;">
</td>
</tr>
<tr>
<td style="background-color:#111111;padding:16px 18px 18px;">
<p style="margin:0 0 4px;color:#ffffff;font-size:18px;font-weight:bold;">The G.A.N.G</p>
<p style="margin:0;color:#b3b3b3;font-size:13px;line-height:1.5;">Music, conversation and more — streaming now on Salt Media.</p>
</td>
</tr>
</table>
</td>
</tr>
<tr>
<td style="padding:30px 30px 0;">
<p style="margin:0 0 6px;color:#a40000;font-size:11px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">What's waiting for you</p>
<h2 style="margin:0 0 22px;color:#111111;font-size:24px;line-height:1.2;font-weight:bold;">Everything Salt Media, in one app.</h2>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f7f7f7;border-radius:8px;border:1px solid #ececec;">
<tr>
<td style="padding:14px 16px;">
<p style="margin:0 0 4px;color:#6b7280;font-size:10px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">Your sign-in email</p>
<p style="margin:0;color:#111111;font-size:15px;font-weight:bold;">{{.Email}}</p>
</td>
</tr>
</table>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:6px;">
<tr>
<td width="50%" valign="top" style="padding:6px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f7f7f7;border-radius:8px;overflow:hidden;">
<tr><td height="120" background="https://edge.solofx.net/storage/v1/object/public/epg-images/Salt-Fm/Program_Line_up/Landscape/TV_thumbs/SALTMOVIE_200x200.webp" style="height:120px;background-color:#e5e7eb;background-image:url('https://edge.solofx.net/storage/v1/object/public/epg-images/Salt-Fm/Program_Line_up/Landscape/TV_thumbs/SALTMOVIE_200x200.webp');background-size:cover;background-position:center;background-repeat:no-repeat;"></td></tr>
<tr><td style="padding:12px 14px 14px;">
<p style="margin:0 0 6px;color:#a40000;font-size:11px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">Live TV</p>
<p style="margin:0;color:#4b5563;font-size:14px;line-height:1.5;">Salt TV on any screen.</p>
</td></tr>
</table>
</td>
<td width="50%" valign="top" style="padding:6px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f7f7f7;border-radius:8px;overflow:hidden;">
<tr><td height="120" background="https://edge.solofx.net/storage/v1/object/public/epg-images/Salt-Fm/Program_Line_up/Landscape/app_thumbnails/WORSHIP_200x200.webp" style="height:120px;background-color:#e5e7eb;background-image:url('https://edge.solofx.net/storage/v1/object/public/epg-images/Salt-Fm/Program_Line_up/Landscape/app_thumbnails/WORSHIP_200x200.webp');background-size:cover;background-position:center;background-repeat:no-repeat;"></td></tr>
<tr><td style="padding:12px 14px 14px;">
<p style="margin:0 0 6px;color:#a40000;font-size:11px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">Salt FM</p>
<p style="margin:0;color:#4b5563;font-size:14px;line-height:1.5;">Live radio, presenters and music.</p>
</td></tr>
</table>
</td>
</tr>
<tr>
<td width="50%" valign="top" style="padding:6px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f7f7f7;border-radius:8px;overflow:hidden;">
<tr><td height="120" background="https://edge.solofx.net/storage/v1/object/public/epg-images/posters/sway_8d811e_16x9.jpg" style="height:120px;background-color:#e5e7eb;background-image:url('https://edge.solofx.net/storage/v1/object/public/epg-images/posters/sway_8d811e_16x9.jpg');background-size:cover;background-position:center;background-repeat:no-repeat;"></td></tr>
<tr><td style="padding:12px 14px 14px;">
<p style="margin:0 0 6px;color:#a40000;font-size:11px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">On Demand</p>
<p style="margin:0;color:#4b5563;font-size:14px;line-height:1.5;">Catch up on shows, sermons and highlights.</p>
</td></tr>
</table>
</td>
<td width="50%" valign="top" style="padding:6px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f7f7f7;border-radius:8px;overflow:hidden;">
<tr><td height="120" background="https://edge.solofx.net/storage/v1/object/public/epg-images/posters/connected_c9bc07_16x9.jpg" style="height:120px;background-color:#e5e7eb;background-image:url('https://edge.solofx.net/storage/v1/object/public/epg-images/posters/connected_c9bc07_16x9.jpg');background-size:cover;background-position:center;background-repeat:no-repeat;"></td></tr>
<tr><td style="padding:12px 14px 14px;">
<p style="margin:0 0 6px;color:#a40000;font-size:11px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">Live Chat</p>
<p style="margin:0;color:#4b5563;font-size:14px;line-height:1.5;">Join the conversation during programs.</p>
</td></tr>
</table>
</td>
</tr>
</table>
</td>
</tr>
<tr>
<td style="padding:22px 24px 0;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#a40000;border-radius:8px;">
<tr><td style="padding:22px 24px;text-align:center;">
<p style="margin:0;color:#ffffff;font-size:16px;line-height:1.5;font-weight:bold;">Sharing hope and the Gospel across Uganda and beyond.</p>
<p style="margin:8px 0 0;color:#f3c6c6;font-size:12px;">— The Salt Media team</p>
</td></tr>
</table>
</td>
</tr>
<tr>
<td style="padding:26px 30px 32px;text-align:center;">
<a href="{{.Site}}" style="display:inline-block;background-color:#141414;color:#ffffff;text-decoration:none;font-size:14px;font-weight:bold;padding:12px 24px;border-radius:4px;">Visit saltmedia.ug</a>
<p style="margin:12px 0 0;color:#9ca3af;font-size:12px;">{{.Site}}</p>
</td>
</tr>
<tr>
<td style="background-color:#fafafa;padding:20px 30px 24px;border-top:1px solid #f0f0f0;text-align:center;">
<p style="margin:0;color:#6b7280;font-size:12px;line-height:1.6;">You received this email because you created a Salt Media account.</p>
<p style="margin:6px 0 0;color:#6b7280;font-size:12px;line-height:1.6;">Need help? <a href="mailto:{{.Support}}" style="color:#a40000;text-decoration:none;">{{.Support}}</a></p>
<p style="margin:14px 0 0;color:#9ca3af;font-size:11px;">Salt Media &middot; <a href="https://saltmedia.ug" style="color:#9ca3af;text-decoration:none;">saltmedia.ug</a></p>
</td>
</tr>
</table>
</td>
</tr>
</table>
</body>
</html>`))

func renderWelcomeEmail(name, emailAddr, support string) (string, string) {
	if strings.TrimSpace(name) == "" {
		name = "there"
	}
	data := welcomeEmailData{Name: name, Email: emailAddr, Site: "https://saltmedia.ug", Support: support}
	var body bytes.Buffer
	_ = welcomeEmailHTML.Execute(&body, data)
	textBody := fmt.Sprintf("Hi %s,\n\nYou're in. Welcome to Salt Media.\n\nYour account is ready. Stream live TV, listen to Salt FM, and catch up on demand.\n\nSign in with: %s\n\nWhat's waiting for you:\n- Live TV: Salt TV on any screen.\n- Salt FM: Live radio, presenters and music.\n- On Demand: Catch up on shows, sermons and highlights.\n- Live Chat: Join the conversation during programs.\n\nOpen Salt Media: %s\n\nSharing hope and the Gospel across Uganda and beyond.\n\nYou received this email because you created a Salt Media account.\nNeed help? %s\n\nSalt Media\nhttps://saltmedia.ug\n", data.Name, data.Email, data.Site, support)
	return textBody, body.String()
}

type contributionEmailData struct {
	Name          string
	Amount        string
	Reason        string
	TransactionID string
	ReceiptNo     string
	Reference     string
	Date          string
	Phone         string
	Site          string
	Support       string
}

var contributionEmailHTML = template.Must(template.New("contribution-html").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Contribution received</title>
</head>
<body style="margin:0;padding:0;background-color:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;">Your contribution of {{.Amount}} has been received. Thank you for giving.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f4f4f5;">
<tr>
<td align="center" style="padding:24px 12px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;background-color:#ffffff;border-radius:14px;overflow:hidden;border:1px solid #e5e7eb;">
<tr>
<td style="background-color:#ffffff;padding:30px 24px 24px;text-align:center;border-bottom:1px solid #f0f0f0;">
<img src="https://saltmedia.ug/images/Salt%20Logos/Salt-Media-Logo-Blue.png" alt="Salt Media" height="40" style="display:inline-block;border:0;height:40px;max-width:100%;">
</td>
</tr>
<tr>
<td style="background-color:#141414;padding:42px 30px 38px;">
<p style="margin:0 0 10px;color:#d66a6a;font-size:11px;font-weight:bold;letter-spacing:2px;text-transform:uppercase;">Contribution received</p>
<h1 style="margin:0 0 16px;color:#ffffff;font-size:34px;line-height:1.1;font-weight:800;letter-spacing:-0.5px;">Thank you, {{.Name}}.</h1>
<p style="margin:0;color:#b3b3b3;font-size:16px;line-height:1.6;">We have received your gift of <strong style="color:#ffffff;">{{.Amount}}</strong> towards <strong style="color:#ffffff;">{{.Reason}}</strong>.</p>
</td>
</tr>
<tr>
<td style="padding:30px 30px 0;">
<p style="margin:0 0 8px;color:#a40000;font-size:11px;font-weight:bold;letter-spacing:1.5px;text-transform:uppercase;">Your receipt</p>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f7f7f7;border-radius:8px;border:1px solid #ececec;">
<tr>
<td style="padding:16px 18px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;">
<tr>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Amount</td>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#111111;font-size:14px;font-weight:bold;text-align:right;">{{.Amount}}</td>
</tr>
<tr>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Purpose</td>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#111111;font-size:14px;text-align:right;">{{.Reason}}</td>
</tr>
<tr>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Receipt No.</td>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#111111;font-size:14px;font-weight:bold;text-align:right;">{{.ReceiptNo}}</td>
</tr>
<tr>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Reference</td>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#111111;font-size:13px;font-family:Menlo,Consolas,monospace;text-align:right;">{{.Reference}}</td>
</tr>
<tr>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Date</td>
<td style="padding:8px 0;border-bottom:1px solid #e5e7eb;color:#111111;font-size:14px;text-align:right;">{{.Date}}</td>
</tr>
<tr>
<td style="padding:8px 0;color:#6b7280;font-size:13px;">Phone</td>
<td style="padding:8px 0;color:#111111;font-size:14px;text-align:right;">{{.Phone}}</td>
</tr>
</table>
</td>
</tr>
</table>
</td>
</tr>
<tr>
<td style="padding:22px 24px 0;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#a40000;border-radius:8px;">
<tr><td style="padding:22px 24px;text-align:center;">
<p style="margin:0;color:#ffffff;font-size:16px;line-height:1.5;font-weight:bold;">Your generosity helps Salt Media share hope and the Gospel across Uganda and beyond.</p>
<p style="margin:8px 0 0;color:#f3c6c6;font-size:12px;">Thank you for giving.</p>
</td></tr>
</table>
</td>
</tr>
<tr>
<td style="padding:26px 30px 32px;text-align:center;">
<a href="{{.Site}}" style="display:inline-block;background-color:#a40000;color:#ffffff;text-decoration:none;font-size:14px;font-weight:bold;padding:12px 24px;border-radius:4px;">Visit Salt Media</a>
<p style="margin:12px 0 0;color:#9ca3af;font-size:12px;">{{.Site}}</p>
</td>
</tr>
<tr>
<td style="background-color:#fafafa;padding:20px 30px 24px;border-top:1px solid #f0f0f0;text-align:center;">
<p style="margin:0;color:#6b7280;font-size:12px;line-height:1.6;">This is an automated receipt for your records.</p>
<p style="margin:6px 0 0;color:#6b7280;font-size:12px;line-height:1.6;">Need help? <a href="mailto:{{.Support}}" style="color:#a40000;text-decoration:none;">{{.Support}}</a></p>
<p style="margin:14px 0 0;color:#9ca3af;font-size:11px;">Salt Media &middot; <a href="https://saltmedia.ug" style="color:#9ca3af;text-decoration:none;">saltmedia.ug</a></p>
</td>
</tr>
</table>
</td>
</tr>
</table>
</body>
</html>`))

func renderContributionEmail(d contributionEmailData) (string, string) {
	if d.Site == "" {
		d.Site = "https://saltmedia.ug"
	}
	if d.Support == "" {
		d.Support = "info@saltmedia.ug"
	}
	var body bytes.Buffer
	_ = contributionEmailHTML.Execute(&body, d)
	textBody := fmt.Sprintf("Dear %s,\n\nThank you for your contribution of %s towards %s.\n\nYour receipt:\nAmount: %s\nPurpose: %s\nReceipt No.: %s\nReference: %s\nDate: %s\nPhone: %s\n\nYour generosity helps Salt Media share hope and the Gospel across Uganda and beyond. Thank you for giving.\n\nVisit Salt Media: %s\n\nThis is an automated receipt for your records.\nNeed help? %s\n\nSalt Media\nhttps://saltmedia.ug\n", d.Name, d.Amount, d.Reason, d.Amount, d.Reason, d.ReceiptNo, d.Reference, d.Date, d.Phone, d.Site, d.Support)
	return textBody, body.String()
}

var contributionAdminEmailBody = template.Must(template.New("contribution-admin-email").Parse(`<p style="margin:0 0 8px;font-size:20px;font-weight:bold;color:#111111;">Contribution received</p>
<p style="margin:0 0 18px;">A contribution was received but could not be matched to a registered app account, so no receipt was sent to the contributor.</p>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;margin:0 0 20px;">
<tr><td style="padding:9px 0;border-bottom:1px solid #eeeeee;color:#6b7280;font-size:13px;">Payer</td><td style="padding:9px 0;border-bottom:1px solid #eeeeee;text-align:right;font-weight:bold;">{{.Name}}</td></tr>
<tr><td style="padding:9px 0;border-bottom:1px solid #eeeeee;color:#6b7280;font-size:13px;">Amount</td><td style="padding:9px 0;border-bottom:1px solid #eeeeee;text-align:right;font-weight:bold;">{{.Amount}}</td></tr>
<tr><td style="padding:9px 0;border-bottom:1px solid #eeeeee;color:#6b7280;font-size:13px;">Purpose</td><td style="padding:9px 0;border-bottom:1px solid #eeeeee;text-align:right;">{{.Reason}}</td></tr>
<tr><td style="padding:9px 0;border-bottom:1px solid #eeeeee;color:#6b7280;font-size:13px;">Phone</td><td style="padding:9px 0;border-bottom:1px solid #eeeeee;text-align:right;">{{.Phone}}</td></tr>
<tr><td style="padding:9px 0;border-bottom:1px solid #eeeeee;color:#6b7280;font-size:13px;">Transaction ID</td><td style="padding:9px 0;border-bottom:1px solid #eeeeee;text-align:right;">{{.TransactionID}}</td></tr>
<tr><td style="padding:9px 0;color:#6b7280;font-size:13px;">Date</td><td style="padding:9px 0;text-align:right;">{{.Date}}</td></tr>
</table>`))

func renderContributionAdminEmail(d contributionEmailData) (string, string) {
	var body bytes.Buffer
	_ = contributionAdminEmailBody.Execute(&body, d)
	htmlBody := renderEmailHTML("Contribution received", "A contribution could not be matched to an app account.", body.String())
	textBody := fmt.Sprintf("Contribution received\n\nPayer: %s\nAmount: %s\nPurpose: %s\nPhone: %s\nTransaction ID: %s\nDate: %s\n\nNo registered app account matched this payment, so no receipt was sent to the contributor.\n", d.Name, d.Amount, d.Reason, d.Phone, d.TransactionID, d.Date)
	return textBody, htmlBody
}

func renderEmailHTML(title, preheader, content string) string {
	var b bytes.Buffer
	_ = emailLayout.Execute(&b, emailLayoutData{
		Title:     title,
		Preheader: preheader,
		Content:   template.HTML(content),
	})
	return b.String()
}

func formatUGX(v any) string {
	n := toInt64(v)
	if n <= 0 {
		return "UGX 0"
	}
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return "UGX " + string(out)
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(t, ",", ""))
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f)
		}
	}
	return 0
}
