package api

import "net/http"

// cancel answers POST /api/jobs/{run_id}/cancel for the owning system
// (PX-JOB-05). Placeholder until backlog story 22.
func (a *API) cancel(w http.ResponseWriter, r *http.Request, c caller) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "cancelling a run arrives in backlog story 22")
}
