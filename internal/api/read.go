package api

import "net/http"

// read answers GET /api/jobs/{run_id} for the owning system (PX-JOB-04).
// The signature has already been checked. The answer is the run as
// contract section 6 shows it, never its data (PX-NFR-06): another
// system's run is 403 and an unknown or malformed ID is 404.
func (a *API) read(w http.ResponseWriter, r *http.Request, c caller) {
	v, owner, err := loadRun(r.Context(), a.Pool, r.PathValue("run_id"), false)
	if a.answerLoadError(w, c, owner, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}
