package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	webassets "github.com/driftywinds/interim/web"
)

func (s *Server) openAPISpec() (string, error) {
	raw, err := webassets.FS.ReadFile("openapi.yaml")
	if err != nil {
		return "", err
	}
	spec := strings.ReplaceAll(string(raw), "__BASE_URL__", s.cfg.PublicBaseURL)
	return spec, nil
}

func (s *Server) handleOpenAPIYAML(w http.ResponseWriter, r *http.Request) {
	spec, err := s.openAPISpec()
	if err != nil {
		s.logger.Error("read openapi yaml", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(spec))
}

func (s *Server) handleOpenAPIJSON(w http.ResponseWriter, r *http.Request) {
	spec, err := s.openAPISpec()
	if err != nil {
		s.logger.Error("read openapi yaml", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var value any
	if err := yaml.Unmarshal([]byte(spec), &value); err != nil {
		s.logger.Error("decode openapi yaml", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		s.logger.Error("encode openapi json", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(data)
}
