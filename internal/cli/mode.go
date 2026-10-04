package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Mode selects the terminal owner. GuidedMode avoids a name collision with Guided.
type Mode uint8

const (
	PlainMode Mode = iota
	GuidedMode
)

// SelectMode never opens a terminal. All three inherited descriptors must be usable.
func SelectMode(args []string, terminalAvailable, termDumb bool) (Mode, error) {
	args, plain, _, help, err := commonFlags(args)
	if err != nil {
		return PlainMode, err
	}
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	if command == "ui" && (plain || !terminalAvailable || termDumb) {
		return PlainMode, errors.New("ui needs terminal input and output; remove --plain and use TERM other than dumb")
	}
	if help {
		return PlainMode, nil
	}
	if !plain && terminalAvailable && !termDumb && (command == "" || command == "ui" || command == "controller" || command == "agent") {
		return GuidedMode, nil
	}
	return PlainMode, nil
}

// commonFlags removes global switches before the command and before positional
// input. A prompt that starts with a flag uses the standard -- separator.
func commonFlags(args []string) (rest []string, plain, verbose, help bool, err error) {
	rest = make([]string, 0, len(args))
	command := ""
	takesValue, positional := false, false
	for _, arg := range args {
		if takesValue || positional {
			rest = append(rest, arg)
			takesValue = false
			continue
		}
		if arg == "--" {
			rest = append(rest, arg)
			positional = true
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			rest = append(rest, arg)
			if command == "" {
				command = arg
				help = arg == "help"
			} else {
				positional = true
			}
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if name == "plain" || name == "verbose" {
			enabled := true
			if hasValue {
				enabled, err = strconv.ParseBool(value)
				if err != nil {
					return rest, plain, verbose, help, fmt.Errorf("invalid value %q for --%s: use true or false", value, name)
				}
			}
			if name == "plain" {
				plain = enabled
			} else {
				verbose = enabled
			}
			continue
		}
		rest = append(rest, arg)
		// flag.FlagSet treats undefined help/h as help regardless of an =value.
		if name == "help" || name == "h" {
			help = true
			continue
		}
		if command == "" {
			command = arg
			continue
		}
		if !hasValue && name != "local" && name != "controller" && name != "worker" && name != "probe" && name != "yes" {
			takesValue = true
		}
	}
	return
}
