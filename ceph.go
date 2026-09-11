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

func (source cephSource) Version(ctx context.Context) (cephVersion, error) {
	out, err := source.run(ctx, "version")
	if err != nil {
		return cephVersion{}, fmt.Errorf("read Ceph version: %w", err)
	}
	return parseCephVersion(string(out))
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

func (source cephSource) Credential(ctx context.Context, credential credentialSpec) (map[string]string, error) {
	switch credential.Kind {
	case "rook-mon", "rook-csi", "rook-cephfs-csi":
		key, err := source.Key(ctx, credential.Entity)
		return map[string]string{"key": key}, err
	case "rook-config":
		out, err := source.run(ctx, "mon", "dump", "--format", "json")
		if err != nil {
			return nil, fmt.Errorf("read Ceph monitors: %w", err)
		}
		return parseMonConfig(out)
	case "rook-dashboard":
		out, err := source.run(ctx, "mgr", "services", "--format", "json")
		if err != nil {
			return nil, fmt.Errorf("read Ceph manager services: %w", err)
		}
		return parseDashboard(out)
	case "rook-rgw-admin":
		return source.RGWCredentials(ctx, credential.Entity)
	default:
		return nil, fmt.Errorf("unsupported credential kind %q", credential.Kind)
	}
}

func (source cephSource) InitRBDPool(ctx context.Context, pool string) error {
	if _, err := source.runCommand(ctx, source.config.RBDCommand, "pool", "init", pool); err != nil {
		return fmt.Errorf("initialize RBD pool %s: %w", pool, err)
	}
	return nil
}

func (source cephSource) EnsureCephFSSubvolumeGroup(ctx context.Context, filesystem string) error {
	if _, err := source.run(ctx, "fs", "subvolumegroup", "create", filesystem, "csi"); err != nil {
		return fmt.Errorf("create CephFS csi subvolume group: %w", err)
	}
	if _, err := source.run(ctx, "fs", "subvolumegroup", "pin", filesystem, "csi", "distributed", "1"); err != nil {
		return fmt.Errorf("pin CephFS csi subvolume group: %w", err)
	}
	return nil
}

func (source cephSource) CreateKey(ctx context.Context, entity string, caps []string, keyType string) error {
	args := append([]string{"auth", "get-or-create", entity}, caps...)
	args = append(args, "--format", "json")
	if keyType != "" {
		args = append(args, "--key-type", keyType)
	}
	if _, err := source.run(ctx, args...); err != nil {
		return fmt.Errorf("create CephX entity %s: %w", entity, err)
	}
	return nil
}

func (source cephSource) RGWCredentials(ctx context.Context, uid string) (map[string]string, error) {
	args := source.rgwArgs("user", "info", "--uid", uid, "--format", "json")
	out, err := source.runCommand(ctx, source.config.RGWCommand, args...)
	if err != nil {
		return nil, fmt.Errorf("read RGW user %s: %w", uid, err)
	}
	return parseRGWCredentials(out, uid)
}

func (source cephSource) EnsureRGWAdmin(ctx context.Context, uid string) (bool, error) {
	created := false
	if _, err := source.RGWCredentials(ctx, uid); err != nil {
		caps := "buckets=*;users=*;usage=read;metadata=read;zone=read"
		args := source.rgwArgs("user", "create", "--uid", uid, "--display-name", "Rook RGW Admin Ops user", "--caps", caps, "--format", "json")
		if _, err := source.runCommand(ctx, source.config.RGWCommand, args...); err != nil {
			return false, fmt.Errorf("create RGW admin user %s: %w", uid, err)
		}
		created = true
	}
	args := source.rgwArgs("caps", "add", "--uid", uid, "--caps", "info=read", "--format", "json")
	if _, err := source.runCommand(ctx, source.config.RGWCommand, args...); err != nil {
		return false, fmt.Errorf("add RGW info capability to %s: %w", uid, err)
	}
	return created, nil
}

func (source cephSource) rgwArgs(args ...string) []string {
	if source.config.RGWRealm != "" {
		args = append(args, "--rgw-realm", source.config.RGWRealm)
	}
	if source.config.RGWZoneGroup != "" {
		args = append(args, "--rgw-zonegroup", source.config.RGWZoneGroup)
	}
	if source.config.RGWZone != "" {
		args = append(args, "--rgw-zone", source.config.RGWZone)
	}
	return args
}

func (source cephSource) run(ctx context.Context, args ...string) ([]byte, error) {
	return source.runCommand(ctx, source.config.Command, args...)
}

func (source cephSource) runCommand(ctx context.Context, command []string, args ...string) ([]byte, error) {
	var cmd *exec.Cmd
	if source.config.Transport == "local" {
		commandArgs := append(append([]string{}, command[1:]...), args...)
		cmd = exec.CommandContext(ctx, command[0], commandArgs...)
	} else {
		target := source.config.Host
		if source.config.User != "" {
			target = source.config.User + "@" + target
		}
		remoteArgs := append(append([]string{}, command...), args...)
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

func parseMonConfig(out []byte) (map[string]string, error) {
	var dump struct {
		Mons []struct {
			Name        string `json:"name"`
			PublicAddr  string `json:"public_addr"`
			PublicAddrs struct {
				AddrVec []struct {
					Type string `json:"type"`
					Addr string `json:"addr"`
				} `json:"addrvec"`
			} `json:"public_addrs"`
		} `json:"mons"`
	}
	if err := json.Unmarshal(out, &dump); err != nil {
		return nil, fmt.Errorf("decode Ceph monitors: %w", err)
	}
	if len(dump.Mons) == 0 {
		return nil, errors.New("decode Ceph monitors: no monitors")
	}
	members := make([]string, 0, len(dump.Mons))
	hosts := make([]string, 0, len(dump.Mons))
	for _, mon := range dump.Mons {
		name := strings.TrimSpace(mon.Name)
		if name == "" {
			return nil, errors.New("decode Ceph monitors: monitor name is empty")
		}
		addresses := make([]string, 0, len(mon.PublicAddrs.AddrVec))
		for _, address := range mon.PublicAddrs.AddrVec {
			addr, _, _ := strings.Cut(strings.TrimSpace(address.Addr), "/")
			if address.Type == "" || addr == "" {
				return nil, fmt.Errorf("decode Ceph monitors: monitor %s has an invalid address", name)
			}
			addresses = append(addresses, address.Type+":"+addr)
		}
		if len(addresses) == 0 {
			addr, _, _ := strings.Cut(strings.TrimSpace(mon.PublicAddr), "/")
			if addr == "" {
				return nil, fmt.Errorf("decode Ceph monitors: monitor %s has no public address", name)
			}
			if !strings.HasPrefix(addr, "v1:") && !strings.HasPrefix(addr, "v2:") {
				addr = "v1:" + addr
			}
			addresses = append(addresses, addr)
		}
		members = append(members, name)
		hosts = append(hosts, "["+strings.Join(addresses, ",")+"]")
	}
	return map[string]string{
		"mon_host":            strings.Join(hosts, ","),
		"mon_initial_members": strings.Join(members, ","),
	}, nil
}

func parseDashboard(out []byte) (map[string]string, error) {
	var services map[string]string
	if err := json.Unmarshal(out, &services); err != nil {
		return nil, fmt.Errorf("decode Ceph manager services: %w", err)
	}
	link := strings.TrimSpace(services["dashboard"])
	if link == "" {
		return nil, errors.New("decode Ceph manager services: dashboard is unavailable")
	}
	return map[string]string{"url": link}, nil
}

func parseRGWCredentials(out []byte, uid string) (map[string]string, error) {
	var user struct {
		Keys []struct {
			User      string `json:"user"`
			AccessKey string `json:"access_key"`
			SecretKey string `json:"secret_key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(out, &user); err != nil {
		return nil, fmt.Errorf("decode RGW user %s: %w", uid, err)
	}
	for _, key := range user.Keys {
		if (key.User == "" || key.User == uid) && key.AccessKey != "" && key.SecretKey != "" {
			return map[string]string{"accessKey": key.AccessKey, "secretKey": key.SecretKey}, nil
		}
	}
	return nil, fmt.Errorf("decode RGW user %s: no usable S3 key", uid)
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
	if before, _, ok := strings.Cut(value, "."); ok {
		return before
	}
	return value
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
