package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type cephSource struct {
	config cephConfig
}

func newCephSource(cfg cephConfig) cephSource {
	return cephSource{config: cfg}
}

func (source cephSource) CanSynchronize(ctx context.Context) (bool, string, error) {
	if source.config.Coordination == "none" {
		return true, "", nil
	}
	out, err := source.run(ctx, "mgr", "dump", "--format", "json")
	if err != nil {
		return false, "", fmt.Errorf("read active ceph manager: %w", err)
	}
	active, err := parseActiveManager(out)
	if err != nil {
		return false, "", err
	}
	local := source.config.ManagerName
	if local == "" && source.config.Transport == "ssh" {
		local = source.config.Host
	}
	if local == "" {
		local, err = os.Hostname()
		if err != nil {
			return false, "", fmt.Errorf("read local hostname: %w", err)
		}
	}
	return managerNamesMatch(local, active), active, nil
}

func (source cephSource) FSID(ctx context.Context) (string, error) {
	out, err := source.run(ctx, "fsid")
	if err != nil {
		return "", fmt.Errorf("read Ceph FSID: %w", err)
	}
	fsid := strings.TrimSpace(string(out))
	if fsid == "" || strings.ContainsAny(fsid, " \t\r\n") {
		return "", errors.New("read Ceph FSID: invalid response")
	}
	return fsid, nil
}

func (source cephSource) Key(ctx context.Context, entity string) (string, error) {
	out, err := source.run(ctx, "auth", "get-key", entity)
	if err != nil {
		return "", fmt.Errorf("read key for %s: %w", entity, err)
	}
	key := strings.TrimSpace(string(out))
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return "", fmt.Errorf("read key for %s: invalid response", entity)
	}
	return key, nil
}

func (source cephSource) run(ctx context.Context, args ...string) ([]byte, error) {
	var cmd *exec.Cmd
	if source.config.Transport == "local" {
		commandArgs := append(append([]string{}, source.config.Command[1:]...), args...)
		cmd = exec.CommandContext(ctx, source.config.Command[0], commandArgs...)
	} else {
		target := source.config.Host
		if source.config.User != "" {
			target = source.config.User + "@" + target
		}
		remoteArgs := append(append([]string{}, source.config.Command...), args...)
		quoted := make([]string, len(remoteArgs))
		for i, arg := range remoteArgs {
			quoted[i] = shellQuote(arg)
		}
		cmd = exec.CommandContext(ctx, "ssh", "-p", strconv.Itoa(source.config.Port), target, strings.Join(quoted, " "))
	}

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message := strings.TrimSpace(string(exitErr.Stderr))
		if message != "" {
			return nil, fmt.Errorf("command failed: %s", message)
		}
	}
	return nil, fmt.Errorf("command failed: %w", err)
}

func parseActiveManager(out []byte) (string, error) {
	var status struct {
		ActiveName string `json:"active_name"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("decode active ceph manager: %w", err)
	}
	if strings.TrimSpace(status.ActiveName) == "" {
		return "", errors.New("decode active ceph manager: active_name is empty")
	}
	return status.ActiveName, nil
}

func managerNamesMatch(local, active string) bool {
	return canonicalManagerName(local) == canonicalManagerName(active)
}

func canonicalManagerName(value string) string {
	value = strings.Trim(strings.ToLower(strings.TrimSpace(value)), "[]")
	value = strings.TrimSuffix(value, ".")
	value = strings.TrimPrefix(value, "mgr.")
	if net.ParseIP(value) != nil {
		return value
	}
	if i := strings.IndexByte(value, '.'); i >= 0 {
		return value[:i]
	}
	return value
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
