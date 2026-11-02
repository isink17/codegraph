package heuristic

import "testing"

func TestFallbackKeepsCommonJSMapping(t *testing.T) {
	if !NewTypeScriptJavaScript().Supports("file.cjs") {
		t.Fatal("fallback lost existing .cjs support")
	}
}

func TestFallbackDoesNotClaimCollidingExtensions(t *testing.T) {
	if NewScala().Supports("build.sc") {
		t.Fatal(".sc claimed by Scala despite the SuperCollider collision")
	}
	if NewPHP().Supports("legacy.inc") || NewCAndCpp().Supports("legacy.inc") {
		t.Fatal(".inc claimed despite the PHP/C include collision")
	}
}
