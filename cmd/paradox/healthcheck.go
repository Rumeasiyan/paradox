package main

import "errors"

// runHealthcheck answers the container health check (backlog story 14).
func runHealthcheck(getenv func(string) string) error {
	return errors.New("healthcheck is not built yet (backlog story 14)")
}
