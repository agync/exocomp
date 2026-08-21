package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// maxStateBytes caps one pushed state document. Tofu state is JSON describing
// resources, not payload data; a body this large is a bug or an attack.
const maxStateBytes = 64 << 20

// maxLockBytes caps a lock document, which is a handful of short fields.
const maxLockBytes = 64 << 10

// lockInfo is OpenTofu's lock document, field for field. The names are the
// wire contract: tofu decodes the body of a 423 response into this shape to
// tell the operator who holds the lock.
type lockInfo struct {
	ID        string    `json:"ID"`
	Operation string    `json:"Operation"`
	Info      string    `json:"Info"`
	Who       string    `json:"Who"`
	Version   string    `json:"Version"`
	Created   time.Time `json:"Created"`
	Path      string    `json:"Path"`
}

// stateHeader is the plaintext preamble tofu leaves outside the encrypted
// payload. Reading it is how the store knows a version's serial and lineage
// without holding the state passphrase.
type stateHeader struct {
	Serial  int64  `json:"serial"`
	Lineage string `json:"lineage"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("workspace")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r, name) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="exocomp"`)
		httpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	ctx := r.Context()
	ws, err := s.store.WorkspaceByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such workspace")
		return
	}
	if err != nil {
		s.fail(w, "lookup workspace", err)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getState(w, r, ws)
	case http.MethodPost:
		s.postState(w, r, ws)
	case "LOCK":
		s.lockState(w, r, ws)
	case "UNLOCK":
		s.unlockState(w, r, ws)
	default:
		w.Header().Set("Allow", "GET, POST, LOCK, UNLOCK")
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// getState returns the newest version verbatim. A workspace that has never
// been applied returns 404, which tofu reads as "empty state".
func (s *Server) getState(w http.ResponseWriter, r *http.Request, ws store.Workspace) {
	latest, err := s.store.LatestTFState(r.Context(), ws.ID)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no state")
		return
	}
	if err != nil {
		s.fail(w, "read state", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(latest.Ciphertext); err != nil {
		s.log.Error("write state response", "workspace", ws.Name, "err", err)
	}
}

// postState appends a version, refusing a writer that does not hold the lock.
func (s *Server) postState(w http.ResponseWriter, r *http.Request, ws store.Workspace) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStateBytes))
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, "state body too large")
		return
	}
	if len(body) == 0 {
		httpError(w, http.StatusBadRequest, "empty state body")
		return
	}

	// The body is opaque: when tofu encrypts state, only serial and lineage
	// stay readable, and an unreadable header is stored rather than rejected.
	var head stateHeader
	_ = json.Unmarshal(body, &head)

	version, err := s.store.AppendTFState(r.Context(), store.NewTFState{
		WorkspaceID: ws.ID,
		LockID:      r.URL.Query().Get("ID"),
		SerialHint:  head.Serial,
		Lineage:     head.Lineage,
		JobID:       s.jobFor(ws.Name),
		Ciphertext:  body,
	})
	if errors.Is(err, store.ErrLocked) {
		s.locked(w, r, ws)
		return
	}
	if err != nil {
		s.fail(w, "append state", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]int64{"serial": version.Serial})
}

// lockState takes the workspace lock, answering 423 plus the holder when it is
// already taken — the response tofu turns into "Lock Info" on the operator's
// terminal.
func (s *Server) lockState(w http.ResponseWriter, r *http.Request, ws store.Workspace) {
	var info lockInfo
	if !decodeLock(w, r, &info) {
		return
	}

	_, err := s.store.AcquireTFLock(r.Context(), store.TFLock{
		WorkspaceID: ws.ID,
		LockID:      info.ID,
		Who:         info.Who,
		Operation:   info.Operation,
		CreatedAt:   info.Created,
		JobID:       s.jobFor(ws.Name),
	})
	if errors.Is(err, store.ErrLocked) {
		s.locked(w, r, ws)
		return
	}
	if err != nil {
		s.fail(w, "acquire lock", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// unlockState releases the lock when the caller is its holder. Unlocking a
// free workspace succeeds: a retried unlock must not fail a finished job.
func (s *Server) unlockState(w http.ResponseWriter, r *http.Request, ws store.Workspace) {
	var info lockInfo
	if !decodeLock(w, r, &info) {
		return
	}

	err := s.store.ReleaseTFLock(r.Context(), ws.ID, info.ID)
	if errors.Is(err, store.ErrLocked) {
		s.locked(w, r, ws)
		return
	}
	if err != nil {
		s.fail(w, "release lock", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// locked answers a lock conflict with the holder's document.
func (s *Server) locked(w http.ResponseWriter, r *http.Request, ws store.Workspace) {
	holder, err := s.store.GetTFLock(r.Context(), ws.ID)
	if err != nil {
		// The holder released between the refusal and this read. Say so
		// plainly rather than inventing a lock document.
		httpError(w, http.StatusConflict, "state lock contended")
		return
	}
	writeJSON(w, http.StatusLocked, lockInfo{
		ID:        holder.LockID,
		Operation: holder.Operation,
		Who:       holder.Who,
		Created:   holder.CreatedAt,
		Path:      ws.Name,
	})
}

func decodeLock(w http.ResponseWriter, r *http.Request, info *lockInfo) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLockBytes))
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, "lock body too large")
		return false
	}
	if err := json.Unmarshal(body, info); err != nil {
		httpError(w, http.StatusBadRequest, "malformed lock body")
		return false
	}
	if info.ID == "" {
		httpError(w, http.StatusBadRequest, "lock body has no ID")
		return false
	}
	return true
}

// authorized compares the presented bearer with the workspace's derived token
// in constant time.
func (s *Server) authorized(r *http.Request, workspace string) bool {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	want := WorkspaceToken(s.tokens, workspace)
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// fail logs the cause and tells the client only that the server broke: a state
// backend must not narrate its database to a job subprocess.
func (s *Server) fail(w http.ResponseWriter, op string, err error) {
	s.log.Error("state backend: "+op, "err", err)
	httpError(w, http.StatusInternalServerError, "state backend error")
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	blob, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(blob)
}
