// Package schedule parses a system's schedules.yaml (PX-SCH-02, PX-SYS-03,
// PX-SYS-04). A bad file is reported with its name, line and fault so the
// deployment can be fixed without guessing (PX-SCH-04).
package schedule

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

// Wake modes (PX-SYS-03).
const (
	WakeSablier = "sablier"
	WakeNone    = "none"
)

// System is one parsed schedules.yaml.
type System struct {
	Name    string
	Wake    string // WakeSablier or WakeNone
	Address string // empty means the system's name on the internal network
	Jobs    []Job
}

// Job is one entry under jobs.
type Job struct {
	Name    string
	Every   string        // cron expression as written; empty means queue only
	Repeat  cron.Schedule // parsed Every; nil when Every is empty
	Timeout time.Duration
	Retries int
	Line    int // where the job starts in the file, for later messages
}

// Defaults fill fields a job leaves out (PARADOX_JOB_TIMEOUT, PARADOX_JOB_RETRIES).
type Defaults struct {
	Timeout time.Duration
	Retries int
}

// Names become DNS labels and URL path segments (/jobs/<name>), so they are
// limited to what is safe in both.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether s is a valid system or job name: lower-case
// letters, digits and hyphens, starting and ending with a letter or digit,
// at most 63 characters. Other packages use it so there is one rule.
func ValidName(s string) bool { return nameRE.MatchString(s) }

const maxRetries = 100

// Parse reads one schedules.yaml. file is used only in messages.
func Parse(file string, data []byte, d Defaults) (System, error) {
	p := parser{file: file}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return System{}, fmt.Errorf("%s: %v", file, err)
	}
	if len(doc.Content) == 0 {
		return System{}, fmt.Errorf("%s: file is empty", file)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return System{}, fmt.Errorf("%s:%d: top level must be a mapping with system and jobs", file, root.Line)
	}

	s := System{Wake: WakeSablier}
	var jobsNode *yaml.Node
	seen := map[string]bool{}
	for k, v := range pairs(root) {
		if seen[k.Value] {
			p.fail(k, "%s appears twice", k.Value)
			continue
		}
		seen[k.Value] = true
		switch k.Value {
		case "system":
			s.Name = p.scalar(v, "system")
		case "wake":
			s.Wake = p.scalar(v, "wake")
			if s.Wake != WakeSablier && s.Wake != WakeNone {
				p.fail(v, "wake must be %s or %s", WakeSablier, WakeNone)
			}
		case "address":
			s.Address = p.scalar(v, "address")
		case "jobs":
			jobsNode = v
		default:
			p.fail(k, "unknown field %s", k.Value)
		}
	}

	if !seen["system"] {
		p.fail(root, "system is required")
	} else if !nameRE.MatchString(s.Name) {
		p.fail(root, "system %q must be lower-case letters, digits and hyphens, at most 63 characters", s.Name)
	}
	if s.Address != "" {
		p.checkAddress(root, s)
	}

	switch {
	case jobsNode == nil:
		p.fail(root, "jobs is required")
	case jobsNode.Kind != yaml.SequenceNode:
		p.fail(jobsNode, "jobs must be a list")
	case len(jobsNode.Content) == 0:
		p.fail(jobsNode, "jobs must list at least one job")
	default:
		names := map[string]bool{}
		for i, n := range jobsNode.Content {
			j := p.job(n, i, d)
			if j.Name != "" && names[j.Name] {
				p.fail(n, "job %s appears twice", j.Name)
			}
			names[j.Name] = true
			s.Jobs = append(s.Jobs, j)
		}
	}

	if len(p.errs) > 0 {
		return System{}, errors.Join(p.errs...)
	}
	return s, nil
}

// checkAddress applies PX-SYS-03 and PX-SYS-04: an explicit address is for a
// system off the internal network, so it must be encrypted and is never woken.
func (p *parser) checkAddress(at *yaml.Node, s System) {
	u, err := url.Parse(s.Address)
	switch {
	case err != nil || u.Host == "":
		p.fail(at, "address must be an absolute URL")
	case u.Scheme != "https":
		p.fail(at, "address must use https: calls leave the internal network")
	case u.Path != "" && u.Path != "/":
		p.fail(at, "address must not have a path; PARADOX adds /jobs/<name>")
	}
	if s.Wake == WakeSablier {
		p.fail(at, "a system with an address must set wake: none, because only systems on this server can be woken")
	}
}

func (p *parser) job(n *yaml.Node, i int, d Defaults) Job {
	j := Job{Timeout: d.Timeout, Retries: d.Retries, Line: n.Line}
	where := fmt.Sprintf("jobs[%d]", i)
	if n.Kind != yaml.MappingNode {
		p.fail(n, "%s must be a mapping with name and optional every, timeout, retries", where)
		return j
	}
	seen := map[string]bool{}
	for k, v := range pairs(n) {
		if seen[k.Value] {
			p.fail(k, "%s.%s appears twice", where, k.Value)
			continue
		}
		seen[k.Value] = true
		switch k.Value {
		case "name":
			j.Name = p.scalar(v, where+".name")
			if !nameRE.MatchString(j.Name) {
				p.fail(v, "%s.name %q must be lower-case letters, digits and hyphens, at most 63 characters", where, j.Name)
			}
		case "every":
			j.Every = p.scalar(v, where+".every")
			sched, err := cron.ParseStandard(j.Every)
			if err != nil || strings.HasPrefix(strings.TrimSpace(j.Every), "TZ=") || strings.HasPrefix(strings.TrimSpace(j.Every), "CRON_TZ=") {
				// Timezones come only from PARADOX_TIMEZONE, so every file is read the same way (PX-SCH-03).
				p.fail(v, "%s.every must be five cron fields (minute hour day month weekday) or a @ shortcut, without a timezone", where)
			} else {
				j.Repeat = sched
			}
		case "timeout":
			raw := p.scalar(v, where+".timeout")
			t, err := time.ParseDuration(raw)
			if err != nil || t <= 0 {
				p.fail(v, "%s.timeout must be a positive duration such as 90s or 10m", where)
			} else {
				j.Timeout = t
			}
		case "retries":
			raw := p.scalar(v, where+".retries")
			r, err := strconv.Atoi(raw)
			if err != nil || r < 0 || r > maxRetries {
				p.fail(v, "%s.retries must be a whole number from 0 to %d", where, maxRetries)
			} else {
				j.Retries = r
			}
		default:
			p.fail(k, "%s has unknown field %s", where, k.Value)
		}
	}
	if !seen["name"] {
		p.fail(n, "%s.name is required", where)
	}
	return j
}

type parser struct {
	file string
	errs []error
}

func (p *parser) fail(at *yaml.Node, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s:%d: %s", p.file, at.Line, fmt.Sprintf(format, args...)))
}

func (p *parser) scalar(n *yaml.Node, field string) string {
	if n.Kind != yaml.ScalarNode {
		p.fail(n, "%s must be a single value", field)
		return ""
	}
	return strings.TrimSpace(n.Value)
}

// pairs walks a mapping node's key and value nodes in file order.
func pairs(m *yaml.Node) func(func(k, v *yaml.Node) bool) {
	return func(yield func(k, v *yaml.Node) bool) {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if !yield(m.Content[i], m.Content[i+1]) {
				return
			}
		}
	}
}
