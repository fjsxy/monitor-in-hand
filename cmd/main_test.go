package main

import (
	"flag"
	"testing"
)

func TestLeaderElectionFlagCompatibility(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "default remains enabled"},
		{name: "bare scaffold flag", args: []string{"--leader-elect"}},
		{name: "explicit true", args: []string{"--leader-elect=true"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := defaultCLIOptions()
			flags := flag.NewFlagSet("manager", flag.ContinueOnError)
			options.bindFlags(flags)

			if err := flags.Parse(tt.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}
			if err := options.validate(); err != nil {
				t.Fatalf("validate options: %v", err)
			}
			if !options.leaderElection {
				t.Fatal("leader election must remain enabled")
			}
		})
	}
}

func TestLeaderElectionCannotBeDisabled(t *testing.T) {
	options := defaultCLIOptions()
	flags := flag.NewFlagSet("manager", flag.ContinueOnError)
	options.bindFlags(flags)

	if err := flags.Parse([]string{"--leader-elect=false"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if err := options.validate(); err == nil {
		t.Fatal("expected disabling leader election to be rejected")
	}
}
