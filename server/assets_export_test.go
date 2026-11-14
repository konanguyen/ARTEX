package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// exportHTTPFixture inserts a company-attributed asset set and returns the
// handler under test plus the inserted ids and the company id.
func exportHTTPFixture(t *testing.T) (s *Server, ids []int64, companyID int64) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	stamp := time.Now().UnixNano() % 1000000
	companyID, _, _, _, _, err = m.pg.Companies().CreateCompanyWithScope(
		fmt.Sprintf("Export HTTP Co %d", stamp), "",
		[]db.ScopeInput{{Kind: "domain", Value: "export-http.example"}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.Companies().DeleteCompany(companyID) })

	taskID := int64(800000 + stamp%100000)
	root, err := m.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: "export-http.example", TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := m.Assets().UpsertSubdomain(db.UpsertSubdomainReq{Domain: "shop.export-http.example", RecordType: "A", RecordValue: []string{"198.51.100.9"}, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	ids = []int64{root, sub}
	t.Cleanup(func() {
		for _, id := range ids {
			m.pg.Exec(`DELETE FROM assets WHERE id = $1`, id)
		}
	})
	if err := m.pg.Companies().RecomputeAttribution(); err != nil {
		t.Fatal(err)
	}
	return &Server{m: m}, ids, companyID
}

func TestAssetsExportSelected(t *testing.T) {
	s, ids, _ := exportHTTPFixture(t)
	for _, format := range []string{"", "csv"} {
		target := fmt.Sprintf("/api/assets/export?scope=selected&ids=%d,%d", ids[0], ids[1])
		if format != "" {
			target += "&format=" + format
		}
		rec := httptest.NewRecorder()
		s.assetsExport(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("format=%q status=%d body=%s", format, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
			t.Fatalf("format=%q content-type=%q", format, ct)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Fatalf("format=%q content-disposition=%q", format, cd)
		}
		body := rec.Body.String()
		// header + exactly two data rows
		if lines := countLines(body); lines != 3 {
			t.Fatalf("format=%q expected 3 lines (header+2), got %d: %q", format, lines, body)
		}
	}
}

func TestAssetsExportSelectedEmptyIDs(t *testing.T) {
	s, _, _ := exportHTTPFixture(t)
	rec := httptest.NewRecorder()
	s.assetsExport(rec, httptest.NewRequest(http.MethodGet, "/api/assets/export?scope=selected", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty selected must be 400, got %d", rec.Code)
	}
}

func TestAssetsExportSelectedBadID(t *testing.T) {
	s, _, _ := exportHTTPFixture(t)
	rec := httptest.NewRecorder()
	s.assetsExport(rec, httptest.NewRequest(http.MethodGet, "/api/assets/export?scope=selected&ids=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id must be 400, got %d", rec.Code)
	}
}

func TestAssetsExportBadScope(t *testing.T) {
	s, _, _ := exportHTTPFixture(t)
	rec := httptest.NewRecorder()
	s.assetsExport(rec, httptest.NewRequest(http.MethodGet, "/api/assets/export?scope=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scope must be 400, got %d", rec.Code)
	}
}

func TestAssetsExportFilteredByCompany(t *testing.T) {
	s, ids, companyID := exportHTTPFixture(t)
	rec := httptest.NewRecorder()
	s.assetsExport(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/assets/export?scope=filtered&company_id=%d&format=json", companyID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []db.Asset
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("company export = %d rows, want %d", len(got), len(ids))
	}
	for _, a := range got {
		if a.CompanyID == nil || *a.CompanyID != companyID {
			t.Fatalf("asset %d not attributed to company %d", a.ID, companyID)
		}
	}
}

func TestAssetsExportFilteredBadDSL(t *testing.T) {
	s, _, _ := exportHTTPFixture(t)
	rec := httptest.NewRecorder()
	s.assetsExport(rec, httptest.NewRequest(http.MethodGet,
		"/api/assets/export?scope=filtered&dsl=nope_field+%3D+%22x%22", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid dsl must be 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAssetsExportFilteredEmptyHeadersOnly(t *testing.T) {
	s, _, _ := exportHTTPFixture(t)
	rec := httptest.NewRecorder()
	s.assetsExport(rec, httptest.NewRequest(http.MethodGet,
		"/api/assets/export?scope=filtered&task_id=999999999", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if lines := countLines(rec.Body.String()); lines != 1 {
		t.Fatalf("empty filtered export must be headers-only, got %d lines", lines)
	}
}

func countLines(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}

func TestAssetsExportAllIgnoresFilters(t *testing.T) {
	s, ids, _ := exportHTTPFixture(t)
	for _, filters := range []string{
		"type=subdomain",
		"dsl=" + url.QueryEscape("domain=missing-export.example"),
		"dsl=" + url.QueryEscape(`nope_field = "x"`),
		"task_id=999999999",
		"company_id=999999999",
		"ids=" + fmt.Sprint(ids[0]),
		"type=subdomain&dsl=" + url.QueryEscape(`nope_field = "x"`) + "&task_id=999999999&company_id=999999999&ids=bad",
	} {
		t.Run(filters, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.assetsExport(rec, httptest.NewRequest(http.MethodGet, "/api/assets/export?scope=all&format=json&"+filters, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("all with irrelevant filters: status=%d body=%s", rec.Code, rec.Body.String())
			}
			var assets []db.Asset
			if err := json.Unmarshal(rec.Body.Bytes(), &assets); err != nil {
				t.Fatal(err)
			}
			got := map[int64]bool{}
			for _, asset := range assets {
				got[asset.ID] = true
			}
			for _, id := range ids {
				if !got[id] {
					t.Errorf("all export omitted fixture asset %d", id)
				}
			}
		})
	}
}

func TestAssetsExportFilteredMatchesListDefault(t *testing.T) {
	s, _, _ := exportHTTPFixture(t)
	listRec := httptest.NewRecorder()
	s.listAssets(listRec, httptest.NewRequest(http.MethodGet, "/api/assets?limit=200", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Total  int        `json:"total"`
		Assets []db.Asset `json:"assets"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "filtered"} {
		t.Run("scope="+scope, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.assetsExport(rec, httptest.NewRequest(http.MethodGet, "/api/assets/export?format=json&scope="+scope, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
			}
			var assets []db.Asset
			if err := json.Unmarshal(rec.Body.Bytes(), &assets); err != nil {
				t.Fatal(err)
			}
			if len(assets) != list.Total {
				t.Errorf("default export rows=%d, list full match total=%d", len(assets), list.Total)
			}
			for _, asset := range assets {
				if asset.Type != "root_domain" {
					t.Errorf("default export includes asset %d type=%s", asset.ID, asset.Type)
				}
			}
		})
	}
}

func TestAssetsExportFilteredUntypedScopesPreserved(t *testing.T) {
	s, ids, companyID := exportHTTPFixture(t)
	fixture, err := s.assetStore().GetByIDs(ids)
	if err != nil {
		t.Fatal(err)
	}
	taskID := fixture[0].TaskIDs[0]
	for _, filters := range []string{
		"dsl=" + url.QueryEscape("domain=export-http.example"),
		fmt.Sprintf("company_id=%d", companyID),
		fmt.Sprintf("task_id=%d", taskID),
	} {
		t.Run(filters, func(t *testing.T) {
			listRec := httptest.NewRecorder()
			s.listAssets(listRec, httptest.NewRequest(http.MethodGet, "/api/assets?limit=200&"+filters, nil))
			if listRec.Code != http.StatusOK {
				t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
			}
			var list struct {
				Total int `json:"total"`
			}
			if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			s.assetsExport(rec, httptest.NewRequest(http.MethodGet, "/api/assets/export?scope=filtered&format=json&"+filters, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
			}
			var assets []db.Asset
			if err := json.Unmarshal(rec.Body.Bytes(), &assets); err != nil {
				t.Fatal(err)
			}
			if len(assets) != list.Total {
				t.Errorf("filtered export rows=%d, list full match total=%d", len(assets), list.Total)
			}
			got := map[int64]bool{}
			for _, asset := range assets {
				got[asset.ID] = true
			}
			for _, id := range ids {
				if !got[id] {
					t.Errorf("untyped filtered export omitted fixture asset %d", id)
				}
			}
		})
	}
}
