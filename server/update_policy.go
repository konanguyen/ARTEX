package server

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

const updateLocalOnlyMessage = "Update checks and downloads are unavailable while local_llm_only is enabled"

// Guard all network-producing update operations, including HTTP redirects and
// downloads that started before the policy changed. Existing staging cleanup
// consumes read errors and the update hub publishes the resulting failure.
func (s *Server) updatePolicyError() error {
	if strict, _ := s.effectiveLLMPolicy(); strict {
		return errors.New(updateLocalOnlyMessage)
	}
	return nil
}
func (s *Server) guardedUpdateClient(client *http.Client) *http.Client {
	copyClient := *client
	transport := copyClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copyClient.Transport = &updatePolicyTransport{s: s, inner: transport}
	redirects := copyClient.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := s.updatePolicyError(); err != nil {
			return err
		}
		if redirects != nil {
			return redirects(req, via)
		}
		if len(via) >= 10 {
			return errors.New("Too many redirects")
		}
		return nil
	}
	return &copyClient
}

type updatePolicyTransport struct {
	s     *Server
	inner http.RoundTripper
}

func (t *updatePolicyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.s.updatePolicyError(); err != nil {
		return nil, err
	}
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if err := t.s.updatePolicyError(); err != nil {
		resp.Body.Close()
		return nil, err
	}
	resp.Body = &updatePolicyBody{ReadCloser: resp.Body, s: t.s}
	return resp, nil
}

type updatePolicyBody struct {
	io.ReadCloser
	s *Server
}

func (b *updatePolicyBody) Read(p []byte) (int, error) {
	if err := b.s.updatePolicyError(); err != nil {
		return 0, err
	}
	n, err := b.ReadCloser.Read(p)
	// If policy changed while blocked in a read, discard those bytes before the
	// downloader can write them or interpret EOF as successful completion.
	if policyErr := b.s.updatePolicyError(); policyErr != nil {
		return 0, policyErr
	}
	return n, err
}

func (s *Server) guardedUpdateCheck(w http.ResponseWriter, r *http.Request) {
	boot := bootUpdateState()
	current := BuildVersion
	out := map[string]any{
		"current":     current,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	if err := s.updatePolicyError(); err != nil {
		out["local_llm_only"], out["error"] = true, err.Error()
		writeJSON(w, 200, out)
		return
	}

	force := r.URL.Query().Get("force") != ""
	client := s.guardedUpdateClient(selfupdate.NewClient(s.m.GlobalProxy()))
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	if err := s.updatePolicyError(); err != nil {
		out["local_llm_only"], out["error"] = true, err.Error()
		writeJSON(w, 200, out)
		return
	}
	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		out["reason"] = fmt.Sprintf("Current version %q is not an official release; one-click update is disabled", current)
	} else if cmp >= 0 {
		out["reason"] = fmt.Sprintf("Already up to date: %s", current)
	}

	writeJSON(w, 200, out)
}

func (s *Server) guardedUpdateApply(w http.ResponseWriter, r *http.Request) {
	if err := s.updatePolicyError(); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "local_llm_only": true, "error": err.Error()})
		return
	}
	current := BuildVersion

	client := s.guardedUpdateClient(selfupdate.NewClient(s.m.GlobalProxy()))
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if err := s.updatePolicyError(); err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("Current version %q is not an official release; one-click update is disabled", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("Already up to date: %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "Update is already in progress")
		return
	}

	go func() {
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] update failed: %v", err)
			return
		}
		log.Printf("[update] %s → %s staged, exiting to complete upgrade", current, rel.TagName)
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}
