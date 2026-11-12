package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func (s *Server) listApplicationSecrets(w http.ResponseWriter, r *http.Request) {
	env, ok := s.localEnv(w, r)
	if !ok {
		return
	}
	out, err := s.St.ApplicationSecrets(r.Context(), env.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) patchApplicationSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	env, ok := s.localEnv(w, r)
	if !ok {
		return
	}
	var in store.ApplicationSecretPatch
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		unprocessable(w, "невалидный JSON")
		return
	}
	if err := s.St.PatchApplicationSecrets(r.Context(), env.ID, in, s.Secrets, currentUser(r).ID); err != nil {
		if errors.Is(err, secretbox.ErrNoKey) {
			writeErrCode(w, http.StatusServiceUnavailable, "no_secret_key", "ключ шифрования секретов не настроен")
			return
		}
		if writeMonitorErr(w, err) {
			return
		}
		writeErr(w, err)
		return
	}
	s.listApplicationSecrets(w, r)
}
