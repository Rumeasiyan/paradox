package api

import "net/http"

// read answers GET /api/jobs/{run_id} for the owning system (PX-JOB-04).
// Placeholder until backlog story 21.
func (a *API) read(w http.ResponseWriter, r *http.Request, c caller) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "reading a run arrives in backlog story 21")
}
