package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// Assets export renders CSV. The server serializes JSON directly from db.Asset.

// assetValue prefers the type's primary value, then falls back to populated fields.
func assetValue(a *db.Asset) string {
	var primary string
	switch a.Type {
	case "root_domain", "subdomain":
		primary = a.Domain
	case "ip":
		primary = a.IP
	case "service", "endpoint":
		primary = a.URL
	case "app":
		primary = a.BundleID
	}
	for _, v := range []string{primary, a.Domain, a.IP, a.URL, a.BundleID} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// assetDetailsJSON packs the type-specific fields into a compact JSON object.
// Empty fields are omitted so the column stays readable.
func assetDetailsJSON(a *db.Asset) string {
	d := map[string]any{}
	if len(a.BoundDomains) > 0 {
		d["bound_domains"] = a.BoundDomains
	}
	if len(a.OpenPorts) > 0 {
		d["open_ports"] = a.OpenPorts
	}
	if a.RecordType != "" {
		d["record_type"] = a.RecordType
	}
	if len(a.RecordValue) > 0 {
		d["record_value"] = a.RecordValue
	}
	if a.AppName != "" {
		d["app_name"] = a.AppName
	}
	if a.Category != "" {
		d["category"] = a.Category
	}
	if a.ServiceType != "" {
		d["service_type"] = a.ServiceType
	}
	if a.ServiceName != "" {
		d["service_name"] = a.ServiceName
	}
	if a.StatusCode != nil {
		d["status_code"] = *a.StatusCode
	}
	if a.ContentLength != nil {
		d["content_length"] = *a.ContentLength
	}
	if a.PageTitle != "" {
		d["page_title"] = a.PageTitle
	}
	if len(a.Technologies) > 0 {
		d["technologies"] = a.Technologies
	}
	if len(a.Auth) > 0 {
		d["auth"] = a.Auth
	}
	if a.Method != "" {
		d["method"] = a.Method
	}
	if len(a.Params) > 0 {
		d["params"] = a.Params
	}
	if a.Port != nil {
		d["port"] = *a.Port
	}
	if a.ICP != "" {
		d["icp"] = a.ICP
	}
	if len(d) == 0 {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// AssetsCSV renders one row per asset with a UTF-8 BOM for Excel.
// Fixed identity columns plus a compact-JSON details column keep the CSV flat.
// Empty input yields a headers-only file, matching the findings export.
func AssetsCSV(assets []*db.Asset) []byte {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{
		"ID",
		"Type",
		"Value",
		"Root domain",
		"Task IDs",
		"Last seen",
		"Source",
		"Details",
	})
	for _, a := range assets {
		if a == nil {
			continue
		}
		taskIDs := make([]string, len(a.TaskIDs))
		for i, id := range a.TaskIDs {
			taskIDs[i] = strconv.FormatInt(id, 10)
		}
		_ = w.Write([]string{
			fmt.Sprintf("%d", a.ID),
			a.Type,
			assetValue(a),
			a.RootDomain,
			strings.Join(taskIDs, ","),
			a.LastSeen,
			a.TaskSource,
			assetDetailsJSON(a),
		})
	}
	w.Flush()
	return buf.Bytes()
}
