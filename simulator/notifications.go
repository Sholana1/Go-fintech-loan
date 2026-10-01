package simulator

import (
	"encoding/json"
	"io"
	"net/http"
)

// Message is one notification the simulator accepted. It is never delivered
// to anyone: the simulator only records that it was asked.
type Message struct {
	MessageID  string            `json:"message_id"`
	CustomerID string            `json:"customer_id"`
	Template   string            `json:"template"`
	Params     map[string]string `json:"params"`
	// Deliveries is how many times this message id was submitted. A sender
	// that retries correctly reuses the id, so this can exceed 1 while the
	// customer would still be notified once.
	Deliveries int `json:"-"`
}

// SetNotifyFault makes the notification endpoint fail: "timeout", "http500"
// or "malformed". An empty fault clears it.
func (s *Server) SetNotifyFault(fault string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyFault = fault
}

// Messages returns the accepted messages in the order first received.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, len(s.messageOrder))
	for _, id := range s.messageOrder {
		out = append(out, s.messages[id])
	}
	return out
}

// NotifyCalls returns how many requests the notification endpoint received,
// including failed ones.
func (s *Server) NotifyCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notifyCalls
}

// acceptMessage is POST /notify/v1/messages. It is idempotent on message_id:
// a repeat is acknowledged and recorded once.
func (s *Server) acceptMessage(w http.ResponseWriter, r *http.Request) {
	var m Message
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.MessageID == "" || m.CustomerID == "" || m.Template == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	s.mu.Lock()
	s.notifyCalls++
	fault := s.notifyFault
	s.mu.Unlock()

	switch fault {
	case "timeout":
		hang(r)
		return
	case "http500":
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	case "malformed":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message_id": "`)
		return
	}

	s.mu.Lock()
	existing, seen := s.messages[m.MessageID]
	if seen {
		existing.Deliveries++
		s.messages[m.MessageID] = existing
	} else {
		m.Deliveries = 1
		s.messages[m.MessageID] = m
		s.messageOrder = append(s.messageOrder, m.MessageID)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]string{"message_id": m.MessageID, "status": "ACCEPTED"})
}
