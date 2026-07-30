package removal

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// §9.1: "Offer a reviewable script as an alternative to acting in-app. Export the
// user's selected operations as a shell script they can read, edit, and run
// themselves. Given the intended operator prefers verifying over trusting, expect
// this to be the primary path rather than a fallback."
//
// So this is written to be read, not to be clever. Every path is absolute and
// single-quoted, every operation is preceded by the reasoning that proposed it,
// and the refusals are included as comments so the script is a complete record of
// the decision rather than only of its outcome.
//
// The script mirrors what the in-app path does — `mv` into a trash batch, never
// `rm` — so running it by hand and clicking the button have the same result and the
// same recoverability.

// ScriptOptions is what the generator needs beyond the plan.
type ScriptOptions struct {
	LibraryRoot string
	DataRoot    string
	TrashDir    string
	// LinkMode is reflink | hardlink | off, and decides which command the link
	// operations use.
	LinkMode string
	// GeneratedAt stamps the header and names the trash batch directory.
	GeneratedAt time.Time
	// Actor is the username, for the header.
	Actor string
}

// WriteScript renders a plan as a POSIX shell script.
func WriteScript(w io.Writer, plan *Plan, opts ScriptOptions) error {
	if plan == nil {
		return fmt.Errorf("no plan to export")
	}
	if opts.GeneratedAt.IsZero() {
		return fmt.Errorf("script needs a generation timestamp")
	}
	if opts.LinkMode == "" {
		opts.LinkMode = "off"
	}
	batch := batchID(opts.GeneratedAt)
	batchDir := filepath.Join(opts.TrashDir, batch)

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Ambar removal script — review this before running it.\n#\n")
	fmt.Fprintf(&b, "# Generated:   %s\n", opts.GeneratedAt.UTC().Format(time.RFC3339))
	if opts.Actor != "" {
		fmt.Fprintf(&b, "# Requested by: %s\n", opts.Actor)
	}
	if plan.Reason != "" {
		fmt.Fprintf(&b, "# Reason:      %s\n", singleLine(plan.Reason))
	}
	fmt.Fprintf(&b, "# Operations:  %d (%d file(s), %s)\n",
		len(plan.Ops), plan.TotalFiles(), humanBytes(plan.TotalBytes()))
	b.WriteString("#\n")
	b.WriteString("# Nothing is deleted. Files are moved into a trash batch directory, keeping\n")
	b.WriteString("# their relative paths, so any of it can be moved back by hand.\n")
	b.WriteString("# Ambar will notice the files are gone on the next `ambar scan`.\n")
	b.WriteString("\nset -eu\n\n")

	if len(plan.Blocked) > 0 {
		b.WriteString("# ---------------------------------------------------------------------------\n")
		b.WriteString("# Refused by Ambar and therefore NOT in this script:\n")
		for _, blocked := range plan.Blocked {
			fmt.Fprintf(&b, "#   %s\n#       %s\n", blocked.Path, singleLine(blocked.Reason))
		}
		b.WriteString("# ---------------------------------------------------------------------------\n\n")
	}

	if len(plan.Transfers) > 0 {
		b.WriteString("# ---------------------------------------------------------------------------\n")
		b.WriteString("# WARNING: this selection expects curation to be transferred first:\n")
		for _, t := range plan.Transfers {
			what := t.What
			if what == "" {
				what = "tags and provenance"
			}
			fmt.Fprintf(&b, "#   pack %d -> pack %d: %s\n", t.FromPackID, t.ToPackID, singleLine(what))
		}
		b.WriteString("# A shell script cannot do that — it lives in the database. Either run the\n")
		b.WriteString("# removal from the web UI, which transfers it first, or accept losing it.\n")
		b.WriteString("# ---------------------------------------------------------------------------\n\n")
	}

	trashOps, linkOps := plan.TrashOps(), plan.LinkOps()

	if len(trashOps) > 0 {
		fmt.Fprintf(&b, "TRASH=%s\n", quote(batchDir))
		b.WriteString("mkdir -p \"$TRASH\"\n\n")
		for _, op := range trashOps {
			root := opts.LibraryRoot
			if op.Root == RootData {
				root = opts.DataRoot
			}
			src := filepath.Join(root, filepath.FromSlash(op.Path))
			destRel := filepath.Join(string(op.Root), filepath.FromSlash(op.Path))
			dest := filepath.Join(batchDir, destRel)

			b.WriteString(comment(op))
			fmt.Fprintf(&b, "mkdir -p %s\n", quote(filepath.Dir(dest)))
			// -n so a name collision never overwrites; the trash batch is fresh, so a
			// collision means something unexpected and stopping is right.
			fmt.Fprintf(&b, "mv -n -- %s %s\n\n", quote(src), quote(dest))
		}
	}

	if len(linkOps) > 0 {
		b.WriteString("# --- linking: content is shared, both paths keep working ---\n\n")
		if opts.LinkMode == "off" {
			b.WriteString("# AMBAR_DEDUPE_LINK_MODE is 'off', so these are listed as comments only:\n")
		}
		for _, op := range linkOps {
			redundant := filepath.Join(opts.LibraryRoot, filepath.FromSlash(op.Path))
			kept := filepath.Join(opts.LibraryRoot, filepath.FromSlash(op.KeepPath))
			b.WriteString(comment(op))
			if opts.LinkMode == "off" {
				fmt.Fprintf(&b, "#   %s  <-  %s\n\n", op.Path, op.KeepPath)
				continue
			}
			// cmp is the same refusal the in-app path makes by re-hashing: never replace
			// a file whose bytes have diverged from the copy being kept.
			fmt.Fprintf(&b, "if cmp -s -- %s %s; then\n", quote(redundant), quote(kept))
			switch opts.LinkMode {
			case "reflink":
				fmt.Fprintf(&b, "    cp --reflink=always --preserve=timestamps -- %s %s.ambar-link\n", quote(kept), quote(redundant))
				fmt.Fprintf(&b, "    mv -- %s.ambar-link %s\n", quote(redundant), quote(redundant))
			case "hardlink":
				fmt.Fprintf(&b, "    ln -f -- %s %s\n", quote(kept), quote(redundant))
			default:
				return fmt.Errorf("unknown link mode %q", opts.LinkMode)
			}
			fmt.Fprintf(&b, "else\n    echo 'skipped %s: content differs from the copy to keep' >&2\nfi\n\n",
				shellSafeComment(op.Path))
		}
	}

	fmt.Fprintf(&b, "echo 'done: %d operation(s), %s reclaimed'\n",
		len(plan.Ops), humanBytes(plan.TotalBytes()))
	if len(trashOps) > 0 {
		b.WriteString("echo 'moved files are in '\"$TRASH\"' — nothing was deleted'\n")
	}
	b.WriteString("echo 'run `ambar scan` so the index notices'\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// comment renders the reasoning above an operation, which is the part that makes
// the script reviewable rather than merely runnable.
func comment(op Op) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s", shellSafeComment(op.Path))
	if op.IsDir {
		fmt.Fprintf(&b, "  (directory, %d file(s))", op.Files)
	}
	fmt.Fprintf(&b, "  %s\n", humanBytes(op.Bytes))
	if op.Finding != "" {
		fmt.Fprintf(&b, "#   finding: %s\n", shellSafeComment(op.Finding))
	}
	if op.Note != "" {
		fmt.Fprintf(&b, "#   %s\n", shellSafeComment(op.Note))
	}
	return b.String()
}

// quote makes any path safe for a POSIX shell by single-quoting it and escaping
// embedded single quotes. Library paths really do contain spaces, ampersands and
// quotes — `PNG_Parts&Spriter_Animation` is a real directory name from §5.1.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellSafeComment keeps a path on one line inside a comment. A newline in a
// filename would otherwise end the comment and turn the rest into a command.
func shellSafeComment(s string) string {
	return singleLine(s)
}

func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// humanBytes matches the web UI's formatting so a script and a page agree.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	value := float64(b)
	units := []string{"KB", "MB", "GB", "TB"}
	var suffix string
	for _, u := range units {
		value /= unit
		suffix = u
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, suffix)
	}
	return fmt.Sprintf("%.0f %s", value, suffix)
}
