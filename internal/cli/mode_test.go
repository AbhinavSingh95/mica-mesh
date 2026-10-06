package cli

import (
	"reflect"
	"testing"
)

func TestModeSelectionHonorsTTYPlainAndDumb(t *testing.T) {
	for _, tt := range []struct {
		args      []string
		tty, dumb bool
		want      Mode
		bad       bool
	}{
		{nil, true, false, GuidedMode, false}, {nil, false, false, PlainMode, false}, {nil, true, true, PlainMode, false},
		{[]string{"ui"}, true, false, GuidedMode, false}, {[]string{"ui"}, false, false, PlainMode, true}, {[]string{"ui"}, true, true, PlainMode, true},
		{[]string{"ui", "--plain"}, true, false, PlainMode, true}, {[]string{"--plain"}, true, false, PlainMode, false},
		{[]string{"--verbose", "agent", "--local"}, true, false, GuidedMode, false}, {[]string{"--plain", "controller"}, true, false, PlainMode, false},
		{[]string{"agent", "--plain"}, true, false, PlainMode, false}, {[]string{"controller"}, false, false, PlainMode, false},
		{[]string{"run", "--controller-address", "x", "--plain"}, true, false, PlainMode, false},
		{[]string{"setup"}, true, false, PlainMode, false}, {[]string{"agent", "--help"}, true, false, PlainMode, false},
	} {
		got, err := SelectMode(tt.args, tt.tty, tt.dumb)
		if got != tt.want || (err != nil) != tt.bad {
			t.Errorf("%v tty=%v dumb=%v got %v %v want %v error=%v", tt.args, tt.tty, tt.dumb, got, err, tt.want, tt.bad)
		}
	}
}

func TestModeCommonBooleanValuesAndArgumentBoundaries(t *testing.T) {
	for _, prefix := range []string{"--plain=", "-plain="} {
		for _, value := range []string{"true", "1", "t", "T", "TRUE", "True"} {
			mode, err := SelectMode([]string{"agent", prefix + value}, true, false)
			if err != nil || mode != PlainMode {
				t.Errorf("%s%s: %v %v", prefix, value, mode, err)
			}
		}
		for _, value := range []string{"false", "0", "f", "F", "FALSE", "False"} {
			mode, err := SelectMode([]string{"agent", prefix + value}, true, false)
			if err != nil || mode != GuidedMode {
				t.Errorf("%s%s: %v %v", prefix, value, mode, err)
			}
		}
	}
	for _, flag := range []string{"--plain=bad", "--plain=", "--verbose=bad"} {
		_, err := SelectMode([]string{"agent", flag}, true, false)
		if err == nil {
			t.Errorf("invalid boolean accepted: %s", flag)
		}
	}
	for _, args := range [][]string{{"agent", "--config", "help"}, {"agent", "--config", "--help"}, {"agent", "--config", "--plain"}, {"agent", "--", "--plain"}, {"agent", "help", "--plain"}, {"agent", "--plain=false"}} {
		mode, err := SelectMode(args, true, false)
		if err != nil || mode != GuidedMode {
			t.Errorf("value consumed %v: %v %v", args, mode, err)
		}
	}
	for _, args := range [][]string{{"agent", "--help"}, {"agent", "-h"}, {"agent", "--help=true"}, {"agent", "--help=false"}, {"help"}} {
		mode, err := SelectMode(args, true, false)
		if err != nil || mode != PlainMode {
			t.Errorf("help %v: %v %v", args, mode, err)
		}
	}
}

func TestCommonFlagsPreserveValuesAndPromptText(t *testing.T) {
	for _, args := range [][]string{{"run", "--", "--plain=true"}, {"run", "hello", "--verbose"}, {"run", "--config", "--plain=true", "hello"}, {"run", "--config=help", "hello"}} {
		rest, plain, verbose, help, err := commonFlags(args)
		if err != nil || plain || verbose || help || !reflect.DeepEqual(rest, args) {
			t.Errorf("%v => %v plain=%v verbose=%v help=%v err=%v", args, rest, plain, verbose, help, err)
		}
	}
	rest, plain, verbose, _, err := commonFlags([]string{"--plain=true", "--verbose=true", "agent", "--plain=false", "--verbose=false"})
	if err != nil || plain || verbose || !reflect.DeepEqual(rest, []string{"agent"}) {
		t.Errorf("boolean last-value semantics: %v %v %v %v", rest, plain, verbose, err)
	}
}
