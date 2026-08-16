package discussion

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// Handler provides REST endpoints for discussion rooms.
type Handler struct {
	store *Store
}

// NewHandler creates an HTTP handler group for discussion rooms.
func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

// Register wires routes onto a mux subrouter.
// All routes are prefixed with /api/projects/{projectId}/rooms.
func (h *Handler) Register(r *mux.Router) {
	r.HandleFunc("/rooms", h.listRooms).Methods("GET")
	r.HandleFunc("/rooms", h.createRoom).Methods("POST")
	// Literal /rooms/* routes MUST register before the /rooms/{roomId} var route:
	// gorilla/mux matches in registration order, so {roomId} would otherwise
	// capture "search"/"memory-index" and their handlers would be unreachable.
	r.HandleFunc("/rooms/search", h.searchMessages).Methods("GET")
	r.HandleFunc("/rooms/memory-index", h.memoryIndex).Methods("GET")
	r.HandleFunc("/rooms/{roomId}", h.getRoom).Methods("GET")
	r.HandleFunc("/rooms/{roomId}", h.archiveRoom).Methods("DELETE")
	r.HandleFunc("/rooms/{roomId}/messages", h.getMessages).Methods("GET")
	r.HandleFunc("/rooms/{roomId}/messages", h.postMessage).Methods("POST")
	r.HandleFunc("/rooms/{roomId}/messages/{messageId}", h.editMessage).Methods("PATCH")
	r.HandleFunc("/rooms/{roomId}/messages/{messageId}", h.deleteMessage).Methods("DELETE")
}

// ============================================================================
// Helpers
// ============================================================================

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func parseUUID(r *http.Request, key string) (uuid.UUID, bool) {
	v := mux.Vars(r)[key]
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func queryInt(r *http.Request, key string, def int) int {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ============================================================================
// Room endpoints
// ============================================================================

func (h *Handler) listRooms(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseUUID(r, "projectId")
	if !ok {
		writeError(w, 400, "invalid projectId")
		return
	}
	rooms, err := h.store.ListProjectRooms(r.Context(), projectID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rooms)
}

func (h *Handler) createRoom(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseUUID(r, "projectId")
	if !ok {
		writeError(w, 400, "invalid projectId")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON body")
		return
	}
	room, err := h.store.EnsureRoom(r.Context(), projectID, body.Name)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, room)
}

func (h *Handler) getRoom(w http.ResponseWriter, r *http.Request) {
	roomID, ok := parseUUID(r, "roomId")
	if !ok {
		writeError(w, 400, "invalid roomId")
		return
	}
	room, err := h.store.GetRoom(r.Context(), roomID)
	if err != nil {
		writeError(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, room)
}

func (h *Handler) archiveRoom(w http.ResponseWriter, r *http.Request) {
	roomID, ok := parseUUID(r, "roomId")
	if !ok {
		writeError(w, 400, "invalid roomId")
		return
	}
	if err := h.store.ArchiveRoom(r.Context(), roomID); err != nil {
		writeError(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "archived"})
}

// ============================================================================
// Message endpoints
// ============================================================================

func (h *Handler) getMessages(w http.ResponseWriter, r *http.Request) {
	roomID, ok := parseUUID(r, "roomId")
	if !ok {
		writeError(w, 400, "invalid roomId")
		return
	}
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)
	depth := queryInt(r, "threadDepth", 1)

	messages, err := h.store.GetMessages(r.Context(), roomID, limit, offset, depth)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, messages)
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	roomID, ok := parseUUID(r, "roomId")
	if !ok {
		writeError(w, 400, "invalid roomId")
		return
	}
	// Author provenance is server-stamped from the authenticated principal —
	// never from the request body — so a caller cannot impersonate another
	// agent/human. Fail closed if the request is unauthenticated.
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, 401, "unauthenticated")
		return
	}
	var msg Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		writeError(w, 400, "invalid JSON body")
		return
	}
	msg.RoomID = roomID
	// Overwrite any client-supplied author fields with the trusted principal.
	msg.AuthorID = principal.ID
	msg.AuthorType = principal.Type
	msg.AuthorName = principal.Name
	posted, err := h.store.PostMessage(r.Context(), roomID, msg)
	if err != nil {
		switch err {
		case ErrRoomArchived:
			writeError(w, 409, err.Error())
		case ErrParentMismatch, ErrMessageNotFound:
			writeError(w, 400, err.Error())
		default:
			writeError(w, 500, err.Error())
		}
		return
	}
	writeJSON(w, 201, posted)
}

func (h *Handler) editMessage(w http.ResponseWriter, r *http.Request) {
	messageID, ok := parseUUID(r, "messageId")
	if !ok {
		writeError(w, 400, "invalid messageId")
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON body")
		return
	}
	edited, err := h.store.EditMessage(r.Context(), messageID, body.Body)
	if err != nil {
		writeError(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, edited)
}

func (h *Handler) deleteMessage(w http.ResponseWriter, r *http.Request) {
	messageID, ok := parseUUID(r, "messageId")
	if !ok {
		writeError(w, 400, "invalid messageId")
		return
	}
	if err := h.store.DeleteMessage(r.Context(), messageID); err != nil {
		writeError(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// ============================================================================
// Search + Memory Index
// ============================================================================

func (h *Handler) searchMessages(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseUUID(r, "projectId")
	if !ok {
		writeError(w, 400, "invalid projectId")
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, 400, "missing 'q' query parameter")
		return
	}
	limit := queryInt(r, "limit", 20)
	results, err := h.store.SearchMessages(r.Context(), projectID, q, limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, results)
}

func (h *Handler) memoryIndex(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseUUID(r, "projectId")
	if !ok {
		writeError(w, 400, "invalid projectId")
		return
	}
	limit := queryInt(r, "limit", 200)
	since := r.URL.Query().Get("since")
	var sinceTime time.Time
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err == nil {
			sinceTime = t
		}
	}
	if sinceTime.IsZero() {
		sinceTime = time.Unix(0, 0)
	}
	records, err := h.store.ForMemoryIndex(r.Context(), projectID, sinceTime, limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, records)
}
