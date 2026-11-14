package report

import (
	"encoding/csv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// mixedAssets covers one row per asset type with type-specific fields populated.
func mixedAssets() []*db.Asset {
	return []*db.Asset{
		{ID: 1, Type: "root_domain", Domain: "example.com", RootDomain: "example.com", TaskIDs: []int64{10}, LastSeen: "2026-10-13 10:00:00", TaskSource: "recon"},
		{ID: 2, Type: "subdomain", Domain: "api.example.com", RootDomain: "example.com", RecordType: "CNAME", RecordValue: []string{"elb.amazonaws.com"}, TaskIDs: []int64{10, 11}, LastSeen: "2026-10-13 10:01:00", TaskSource: "recon"},
		{ID: 3, Type: "ip", IP: "203.0.113.10", BoundDomains: []string{"example.com"}, OpenPorts: []map[string]any{{"port": 443, "service": "https"}}, TaskIDs: []int64{11}, LastSeen: "2026-10-13 10:02:00", TaskSource: "scan"},
		{ID: 4, Type: "service", URL: "https://api.example.com", ServiceType: "web", StatusCode: new(200), PageTitle: "API", Technologies: []string{"nginx"}, TaskIDs: []int64{11}, LastSeen: "2026-10-13 10:03:00", TaskSource: "scan"},
		{ID: 5, Type: "endpoint", URL: "https://api.example.com/v1/users", Method: "GET", Params: []map[string]any{{"name": "page", "in": "query"}}, TaskIDs: []int64{12}, LastSeen: "2026-10-13 10:04:00", TaskSource: "fuzz"},
		{ID: 6, Type: "app", BundleID: "com.example.app", AppName: "Example", Category: "tools", TaskIDs: []int64{12}, LastSeen: "2026-10-13 10:05:00", TaskSource: "mobile"},
	}
}

func TestAssetsCSVGolden(t *testing.T) {
	got := string(AssetsCSV(mixedAssets()))
	want := "\xEF\xBB\xBF" + strings.Join([]string{
		"ID,Type,Value,Root domain,Task IDs,Last seen,Source,Details",
		`1,root_domain,example.com,example.com,10,2026-10-13 10:00:00,recon,`,
		`2,subdomain,api.example.com,example.com,"10,11",2026-10-13 10:01:00,recon,"{""record_type"":""CNAME"",""record_value"":[""elb.amazonaws.com""]}"`,
		`3,ip,203.0.113.10,,11,2026-10-13 10:02:00,scan,"{""bound_domains"":[""example.com""],""open_ports"":[{""port"":443,""service"":""https""}]}"`,
		`4,service,https://api.example.com,,11,2026-10-13 10:03:00,scan,"{""page_title"":""API"",""service_type"":""web"",""status_code"":200,""technologies"":[""nginx""]}"`,
		`5,endpoint,https://api.example.com/v1/users,,12,2026-10-13 10:04:00,fuzz,"{""method"":""GET"",""params"":[{""in"":""query"",""name"":""page""}]}"`,
		`6,app,com.example.app,,12,2026-10-13 10:05:00,mobile,"{""app_name"":""Example"",""category"":""tools""}"`,
	}, "\n") + "\n"
	if got != want {
		t.Fatalf("CSV mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestAssetsCSVEmptyHeadersOnly(t *testing.T) {
	got := string(AssetsCSV(nil))
	if got != "\xEF\xBB\xBFID,Type,Value,Root domain,Task IDs,Last seen,Source,Details\n" {
		t.Fatalf("empty export must be headers-only, got %q", got)
	}
}

func TestAssetValueFallbackChain(t *testing.T) {
	cases := []struct {
		a    db.Asset
		want string
	}{
		{db.Asset{Type: "root_domain", Domain: "a.com", IP: "1.2.3.4"}, "a.com"},
		{db.Asset{Type: "ip", IP: "1.2.3.4", URL: "http://x"}, "1.2.3.4"},
		{db.Asset{Type: "service", URL: "http://x"}, "http://x"},
		{db.Asset{Type: "service", Domain: "x", IP: "1.2.3.4", URL: "http://x"}, "http://x"},
		{db.Asset{Type: "endpoint", Domain: "x", IP: "1.2.3.4", URL: "http://x/users"}, "http://x/users"},
		{db.Asset{Type: "service", Domain: "x"}, "x"},
		{db.Asset{Type: "endpoint", IP: "1.2.3.4"}, "1.2.3.4"},
		{db.Asset{Type: "app", BundleID: "com.x"}, "com.x"},
		{db.Asset{Type: "unknown"}, ""},
	}
	for _, c := range cases {
		if got := assetValue(&c.a); got != c.want {
			t.Fatalf("assetValue(%+v) = %q, want %q", c.a, got, c.want)
		}
	}
}

func TestAssetDetailsJSONOmitsEmpty(t *testing.T) {
	if got := assetDetailsJSON(&db.Asset{Type: "root_domain", Domain: "a.com"}); got != "" {
		t.Fatalf("bare asset should have empty details, got %q", got)
	}
	got := assetDetailsJSON(&db.Asset{Type: "service", URL: "http://x", StatusCode: new(404)})
	if got != `{"status_code":404}` {
		t.Fatalf("details = %q", got)
	}
}

func TestAssetsCSVPreservesStoredHTTPURLs(t *testing.T) {
	assets := []*db.Asset{
		{ID: 1, Type: "service", Domain: "api.example.com", IP: "203.0.113.10", URL: "https://api.example.com", ServiceType: "http"},
		{ID: 2, Type: "endpoint", Domain: "api.example.com", IP: "203.0.113.10", URL: "https://api.example.com/v1/users", Method: "GET"},
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(AssetsCSV(assets)), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(assets)+1 {
		t.Fatalf("CSV rows = %d, want %d", len(rows), len(assets)+1)
	}
	for i, asset := range assets {
		if got := rows[i+1][2]; got != asset.URL {
			t.Errorf("%s Value = %q, want full URL %q", asset.Type, got, asset.URL)
		}
	}
}
