package main

// jarvix backup / jarvix restore (ADR 0045): the CLI face of
// internal/backup. Everything of substance — discovery, the daemon hold,
// the manifest, the refusal matrix, the staged swap — lives in that
// package; this file parses flags, prints the report, and keeps the exit
// codes stable for cron: 0 success, 1 any failure, 2 unknown command.

import (
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/backup"
	"github.com/rpickz/jarvix/internal/config"
)

// cmdBackup archives the config and state roots. quiet is for the cron
// line: success prints nothing, failures still reach stderr through run().
func cmdBackup(paths config.Paths, args []string) error {
	var dest string
	var noSecrets, quiet bool
	for _, arg := range args {
		switch {
		case arg == "--no-secrets":
			noSecrets = true
		case arg == "--quiet":
			quiet = true
		case strings.HasPrefix(arg, "--"):
			return fmt.Errorf("usage: jarvix backup [path] [--no-secrets] [--quiet]")
		case dest == "":
			dest = arg
		default:
			return fmt.Errorf("usage: jarvix backup [path] [--no-secrets] [--quiet]")
		}
	}
	report, err := backup.Create(paths, backup.ResolveDest(dest, time.Now()), backup.CreateOptions{NoSecrets: noSecrets})
	if err != nil {
		return err
	}
	if quiet {
		return nil
	}
	fmt.Printf("Backed up %d files to %s (%s capture).\n", report.Files, report.Path, report.Capture)
	if len(report.RedactedKeys) > 0 {
		fmt.Printf("Redacted api keys: %s — a restore of this archive will need them re-entered.\n",
			strings.Join(report.RedactedKeys, ", "))
	}
	for _, link := range report.SkippedSymlinks {
		fmt.Printf("Skipped symlink %s — links are never followed into an archive.\n", link)
	}
	return nil
}

// cmdRestore validates an archive and swaps it into place. The report names
// every safety copy, because "where did my old state go" must never need
// source code to answer.
func cmdRestore(paths config.Paths, args []string) error {
	var archive string
	var quiet bool
	for _, arg := range args {
		switch {
		case arg == "--quiet":
			quiet = true
		case strings.HasPrefix(arg, "--"):
			return fmt.Errorf("usage: jarvix restore <archive.tar.gz> [--quiet]")
		case archive == "":
			archive = arg
		default:
			return fmt.Errorf("usage: jarvix restore <archive.tar.gz> [--quiet]")
		}
	}
	if archive == "" {
		return fmt.Errorf("usage: jarvix restore <archive.tar.gz> [--quiet]")
	}
	report, err := backup.Restore(paths, archive, backup.RestoreOptions{})
	if err != nil {
		return err
	}
	if quiet {
		return nil
	}
	fmt.Printf("Restored %d files from %s.\n", report.Files, archive)
	for _, aside := range report.SafetyCopies {
		fmt.Printf("Previous state moved aside to %s — remove it once you trust the restore.\n", aside)
	}
	for _, key := range report.RedactedKeys {
		fmt.Printf("The archive was made with --no-secrets: re-enter %s in config.toml.\n", key)
	}
	for _, warning := range report.Warnings {
		fmt.Println("Note:", warning)
	}
	fmt.Println("Validated: the restored configuration and stores load. Run `jarvix doctor`, then start the daemon.")
	return nil
}
