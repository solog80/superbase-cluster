package main

import "testing"

func TestNormalizeYoPhone(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"+256701234567", "256701234567", true},
		{"0701234567", "256701234567", true},
		{"256751234567", "256751234567", true},
		{"+256741234567", "256741234567", true},
		{"+256771234567", "", false},
		{"+256781234567", "", false},
		{"+25670123", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeYoPhone(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("normalizeYoPhone(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizePaymentStatus(t *testing.T) {
	cases := []struct {
		transactionStatus string
		status            string
		want              string
	}{
		{"SUCCEEDED", "OK", paymentStatusDone},
		{"SUCCESSFUL", "", paymentStatusDone},
		{"PENDING", "OK", paymentStatusPending},
		{"FAILED", "", paymentStatusFailed},
		{"REJECTED", "", paymentStatusFailed},
		{"", "ERROR", paymentStatusFailed},
		{"", "OK", paymentStatusPending},
	}
	for _, tc := range cases {
		if got := normalizePaymentStatus(tc.transactionStatus, tc.status); got != tc.want {
			t.Fatalf("normalizePaymentStatus(%q,%q) = %q, want %q", tc.transactionStatus, tc.status, got, tc.want)
		}
	}
}

func TestParseAmount(t *testing.T) {
	if got, err := parseAmount("50000"); err != nil || got != 50000 {
		t.Fatalf("parseAmount(50000) = (%v,%v)", got, err)
	}
	if _, err := parseAmount("0"); err == nil {
		t.Fatal("parseAmount(0) should fail")
	}
	if _, err := parseAmount("10000001"); err == nil {
		t.Fatal("parseAmount above max should fail")
	}
	if _, err := parseAmount("abc"); err == nil {
		t.Fatal("parseAmount(abc) should fail")
	}
}

func TestPaymentUIDAndPhone(t *testing.T) {
	if got := paymentUID(paymentProviderYo, "TX-1"); got != "YO:TX-1" {
		t.Fatalf("paymentUID = %q", got)
	}
	if got := cleanPhoneDigits("0701234567"); got != "256701234567" {
		t.Fatalf("cleanPhoneDigits = %q", got)
	}
}
