package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parsedFlags is what one command line gave. The two reserved names are kept
// apart from the input items: they select the primary config source and ask
// for the help output, and letting them into the config data would make the
// unknown-key check trip over this module's own names.
type parsedFlags struct {
	// help records that --help was given.
	help bool
	// locator is the value of --config, and hasLocator whether it was given
	// at all, which is what separates an empty locator from an absent one.
	locator    string
	hasLocator bool
	// values holds the text each input item was given, by path. A repeated
	// argument keeps the last occurrence, as a later layer overrides an
	// earlier one.
	values map[string]string
}

// parseFlags reads the command line against the manifest. The parser is
// strict: an argument no input item declared is a failure rather than
// something passed through, because there is nobody downstream to pass it to.
//
// The syntax is fixed and the implementation does not deviate from it:
// --name=value and --name value, -s value and -s=value, a boolean that may
// omit its value and is given one with an equals sign alone, no clustering of
// short names, no positional arguments, and no -- terminator. A failure of the
// syntax itself is ErrMalformedCommandLine; a name the syntax admits and no
// item declares is ErrUnknownKey.
func parseFlags(m *manifest, args []string) (*parsedFlags, error) {
	p := &parsedFlags{values: make(map[string]string)}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// The only thing a terminator does in the common convention is
			// let a positional argument that looks like an option through,
			// and positional arguments are refused here, so keeping it would
			// leave one piece of syntax able to swallow everything after it
			// without a word.
			return nil, fmt.Errorf("%w: the command line gives %q, and this program has no "+
				"argument terminator: every argument is parsed, and what follows a %q would "+
				"otherwise be swallowed silently. Drop it",
				ErrMalformedCommandLine, arg, arg)
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			if err := p.long(m, args, &i, name, value, hasValue); err != nil {
				return nil, err
			}
		case len(arg) > 1 && strings.HasPrefix(arg, "-"):
			name, value, hasValue := strings.Cut(arg[1:], "=")
			if err := p.short(m, args, &i, name, value, hasValue); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: the command line gives %q, and this program takes no "+
				"positional arguments: every value is named by the argument it belongs to",
				ErrMalformedCommandLine, arg)
		}
	}
	return p, nil
}

func (p *parsedFlags) long(m *manifest, args []string, i *int, name, value string, hasValue bool) error {
	switch name {
	case helpFlag:
		given := true
		if hasValue {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%w: --%s takes true or false, and the command line gave %q",
					ErrMalformedCommandLine, helpFlag, value)
			}
			given = parsed
		}
		p.help = given
		return nil
	case locatorFlag:
		text, err := takeValue(args, i, "--"+name, value, hasValue)
		if err != nil {
			return err
		}
		p.locator, p.hasLocator = text, true
		return nil
	}
	item, declared := m.byFlag[name]
	if !declared {
		return fmt.Errorf("%w: the command line gives --%s, which no input item declares. An "+
			"item reaches the command line only by naming itself with FlagName, and a long name "+
			"is never derived from a path",
			ErrUnknownKey, name)
	}
	if err := acceptsFlag(item, "--"+name); err != nil {
		return err
	}
	return p.record(item, args, i, "--"+name, value, hasValue)
}

func (p *parsedFlags) short(m *manifest, args []string, i *int, name, value string, hasValue bool) error {
	// The length decides before the lookup does: a short name is a single
	// character, which collection enforces on the declaring end, so a longer
	// one is the syntax being wrong rather than a name nobody declared.
	if utf8.RuneCountInString(name) > 1 {
		return fmt.Errorf("%w: the command line gives -%s, and a short name is a single "+
			"character. Short names do not cluster, so -%s is read as one name rather than as "+
			"several: write them apart, as -a -b",
			ErrMalformedCommandLine, name, name)
	}
	item, declared := m.byShort[name]
	if !declared {
		return fmt.Errorf("%w: the command line gives -%s, which no input item declares. An "+
			"item reaches the command line only by declaring OriginFlag, and its short name "+
			"comes from FlagShort",
			ErrUnknownKey, name)
	}
	if err := acceptsFlag(item, "-"+name); err != nil {
		return err
	}
	return p.record(item, args, i, "-"+name, value, hasValue)
}

// acceptsFlag refuses an argument whose item names itself on the command line
// without taking the command line as an origin. A name alone does not open the
// origin: the item is in the manifest, where its name still has to be unique
// and where the help output passes over it, but nothing reads it from here. A
// value given anyway would be an argument the help does not list and yet
// changes the configuration, which is the same refusal the primary source
// makes, told apart by its own message.
func acceptsFlag(item *manifestItem, written string) error {
	if item.origins.has(OriginFlag) {
		return nil
	}
	return fmt.Errorf("%w: the command line gives %s, which module %q declares as the name of "+
		"%q but does not accept from the command line: it reads %s. Add OriginFlag to the item, "+
		"or drop FlagName",
		ErrUnknownKey, written, item.module, item.path, originNames(item.origins))
}

// record takes the value of one argument and files it under the item's path.
// A boolean may omit its value, in which case the argument itself means true;
// giving it one is written with an equals sign, never as the next word, or
// whether that word is the value or the next argument could not be told.
func (p *parsedFlags) record(item *manifestItem, args []string, i *int, written, value string, hasValue bool) error {
	if item.typ.Kind() == reflect.Bool && !hasValue {
		p.values[item.path] = "true"
		return nil
	}
	text, err := takeValue(args, i, written, value, hasValue)
	if err != nil {
		return err
	}
	p.values[item.path] = text
	return nil
}

// takeValue resolves the value of an argument written either way.
func takeValue(args []string, i *int, written, value string, hasValue bool) (string, error) {
	if hasValue {
		return value, nil
	}
	if *i+1 >= len(args) {
		return "", fmt.Errorf("%w: the command line ends with %s, which takes a value. "+
			"Write %s=VALUE or %s VALUE", ErrMalformedCommandLine, written, written, written)
	}
	*i++
	return args[*i], nil
}

// applyFlags overlays the command line, the topmost layer. Items are walked in
// manifest order, so the result does not depend on the order the arguments
// were written in.
func (d *data) applyFlags(m *manifest, p *parsedFlags) error {
	for _, item := range m.items {
		text, given := p.values[item.path]
		if !given {
			continue
		}
		value, err := coerceText(item.path, text, item)
		if err != nil {
			return err
		}
		d.set(item, value, layerFlag)
	}
	return nil
}
