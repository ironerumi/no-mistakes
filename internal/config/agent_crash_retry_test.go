package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestAgentCrashRetriesConfig(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 3, 4} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(fmt.Sprintf("agent_crash_retries: %d\n", n)))
			if n < 0 || n > 3 {
				if err == nil || !strings.Contains(err.Error(), "agent_crash_retries") {
					t.Fatalf("invalid budget accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := Merge(global, &RepoConfig{}).AgentCrashRetries; got != n {
				t.Fatalf("budget=%d want=%d", got, n)
			}
		})
	}
	global, err := LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil || global.AgentCrashRetries != 0 || Merge(global, &RepoConfig{}).AgentCrashRetries != 0 {
		t.Fatalf("unconfigured retry enabled: %+v, %v", global, err)
	}
	repo, err := LoadRepoFromBytes([]byte("agent_crash_retries: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(global, repo).AgentCrashRetries != 0 {
		t.Fatal("repository enabled operator-owned crash retry budget")
	}
}

func TestAgentCrashRetryExplicitTimeouts(t *testing.T) {
	for _, key := range []string{"agent_timeout", "review_agent_timeout", "test_agent_timeout", "agent_working_timeout", "review_agent_working_timeout", "test_agent_working_timeout"} {
		t.Run(key, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(key + ": 30m\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !global.AgentTimeoutsConfigured || !Merge(global, &RepoConfig{}).AgentTimeoutsConfigured {
				t.Fatal("explicit timeout not retained through merge")
			}
		})
	}
	global, err := LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil || global.AgentTimeoutsConfigured {
		t.Fatalf("default timeouts marked explicit: %+v, %v", global, err)
	}
}
