package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	pgClientRoot   = "/usr/libexec"
	pgClientPrefix = "postgresql"
	pgDumpBinary   = "pg_dump"
	psqlBinary     = "psql"
)

type serverProbe struct {
	Host     string
	Port     int
	Username string
	Database string
	Env      []string
}

func resolvePgDump(ctx context.Context, root string, probe *serverProbe, logger *slog.Logger) (string, error) {
	return resolveClientBinary(ctx, root, probe, logger, pgDumpBinary, pickDumpMajor)
}

func resolvePsql(ctx context.Context, root string, probe *serverProbe, logger *slog.Logger) (string, error) {
	return resolveClientBinary(ctx, root, probe, logger, psqlBinary, pickPsqlMajor)
}

func resolveClientBinary(ctx context.Context, root string, probe *serverProbe, logger *slog.Logger,
	tool string, pick func(available []int, server int) (int, bool)) (string, error) {
	bundled, err := bundledClientMajors(root)
	if err != nil {
		return "", err
	}
	if len(bundled) == 0 {
		return tool, nil
	}

	newest := bundled[len(bundled)-1]
	serverVersion, err := serverVersionNum(ctx, probe, clientBinaryPath(root, newest, psqlBinary))
	if err != nil {
		return "", err
	}

	serverMajor := serverVersion / 10000
	major, ok := pick(bundled, serverMajor)
	if !ok {
		return "", fmt.Errorf("no %s for a server on major %d: %s bundles %s",
			tool, serverMajor, root, joinInts(bundled))
	}

	logger.Info("selected PostgreSQL client", "tool", tool, "clientMajor", major, "serverVersion", serverVersion)
	return clientBinaryPath(root, major, tool), nil
}

// pg_dump refuses a server newer than itself, so a client below the server's major is no fallback.
func pickDumpMajor(available []int, server int) (int, bool) {
	return smallestFrom(available, server)
}

// psql emits SQL text only, so the newest client below the server still restores a dump the server understands.
func pickPsqlMajor(available []int, server int) (int, bool) {
	if major, ok := smallestFrom(available, server); ok {
		return major, true
	}
	return largestBelow(available, server)
}

func smallestFrom(available []int, server int) (int, bool) {
	best, found := 0, false
	for _, major := range available {
		if major >= server && (!found || major < best) {
			best, found = major, true
		}
	}
	return best, found
}

func largestBelow(available []int, server int) (int, bool) {
	best, found := 0, false
	for _, major := range available {
		if major < server && (!found || major > best) {
			best, found = major, true
		}
	}
	return best, found
}

func bundledClientMajors(root string) ([]int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list %s: %w", root, err)
	}

	majors := make([]int, 0, len(entries))
	for _, entry := range entries {
		major, ok := clientDirMajor(entry.Name())
		if !ok {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, entry.Name())); err == nil && info.IsDir() {
			majors = append(majors, major)
		}
	}
	sort.Ints(majors)
	return majors, nil
}

func clientDirMajor(name string) (int, bool) {
	suffix, ok := strings.CutPrefix(name, pgClientPrefix)
	if !ok {
		return 0, false
	}
	major, err := strconv.Atoi(suffix)
	if err != nil || major <= 0 {
		return 0, false
	}
	return major, true
}

func clientBinaryPath(root string, major int, tool string) string {
	return filepath.Join(root, pgClientPrefix+strconv.Itoa(major), tool)
}

func serverVersionNum(ctx context.Context, probe *serverProbe, psql string) (int, error) {
	args := []string{
		"-h", probe.Host,
		"-p", strconv.Itoa(probe.Port),
		"-U", probe.Username,
		"-d", probe.Database,
		"-tAc", "SHOW server_version_num",
	}
	var stdout bytes.Buffer
	diagnostic, err := runPsql(ctx, psql, probe.Env, args, &stdout)
	if err != nil {
		return 0, fmt.Errorf("server version probe failed: %w", withDiagnostic(err, diagnostic))
	}

	reported := strings.TrimSpace(stdout.String())
	version, err := strconv.Atoi(reported)
	if err != nil {
		return 0, fmt.Errorf("server reported an unparsable server_version_num %q", reported)
	}
	return version, nil
}

func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, ", ")
}
