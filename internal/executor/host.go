package executor

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// hostTools are the programs agents most often reach for; the summary says
// which of them this machine has.
var hostTools = []string{"docker", "git", "curl", "wget", "python3", "node", "npm", "bun", "jq", "go", "make", "timeout", "gtimeout"}

// HostSummary describes the machine agents run commands on: operating
// system, which common tools are on PATH, Docker's version and the running
// containers. Every probe is bounded; the whole summary takes a few seconds
// at most, and a missing tool is reported rather than failing.
func HostSummary(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	probe := func(name string, args ...string) string {
		c, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		out, err := exec.CommandContext(c, name, args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	var b strings.Builder
	osName := runtime.GOOS
	switch runtime.GOOS {
	case "darwin":
		if v := probe("sw_vers", "-productVersion"); v != "" {
			osName = "macOS " + v
		}
	default:
		if v := probe("uname", "-sr"); v != "" {
			osName = v
		}
	}
	fmt.Fprintf(&b, "- OS: %s (%s)\n", osName, runtime.GOARCH)
	var have, missing []string
	for _, tool := range hostTools {
		if _, err := exec.LookPath(tool); err == nil {
			have = append(have, tool)
		} else {
			missing = append(missing, tool)
		}
	}
	fmt.Fprintf(&b, "- On PATH: %s\n", strings.Join(have, ", "))
	if len(missing) > 0 {
		fmt.Fprintf(&b, "- Not installed: %s\n", strings.Join(missing, ", "))
	}
	if !contains(have, "timeout") && !contains(have, "gtimeout") {
		b.WriteString("- There is no `timeout` program: bound commands with run_command's timeout_seconds instead.\n")
	}
	if contains(have, "docker") {
		if v := probe("docker", "version", "--format", "{{.Server.Version}}"); v != "" {
			compose := probe("docker", "compose", "version", "--short")
			fmt.Fprintf(&b, "- Docker server %s, compose %s\n", v, orNone(compose))
			containers := probe("docker", "ps", "--format", "{{.Names}} ({{.Image}}): {{.Status}}")
			lines := strings.Split(containers, "\n")
			if containers == "" {
				lines = nil
			}
			if len(lines) > 30 {
				lines = append(lines[:30], fmt.Sprintf("... and %d more", len(lines)-30))
			}
			fmt.Fprintf(&b, "- Running containers: %d\n", len(lines))
			for _, l := range lines {
				fmt.Fprintf(&b, "  - %s\n", l)
			}
		} else {
			b.WriteString("- Docker is installed but its daemon did not answer (is Docker Desktop running?)\n")
		}
	}
	return b.String()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "not available"
	}
	return s
}
