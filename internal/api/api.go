// Package api is Tether's small control plane: health, tab listing and
// lifecycle, JavaScript evaluation, and the local CA certificate. It lives
// under /tether/v1/ and shares the session core with every face.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/httpx"
	"github.com/adeelahmad/tether/internal/session"
	"github.com/adeelahmad/tether/internal/version"
)

// Prefix is where the control API is mounted.
const Prefix = "/tether/v1/"

// API is the control-plane handler.
type API struct {
	core  *core.Core
	caPEM []byte
	mux   *http.ServeMux
	faces []string
}

// New returns the control API. caPEM may be nil.
func New(c *core.Core, caPEM []byte, faces []string) *API {
	a := &API{core: c, caPEM: caPEM, mux: http.NewServeMux(), faces: faces}
	a.mux.HandleFunc("GET /tether/v1/health", a.health)
	a.mux.HandleFunc("GET /tether/v1/ca.pem", a.ca)
	a.mux.HandleFunc("GET /tether/v1/tabs", a.listTabs)
	a.mux.HandleFunc("POST /tether/v1/tabs", a.createTab)
	a.mux.HandleFunc("GET /tether/v1/tabs/{id}", a.getTab)
	a.mux.HandleFunc("DELETE /tether/v1/tabs/{id}", a.closeTab)
	a.mux.HandleFunc("POST /tether/v1/tabs/{id}/eval", a.eval)
	a.mux.HandleFunc("GET /tether/v1/tabs/{id}/logs", a.logs)
	a.mux.HandleFunc("/tether/v1/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return a
}

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := map[string]any{
		"ok":      true,
		"version": version.Version,
		"faces":   a.faces,
		"tabs":    len(a.core.Sessions.List()),
	}
	product, err := a.core.Browser.Version(ctx)
	if err != nil {
		out["ok"] = false
		out["error"] = err.Error()
		httpx.WriteJSON(w, http.StatusServiceUnavailable, out)
		return
	}
	out["browser"] = product
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (a *API) ca(w http.ResponseWriter, _ *http.Request) {
	if a.caPEM == nil {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "HTTPS interception is disabled")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="tether-ca.pem"`)
	_, _ = w.Write(a.caPEM)
}

type tabView struct {
	session.Info
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

func (a *API) view(ctx context.Context, s *session.Session) tabView {
	v := tabView{Info: s.Info()}
	if info, err := s.Tab.Info(ctx); err == nil {
		v.URL, v.Title = info.URL, info.Title
	}
	return v
}

func (a *API) listTabs(w http.ResponseWriter, r *http.Request) {
	infos := a.core.Sessions.List()
	out := make([]tabView, 0, len(infos))
	for _, i := range infos {
		if s, err := a.core.Sessions.Get(r.Context(), i.ID); err == nil {
			out = append(out, a.view(r.Context(), s))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tabs": out})
}

func (a *API) createTab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Wait string `json:"wait"`
	}
	if err := decode(r, &req); err != nil {
		a.fail(w, err)
		return
	}
	wait, err := cdp.ParseWait(req.Wait)
	if err != nil {
		a.fail(w, errors.Join(core.ErrBadRequest, err))
		return
	}
	s, err := a.core.Sessions.Create(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	if req.URL != "" {
		unlock, err := s.Lock(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		_, err = a.core.Navigate(r.Context(), s, req.URL, wait)
		unlock()
		if err != nil {
			_ = a.core.Sessions.Close(context.WithoutCancel(r.Context()), s.ID, false)
			a.fail(w, err)
			return
		}
	}
	w.Header().Set("X-Tether-Tab-ID", s.ID)
	httpx.WriteJSON(w, http.StatusCreated, a.view(r.Context(), s))
}

func (a *API) getTab(w http.ResponseWriter, r *http.Request) {
	s, err := a.core.Sessions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a.view(r.Context(), s))
}

func (a *API) closeTab(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"
	if err := a.core.Sessions.Close(r.Context(), r.PathValue("id"), force); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) eval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Expression   string `json:"expression"`
		AwaitPromise *bool  `json:"awaitPromise"`
	}
	if err := decode(r, &req); err != nil {
		a.fail(w, err)
		return
	}
	if req.Expression == "" {
		a.fail(w, errors.Join(core.ErrBadRequest, errors.New("expression is required")))
		return
	}
	s, err := a.core.Sessions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.core.Timeout)
	defer cancel()
	await := req.AwaitPromise == nil || *req.AwaitPromise
	v, err := a.core.Eval(ctx, s, req.Expression, await)
	if err != nil {
		a.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"value": v})
}

// logs returns the tab's buffered console and browser log entries. Poll
// with ?after=<next from the previous response> to read only new ones.
func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			a.fail(w, errors.Join(core.ErrBadRequest, errors.New("after must be a non-negative integer")))
			return
		}
		after = n
	}
	level, err := core.ParseLevel(q.Get("level"))
	if err != nil {
		a.fail(w, err)
		return
	}
	s, err := a.core.Sessions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	entries, next := a.core.Logs(s, after, level)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": next})
}

func (a *API) fail(w http.ResponseWriter, err error) {
	status, code := core.Classify(err)
	httpx.WriteError(w, status, code, err.Error())
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return errors.Join(core.ErrBadRequest, err)
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return errors.Join(core.ErrBadRequest, err)
	}
	return nil
}
