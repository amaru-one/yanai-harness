package workflow

import (
	"encoding/json"
	"testing"
)

func TestCheckPolicyRejectsUndeclaredRuntimeControlsAndOldFields(t *testing.T) {
	for _, name := range []string{"PATH", "GOPROXY", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PYTHONPATH", "OPENROUTER_API_KEY", "GIT_CONFIG_GLOBAL"} {
		if ValidCheckEnvName(name) {
			t.Fatal("accepted runtime control", name)
		}
	}
	if !ValidCheckEnvName("MAWTA_TEST_ADMIN_URL") {
		t.Fatal("rejected project-specific test input")
	}
	var check Check
	if err := json.Unmarshal([]byte(`{"id":"db","requires_postgres":true}`), &check); err == nil {
		t.Fatal("silently accepted removed requires_postgres field")
	}
}
