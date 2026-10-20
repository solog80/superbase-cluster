package main

import (
	"strings"
	"testing"
)

func TestParseVideoLogCSV_HeaderAndOverlay(t *testing.T) {
	body := "played_at,channel_id,monitor,client,title,duration_seconds,filename,input_number,input_key,log_hash\n" +
		"2026-10-16 06:16:28,401,Program,MTN,MTN Boda Code Lug,30,D:\\SaltTV\\Ads\\x.mp4,12,abc,\n" +
		"2026-10-16 06:31:22,401,Overlay 2,,EBENEZER HARDWARE,30,D:\\SaltTV\\Ads\\y.mp4,19,def,zzz\n"

	recs, err := parseVideoLogCSV(strings.NewReader(body), 401)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].Client != "MTN" || recs[0].Title != "MTN Boda Code Lug" {
		t.Fatalf("bad first record: %+v", recs[0])
	}
	if recs[1].Monitor != "Overlay 2" || recs[1].MonitorNumber != 2 {
		t.Fatalf("overlay not parsed: %+v", recs[1])
	}
	if recs[1].LogHash != "zzz" {
		t.Fatalf("log_hash not preserved: %q", recs[1].LogHash)
	}
}

func TestParseVideoLogCSV_Headerless(t *testing.T) {
	body := "2026-10-17 07:00:00,401,Program,Airtel,AIRTEL PROMO,15,D:\\Ads\\a.mp4,3,aaa,\n"
	recs, err := parseVideoLogCSV(strings.NewReader(body), 401)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].DurationSeconds != 15 || recs[0].InputNumber != 3 {
		t.Fatalf("bad record: %+v", recs[0])
	}
}
