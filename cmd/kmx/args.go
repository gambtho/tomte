package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/app"
)

// extractContext pulls a global `--context <name>` (or `--context=<name>`,
// and the single-dash spellings) out of the argument list wherever it
// appears.
//
// It is global rather than per-subcommand because it decides WHERE a command
// lands, and an operator who has just been shown a guard banner naming the
// wrong cluster should be able to append `--context ...` to the command they
// already typed without first learning which side of the verb it goes on.
func extractContext(argv []string) ([]string, string, error) {
	var kept []string
	value := ""
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		if name != "--context" && name != "-context" {
			kept = append(kept, arg)
			continue
		}
		if hasInline {
			value = inline
			continue
		}
		if i+1 >= len(argv) {
			return nil, "", fmt.Errorf("--context needs a context name")
		}
		i++
		value = argv[i]
	}
	if strings.TrimSpace(value) == "" && value != "" {
		return nil, "", fmt.Errorf("--context needs a context name")
	}
	return kept, value, nil
}

// joinArgs joins the remaining words of `kmx agent chat <name> ...` into one
// message, so a question does not have to be quoted.
func joinArgs(args []string) string { return strings.Join(args, " ") }

func joinSteps() string { return strings.Join(app.UpSteps, ", ") }

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("kmx "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
