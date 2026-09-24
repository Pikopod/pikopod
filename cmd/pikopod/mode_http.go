package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/pikopod/pikopod/internal/mode"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario/resolve"
)

type modeRequest struct {
	Name  string            `json:"name"`
	Seed  string            `json:"seed,omitempty"`
	Binds map[string]string `json:"bind,omitempty"`
}

func (s *sandboxServer) serveMode(w http.ResponseWriter, r *http.Request, name string, engine *sandbox.Engine) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		spec := s.modes[name]
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"mode": spec})

	case http.MethodPost:
		var req modeRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Name == "" {
			writeSandboxJSONError(w, http.StatusBadRequest, "body must be {\"name\": \"<archetype or pack>\"}")
			return
		}
		_, def, err := loadSandboxDef(s.cfg, name)
		if err != nil {
			writeSandboxJSONError(w, http.StatusNotFound, "unknown sandbox "+name)
			return
		}
		parsed, info, err := resolve.ResolveDetailed(def, req.Name, resolve.Options{
			PackDirs:      packDirs(s.cfg),
			BindOverrides: req.Binds,
		})
		if err != nil {
			writeSandboxJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		source := "archetype or pack " + req.Name
		if note := info.Note(); note != "" {
			source += "; " + note
		}
		spec, err := mode.Compile(req.Name, source, parsed)
		if err != nil {
			writeSandboxJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.mu.Lock()
		prev := s.modes[name]
		if prev != nil {
			spec.Revision = prev.Revision + 1
		}
		s.mu.Unlock()
		if err := mode.Apply(engine, spec); err != nil {
			writeSandboxJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.mu.Lock()
		if s.modes == nil {
			s.modes = map[string]*mode.Spec{}
		}
		s.modes[name] = spec
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"mode": spec})

	case http.MethodDelete:
		cleared := engine.ClearFaults("", "")
		s.mu.Lock()
		delete(s.modes, name)
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"cleared": cleared})

	default:
		writeSandboxJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}
