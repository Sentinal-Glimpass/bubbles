package main

import (
	"strings"
	"testing"
)

// spawnOnlyTokens are the calls that only exist for spawn-granted bubbles;
// their prose must not be billed to a bubble that holds no spawn tool.
var spawnOnlyTokens = []string{"spawn(", "edit(", "delete(", "introduce(", "broadcast(", "assign_task("}

func TestCitizenPromptGatedOnCanSpawn(t *testing.T) {
	nonSpawner := citizenPromptFor(false)
	spawner := citizenPromptFor(true)

	for _, tok := range spawnOnlyTokens {
		if strings.Contains(nonSpawner, tok) {
			t.Errorf("non-spawner prompt must not contain %q, got:\n%s", tok, nonSpawner)
		}
		if !strings.Contains(spawner, tok) {
			t.Errorf("spawner prompt must contain %q", tok)
		}
	}

	// Core citizen content every bubble needs must survive the split.
	for _, tok := range []string{"send(", "inbox()", "brain folder"} {
		if !strings.Contains(nonSpawner, tok) {
			t.Errorf("non-spawner prompt missing core content %q", tok)
		}
		if !strings.Contains(spawner, tok) {
			t.Errorf("spawner prompt missing core content %q", tok)
		}
	}

	const minReduction = 500 // bytes; a regression that reunites the sections must fail this
	if reduction := len(spawner) - len(nonSpawner); reduction < minReduction {
		t.Errorf("expected non-spawner prompt to be at least %d bytes shorter than spawner prompt, got %d (spawner=%d non-spawner=%d)",
			minReduction, reduction, len(spawner), len(nonSpawner))
	}
}
