package simulator

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// BureauBehaviour describes how the simulated bureau answers for one BVN.
type BureauBehaviour struct {
	Score                   *int
	WorstDelinquencyDays12m int
	MonthlyObligationsMinor int64
	TotalOutstandingMinor   int64
	ActiveLoans             int
	// Fail is "", "timeout", "http500" or "malformed".
	Fail string
	// FailFirst makes the first N calls for a request reference answer 503.
	FailFirst int
}

func score(v int) *int { return &v }

// SetBureauBehaviour overrides the answer for one BVN.
func (s *Server) SetBureauBehaviour(bvn string, b BureauBehaviour) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bureauByBVN[bvn] = b
}

// BureauCalls returns how many enquiries were received for a request ref.
func (s *Server) BureauCalls(requestRef string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bureauCalls[requestRef]
}

func bureauDefault(bvn string) BureauBehaviour {
	suffix := ""
	if len(bvn) >= 4 {
		suffix = bvn[len(bvn)-4:]
	}
	switch suffix {
	case "4201":
		return BureauBehaviour{Score: score(640)}
	case "4202":
		return BureauBehaviour{Score: score(560)}
	case "4203":
		return BureauBehaviour{Score: score(480)}
	case "4204":
		return BureauBehaviour{} // thin file: no score
	case "4205":
		return BureauBehaviour{Score: score(700), WorstDelinquencyDays12m: 120}
	case "4206":
		return BureauBehaviour{Score: score(720), MonthlyObligationsMinor: 9_000_000, ActiveLoans: 2, TotalOutstandingMinor: 40_000_000}
	case "4207":
		return BureauBehaviour{Fail: "timeout"}
	case "4208":
		return BureauBehaviour{Fail: "malformed"}
	case "4209":
		return BureauBehaviour{Fail: "http500"}
	case "4210":
		return BureauBehaviour{Score: score(720), FailFirst: 2}
	default:
		return BureauBehaviour{Score: score(720)}
	}
}

type bureauRequest struct {
	RequestRef  string `json:"request_ref"`
	BVN         string `json:"bvn"`
	FullName    string `json:"full_name"`
	DateOfBirth string `json:"date_of_birth"`
}

type bureauResponse struct {
	ReportRef               string `json:"report_ref"`
	Score                   *int   `json:"score"`
	ActiveLoans             int    `json:"active_loans"`
	TotalOutstandingMinor   int64  `json:"total_outstanding_minor"`
	MonthlyObligationsMinor int64  `json:"monthly_obligations_minor"`
	WorstDelinquencyDays12m int    `json:"worst_delinquency_days_12m"`
	AsOf                    string `json:"as_of"`
}

func (s *Server) bureauReport(w http.ResponseWriter, r *http.Request) {
	var req bureauRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RequestRef == "" || req.BVN == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	s.mu.Lock()
	s.bureauCalls[req.RequestRef]++
	calls := s.bureauCalls[req.RequestRef]
	b, ok := s.bureauByBVN[req.BVN]
	if !ok {
		b = bureauDefault(req.BVN)
	}
	cached := s.bureauReports[req.RequestRef]
	s.mu.Unlock()

	if calls <= b.FailFirst {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "TEMPORARILY_UNAVAILABLE"})
		return
	}
	switch b.Fail {
	case "timeout":
		hang(r)
		return
	case "http500":
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	case "malformed":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"report_ref": "x", "score": "not-a-number"`)
		return
	}
	// The same request reference always returns the same report.
	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}
	body, _ := json.Marshal(bureauResponse{
		ReportRef: "BR-" + uuid.NewString(), Score: b.Score, ActiveLoans: b.ActiveLoans,
		TotalOutstandingMinor: b.TotalOutstandingMinor, MonthlyObligationsMinor: b.MonthlyObligationsMinor,
		WorstDelinquencyDays12m: b.WorstDelinquencyDays12m, AsOf: s.opts.Now().UTC().Format(time.RFC3339),
	})
	s.mu.Lock()
	s.bureauReports[req.RequestRef] = body
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
