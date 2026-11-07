package server

import (
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5"
)

func (s *Server) mountCooldownProfiles(r chi.Router, proxy *handler.Proxy) error {
	if err := s.cfg.Cooldown.ValidateProfiles(); err != nil {
		return err
	}
	for name, cfg := range s.cfg.Cooldown.Profiles {
		policy, err := cfg.Policy()
		if err != nil {
			return err
		}
		prefix := "/cooldown/" + name
		profile := chi.NewRouter()
		s.mountProtocolHandlersAt(profile, proxy.WithCooldown(policy, cfg.Shortest()), strings.TrimRight(s.cfg.BaseURL, "/")+prefix)
		r.Mount(prefix, http.StripPrefix(prefix, profile))
	}
	return nil
}
